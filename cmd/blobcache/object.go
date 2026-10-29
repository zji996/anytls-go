package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

// On-disk format, one file per object, ciphertext only:
//
//	"EB1\x00" | be32(len(meta)) | meta ciphertext | body chunk 0 | chunk 1 | ...
//
// The metadata block is the JSON below sealed with AES-256-GCM under a nonce of
// twelve 0xFF bytes; the body is 64 KiB plaintext per chunk, chunk i sealed
// under a nonce of 4 zero bytes and be64(i), so any range can be read by
// decrypting only the chunks it touches.
const (
	magic       = "EB1\x00"
	chunkSize   = 64 << 10
	tagSize     = 16
	sealedChunk = chunkSize + tagSize
	tmpDirName  = "tmp"
	maxMetaSize = 1 << 20
)

var (
	errLocked  = errors.New("cache is locked")
	errNoRoom  = errors.New("cache has no room")
	errCorrupt = errors.New("corrupt cache file")
)

// metadataNonce is the metadata block's nonce: twelve 0xFF bytes. Body chunks
// use a nonce of four zero bytes followed by the chunk index, so the two never
// collide.
var metadataNonce = [12]byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}

// meta is the metadata plaintext: what the origin said about the object,
// except Content-Disposition, which comes from the token on every request.
type meta struct {
	Size    int64  `json:"size"`
	Type    string `json:"type"`
	CSP     string `json:"csp"`
	Nosniff bool   `json:"nosniff"`
}

// objectName is the file name: hex(HMAC-SHA256(master, obj)).
func objectName(master []byte, obj string) string {
	mac := hmac.New(sha256.New, master)
	mac.Write([]byte(obj))
	return hex.EncodeToString(mac.Sum(nil))
}

