#!/usr/bin/env python3
"""Exercise installed services inside a disposable systemd Docker container.

Requires the default installer paths and Python 3/openssl. It temporarily adds
an origin override to the decoder, then removes it and restarts the decoder.
Never run this on a production server.
"""

import concurrent.futures
import hashlib
import http.client
import http.server
import json
import os
from pathlib import Path
import shlex
import shutil
import socket
import ssl
import struct
import subprocess
import tempfile
import threading
import time
import zlib


def run(*args):
    return subprocess.check_output(args, stderr=subprocess.STDOUT, timeout=45).decode().strip()


def prop(unit, key):
    return run("systemctl", "show", unit, "-p", key, "--value")


def eventually(check):
    deadline = time.monotonic() + 15
    while True:
        try:
            value = check()
            if value:
                return value
        except (OSError, http.client.HTTPException, subprocess.CalledProcessError):
            pass
        if time.monotonic() >= deadline:
            raise AssertionError("service did not become ready within 15 seconds")
        time.sleep(0.1)


def local_request(path):
    conn = http.client.HTTPConnection("dedup.anytls.arpa", timeout=5)
    conn.sock = socket.socket(socket.AF_UNIX)
    conn.sock.settimeout(5)
    endpoint = "back" if path == "/health" else "admin"
    conn.sock.connect(f"/run/anytls-dedup/{endpoint}.sock")
    try:
        conn.request("GET", path)
        response = conn.getresponse()
        data = response.read()
        assert response.status == (204 if path == "/health" else 200)
        return response.getheader("X-Edge-Dedup-Epoch") if path == "/health" else json.loads(data)
    finally:
        conn.close()


def recv_exact(conn, size):
    data = bytearray()
    while len(data) < size:
        part = conn.recv(size - len(data))
        if not part:
            raise AssertionError("SOCKS stream ended early")
        data.extend(part)
    return bytes(data)


def socks_connect(proxy_port, host, port):
    conn = socket.create_connection(("127.0.0.1", proxy_port), timeout=5)
    try:
        conn.sendall(b"\x05\x01\x00")
        assert recv_exact(conn, 2) == b"\x05\x00"
        name = host.encode("ascii")
        conn.sendall(b"\x05\x01\x00\x03" + bytes([len(name)]) + name + struct.pack("!H", port))
        reply = recv_exact(conn, 4)
        assert reply[:2] == b"\x05\x00", reply
        addr_len = {1: 4, 4: 16}.get(reply[3])
        if reply[3] == 3:
            addr_len = recv_exact(conn, 1)[0]
        assert addr_len is not None
        recv_exact(conn, addr_len + 2)
        return conn
    except BaseException:
        conn.close()
        raise


def varint(value):
    result = bytearray()
    while value >= 128:
        result.append((value & 127) | 128)
        value >>= 7
    result.append(value)
    return bytes(result)


def frame(record):
    compressor = zlib.compressobj(wbits=-15)
    return b"EDG1" + compressor.compress(record + b"E") + compressor.flush()