// objectAEAD derives K_obj = HKDF-SHA256(K, salt=obj's 16 bytes, info="edge-blob v1").
func objectAEAD(master []byte, obj string) (cipher.AEAD, error) {
	salt, err := hex.DecodeString(obj)
	if err != nil {
		return nil, err
	}
	key, err := hkdf.Key(sha256.New, master, salt, "edge-blob v1", 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func chunkNonce(i int64) []byte {
	var nonce [12]byte
	binary.BigEndian.PutUint64(nonce[4:], uint64(i))
	return nonce[:]
}

func chunkCount(size int64) int64 { return (size + chunkSize - 1) / chunkSize }

// estimateSize is the exact file size for a complete object of this size.
func estimateSize(m meta, size int64) int64 {
	plain, err := json.Marshal(m)
	if err != nil {
		plain = nil
	}
	return int64(len(magic)) + 4 + int64(len(plain)) + tagSize + size + chunkCount(size)*tagSize
}

// objectWriter writes one object to dir/tmp and renames it into place once the
// whole body is there.
type objectWriter struct {
	c       *cache
	obj     string
	name    string
	pending string
	tmp     string
	f       *os.File
	aead    cipher.AEAD
	size    int64
	written int64
	idx     int64
	buf     []byte
	done    bool
}

// create reserves room and writes the file header. It fails with errLocked
// while the cache has no master key and errNoRoom when the object cannot be
// kept, both of which mean "pass through".
func (c *cache) create(obj string, m meta) (*objectWriter, error) {
	name, err := c.name(obj)
	if err != nil {
		return nil, err
	}
	aead, err := c.aeadFor(obj)
	if err != nil {
		return nil, err
	}
	pending, err := c.reserveRoom(estimateSize(m, m.Size))
	if err != nil {
		return nil, err
	}
	w := &objectWriter{c: c, obj: obj, name: name, pending: pending, aead: aead, size: m.Size, buf: make([]byte, 0, chunkSize)}
	plain, err := json.Marshal(m)
	if err != nil {
		c.release(pending)
		return nil, err
	}
	f, err := os.CreateTemp(c.dir+"/"+tmpDirName, "obj-")
	if err != nil {
		c.release(pending)
		return nil, err
	}
	w.f = f
	w.tmp = f.Name()
	head := make([]byte, 0, len(magic)+4+len(plain)+tagSize)
	head = append(head, magic...)
	head = binary.BigEndian.AppendUint32(head, uint32(len(plain)))
	head = aead.Seal(head, metadataNonce[:], plain, nil)
	if _, err := f.Write(head); err != nil {
		w.Abort()
		return nil, err
	}
	return w, nil
}

// Write appends plaintext; a full 64 KiB chunk is sealed and written at once.
func (w *objectWriter) Write(p []byte) error {
	for len(p) > 0 {
		n := min(len(p), chunkSize-len(w.buf))
		w.buf = append(w.buf, p[:n]...)
		p = p[n:]
		if len(w.buf) == chunkSize {
			if err := w.flush(); err != nil {
				return err
			}
		}
	}
	return nil
}

func (w *objectWriter) flush() error {
	if len(w.buf) == 0 {
		return nil
	}
	sealed := w.aead.Seal(nil, chunkNonce(w.idx), w.buf, nil)
	if _, err := w.f.Write(sealed); err != nil {
		return err
	}
	w.written += int64(len(w.buf))
	w.idx++
	w.buf = w.buf[:0]
	return nil
}

// Commit finishes the object. Only a file whose plaintext length is exactly
// the size the token and the origin agreed on is renamed into place.
func (w *objectWriter) Commit() error {
	if w.done {
		return errors.New("object writer already closed")
	}
	if err := w.flush(); err != nil {
		w.Abort()
		return err
	}
	if w.written != w.size || w.idx != chunkCount(w.size) {
		w.Abort()
		return fmt.Errorf("object ended at %d of %d bytes", w.written, w.size)
	}
	if err := w.f.Sync(); err != nil {
		w.Abort()
		return err
	}
	if err := w.f.Close(); err != nil {
		w.done = true
		os.Remove(w.tmp)
		w.c.release(w.pending)
		return err
	}
	final := w.c.dir + "/" + w.name
	if err := os.Rename(w.tmp, final); err != nil {
		w.done = true
		os.Remove(w.tmp)
		w.c.release(w.pending)
		return err
	}
	w.done = true
	size := estimateSize(meta{Size: w.size}, w.size)
	if info, err := os.Stat(final); err == nil {
		size = info.Size()
	}
	w.c.commit(w.pending, w.name, size)
	return nil
}

// Abort drops the partial file and the room reserved for it.
func (w *objectWriter) Abort() {
	if w.done {
		return
	}
	w.done = true
	if w.f != nil {
		w.f.Close()
	}
	os.Remove(w.tmp)
	w.c.release(w.pending)
}

// object is an open cache file, ready to serve ranges of plaintext.
type object struct {
	f    *os.File
	name string
	aead cipher.AEAD
	meta meta
	body int64 // file offset of chunk 0
}

func openObject(f *os.File, name string, aead cipher.AEAD) (*object, error) {
	head := make([]byte, len(magic)+4)
	if _, err := io.ReadFull(f, head); err != nil {
		return nil, errCorrupt
	}
	if string(head[:len(magic)]) != magic {
		return nil, errCorrupt
	}
	n := int64(binary.BigEndian.Uint32(head[len(magic):]))
	if n <= 0 || n > maxMetaSize {
		return nil, errCorrupt
	}
	sealed := make([]byte, n+tagSize)
	if _, err := io.ReadFull(f, sealed); err != nil {
		return nil, errCorrupt
	}
	plain, err := aead.Open(nil, metadataNonce[:], sealed, nil)
	if err != nil {
		return nil, errCorrupt
	}
	var m meta
	if err := json.Unmarshal(plain, &m); err != nil || m.Size < 0 {
		return nil, errCorrupt
	}
	o := &object{f: f, name: name, aead: aead, meta: m, body: int64(len(head)) + n + tagSize}
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	want := o.body + chunkCount(m.Size)*sealedChunk
	if rest := m.Size % chunkSize; rest != 0 {
		want -= chunkSize - rest
	}
	if info.Size() != want {
		return nil, errCorrupt
	}
	return o, nil
}

func (o *object) Size() int64 { return o.meta.Size }

func (o *object) Close() error { return o.f.Close() }

// readAt decrypts the plaintext bytes [off, off+len(dst)) into dst, one whole
// chunk at a time: a block's tag sits after its plaintext, so a range can only
// be read by decrypting the blocks that hold it.
func (o *object) readAt(off int64, dst []byte) error {
	for len(dst) > 0 {
		i := off / chunkSize
		within := off % chunkSize
		chunkLen := min(int64(chunkSize), o.meta.Size-i*chunkSize)
		n := min(int64(len(dst)), chunkLen-within)
		if n <= 0 {
			return errCorrupt
		}
		sealed := make([]byte, chunkLen+tagSize)
		if _, err := o.f.ReadAt(sealed, o.body+i*sealedChunk); err != nil {
			return errCorrupt
		}
		plain, err := o.aead.Open(sealed[:0], chunkNonce(i), sealed, nil)
		if err != nil {
			return errCorrupt
		}
		copy(dst, plain[within:within+n])
		dst = dst[n:]
		off += n
	}
	return nil
}

// copyRange streams plaintext [lo, hi] to w and reports the bytes written.
func (o *object) copyRange(w io.Writer, lo, hi int64) (int64, error) {
	buf := make([]byte, chunkSize)
	var written int64
	for off := lo; off <= hi; {
		n := min(int64(len(buf)), hi-off+1)
		if err := o.readAt(off, buf[:n]); err != nil {
			return written, err
		}
		got, err := w.Write(buf[:n])
		written += int64(got)
		if err != nil {
			return written, err
		}
		off += n
	}
	return written, nil
}

// at most hourly, so a hot object does not cost a write per hit.
const touchInterval = time.Hour