def main():
    if not (os.environ.get("ANYTLS_SYSTEMD_TEST") == "1"
            and Path("/.dockerenv").exists()
            and Path("/proc/1/comm").read_text().strip() == "systemd"):
        raise SystemExit("requires ANYTLS_SYSTEMD_TEST=1 inside a disposable systemd Docker container")
    assert os.geteuid() == 0
    unit = "anytls-dedup.service"
    main_unit = "anytls-server.service"
    assert prop(unit, "ActiveState") == prop(main_unit, "ActiveState") == "active"
    assert prop(unit, "UnitFileState") == "enabled"
    assert prop(unit, "MemoryMax") == "524288000"
    cgroup = prop(unit, "ControlGroup")
    assert Path("/sys/fs/cgroup" + cgroup + "/memory.max").read_text().strip() == "524288000"
    assert not prop(unit, "DropInPaths"), "test requires an unmodified decoder unit"
    env = {}
    for line in Path("/etc/anytls/server.env").read_text().splitlines():
        if line.startswith("ANYTLS_") and "=" in line:
            key, value = line.split("=", 1)
            env[key] = "".join(shlex.split(value))
    server_port = int(env["ANYTLS_LISTEN"].rsplit(":", 1)[1])
    main_pid = prop(main_unit, "MainPID")
    work = Path(tempfile.mkdtemp(prefix="anytls-systemd-test-", dir="/run"))
    work.chmod(0o755)
    override = Path("/etc/systemd/system/anytls-dedup.service.d/90-test-origin.conf")
    assert not override.exists()
    release = threading.Event()
    finished = threading.Event()
    first, second = b"data: first\n\n", b"data: second\n\n"
    client = None
    servers = []

    class Origin(http.server.BaseHTTPRequestHandler):
        def log_message(self, *_args):
            pass

        def do_POST(self):
            body = self.rfile.read(int(self.headers["Content-Length"]))
            digest = hashlib.sha256(body).hexdigest().encode()
            assert all(self.headers.get(name) is None for name in
                       ("X-Edge-Dedup", "X-Edge-Dedup-Len", "X-Edge-Dedup-Epoch"))
            data = first + second if self.path == "/stream" else digest
            self.send_response(200)
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            if self.path == "/stream":
                self.wfile.write(first)
                self.wfile.flush()
                release.wait(10)
                self.wfile.write(second)
                finished.set()
            else:
                self.wfile.write(data)

    try:
        cert, key = work / "cert.pem", work / "key.pem"
        run("openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1",
            "-subj", "/CN=api.openai.com", "-addext", "subjectAltName=DNS:api.openai.com",
            "-keyout", str(key), "-out", str(cert))
        cert.chmod(0o644)
        for tls in (False, True):
            server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Origin)
            if tls:
                ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
                ctx.load_cert_chain(cert, key)
                server.socket = ctx.wrap_socket(server.socket, server_side=True)
            servers.append(server)
            threading.Thread(target=server.serve_forever, daemon=True).start()
        base_start = next(line for line in Path("/etc/systemd/system/" + unit).read_text().splitlines()
                          if line.startswith("ExecStart="))
        override.parent.mkdir(exist_ok=True)
        override.write_text("[Service]\nExecStart=\n" + base_start
                            + f" -resolve api.openai.com:443=127.0.0.1:{servers[1].server_port}"
                            + f" -upstream-ca {cert}\n")
        run("systemctl", "daemon-reload")
        run("systemctl", "restart", unit)
        epoch = eventually(lambda: local_request("/health"))
        with socket.socket() as probe:
            probe.bind(("127.0.0.1", 0))
            proxy_port = probe.getsockname()[1]
        with (work / "client.log").open("w") as log:
            client = subprocess.Popen(["/usr/local/libexec/anytls-health-probe", "-l", f"127.0.0.1:{proxy_port}",
                                       "-s", f"127.0.0.1:{server_port}", "-password-file",
                                       "/etc/anytls/server.password", "-m", "1"], stdout=log, stderr=log,
                                      env={**os.environ, "CLIENT_DEBUG_SESSION_POOL": "1"})

        def request(body, *, record=None, path="/echo", old_epoch=None, streaming=False):
            dest = "dedup.anytls.arpa" if record is not None else "127.0.0.1"
            port = 80 if record is not None else servers[0].server_port
            conn = http.client.HTTPConnection("api.openai.com" if record is not None else dest, timeout=5)
            conn.sock = socks_connect(proxy_port, dest, port)
            headers = {}
            if record is not None:
                headers = {"X-Edge-Dedup": "1", "X-Edge-Dedup-Len": str(len(body)),
                           "X-Edge-Dedup-Epoch": old_epoch or epoch}
            try:
                conn.request("POST", path, frame(record) if record is not None else body, headers)
                response = conn.getresponse()
                if streaming:
                    return conn, response
                return response.status, response.read(), response.getheader("X-Edge-Dedup-Reset")
            finally:
                if not streaming:
                    conn.close()

        body = os.urandom(48 << 10)
        digest = hashlib.sha256(body).digest()
        literal, reference = b"L" + varint(len(body)) + body, b"R" + digest

        def successful(**kwargs):
            status, data, _ = request(body, **kwargs)
            assert status == 200 and data == digest.hex().encode(), (status, data)
            return True

        eventually(successful)
        eventually(lambda: (work / "client.log").read_text().count("put session:") >= 1)
        successful(record=literal)
        eventually(lambda: (work / "client.log").read_text().count("put session:") >= 2)
        successful(record=reference)
        eventually(lambda: (work / "client.log").read_text().count("put session:") >= 3)
        # Sequential ordinary and decoder streams should reuse one TLS session.
        assert (work / "client.log").read_text().count("create session:") == 1
        with concurrent.futures.ThreadPoolExecutor(max_workers=8) as pool:
            list(pool.map(lambda _: successful(record=reference), range(8)))
        stats = local_request("/stats")
        assert stats["framed"] >= 10 and stats["frame_bytes"] < stats["original_bytes"] / 2
        assert stats["cache_bytes"] >= len(body)
        print("PASS: ordinary/decoder streams, concurrent body hashes and repeated-content savings")

        conn, response = request(body, record=reference, path="/stream", streaming=True)
        try:
            assert response.status == 200 and response.read(len(first)) == first
            assert not finished.is_set(), "response was buffered until the origin finished"
            with concurrent.futures.ThreadPoolExecutor(max_workers=1) as pool:
                restart = pool.submit(run, "systemctl", "restart", unit)
                eventually(lambda: prop(unit, "ActiveState") == "deactivating")
                release.set()
                assert response.read() == second
                restart.result()
        finally:
            release.set()
            conn.close()
        epoch = eventually(lambda: local_request("/health"))
        assert local_request("/stats")["cache_bytes"] == 0
        print("PASS: complete streamed response while decoder drains; restart clears cache")

        successful(record=literal)
        old_epoch = local_request("/health")
        restarts = int(prop(unit, "NRestarts"))
        run("systemctl", "kill", "--signal=SIGKILL", "--kill-whom=main", unit)
        eventually(lambda: int(prop(unit, "NRestarts")) > restarts and local_request("/health") != old_epoch)
        assert prop(main_unit, "MainPID") == main_pid and prop(main_unit, "ActiveState") == "active"
        assert local_request("/stats")["cache_bytes"] == 0
        status, _, reset = request(body, record=reference, old_epoch=old_epoch)
        assert status == 409 and reset == "1"
        epoch = local_request("/health")
        successful(record=literal)
        run("systemctl", "stop", unit)
        successful()
        assert prop(main_unit, "MainPID") == main_pid
        run("systemctl", "start", unit)
        eventually(lambda: local_request("/health"))
        print("PASS: crash recovery, cold-cache reset, decoder stop leaves ordinary proxy working")
        print("PASS: enforced 500 MiB cgroup limit; AnyTLS main process was unchanged")
    finally:
        release.set()
        if client is not None:
            client.terminate()
            try:
                client.wait(timeout=5)
            except subprocess.TimeoutExpired:
                client.kill()
                client.wait()
        for server in servers:
            server.shutdown()
            server.server_close()
        if override.exists():
            override.unlink()
            if not any(override.parent.iterdir()):
                override.parent.rmdir()
            run("systemctl", "daemon-reload")
            run("systemctl", "restart", unit)
        shutil.rmtree(work)


if __name__ == "__main__":
    main()
