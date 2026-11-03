# Vendored libwebp

Encoding half of **libwebp v1.6.0**, the release tagged `v1.6.0` in
https://github.com/webmproject/libwebp, commit
`4fa21912338357f89e4fd51cf2368325b59e9bd9` (2025-07-07), fetched with
`git clone -b v1.6.0 --depth 1`.

It is compiled into the Go binary through cgo by the package above, so that a
static edge-dedup needs a C compiler at build time and no system libwebp. The
same version is what `github.com/gen2brain/webp` v0.6.4 was built from, and
both produce byte-identical files; see `../webp_compare_test.go`.

## What was taken, and why

Only the files a lossy or lossless *encode* needs. The set is the one
libwebp's own build lists in its `Makefile.am` files, minus every decoder,
demuxer, muxer, tool and test:

| directory      | files | from                                                          |
| -------------- | ----- | ------------------------------------------------------------- |
| `src/enc/`     | 23 .c | `src/enc/Makefile.am`, `libwebpencode_la_SOURCES`              |
| `src/dsp/`     | 13 .c | `src/dsp/Makefile.am`: `COMMON_SOURCES`, `ENC_SOURCES`         |
| `src/utils/`   | 13 .c | `src/utils/Makefile.am`: `COMMON_SOURCES`, `ENC_SOURCES`       |
| `sharpyuv/`    | 5 .c  | `sharpyuv/Makefile.am`, `libsharpyuv_la_SOURCES`               |
| headers        | 42    | `src/enc`, `src/dsp`, `src/utils`, `sharpyuv`, `src/webp`, and the `src/dec` headers the encoder includes |

54 C files and 42 headers, 1.4 MB on disk in total.

Not taken: `src/dec/*.c`, `src/demux`, `src/mux`, `examples`, `extras`,
`imageio`, `tests`, `webp_js`, the build system, and the SIMD sources
(`src/dsp/*_{sse2,sse41,avx2,neon,msa,mips32,mips_dsp_r2}.c`,
`sharpyuv/sharpyuv_{sse2,neon}.c`, and the headers only those use). Headers from `src/dec` are kept because
`src/dsp/dec.c`, `lossless.c` and the encoder's `*_enc.c` files include them
for shared constants and structs; no `src/dec/*.c` is compiled.

`COPYING`, `PATENTS` and `AUTHORS` are the upstream files, unmodified.
`src/webp/config.h` did not exist upstream: libwebp's build generates it from
`cmake/config.h.in`, and the copy here pins the values that build produces for
this configuration. See the comment in that file.

## How the C files reach cgo

cgo only compiles the `.c` files that sit in the package directory itself, not
those in subdirectories. So each vendored source has a one-line wrapper next to
this directory, `c_src_enc_webp_enc.c` and the like, which just `#include`s the
vendored file. One translation unit per source is also required: several
libwebp sources define file-static symbols with identical names, which would
collide if they shared a unit.

`../gen.sh` regenerates the wrappers, so a version bump is:

```sh
rm -rf libwebp/c_src_*  # or let gen.sh do it
# re-copy the upstream files into libwebp/ by the table above
cd .. && ./gen.sh && gofmt -l . && go build -tags webpnative .
```

## SIMD and threads

Both are off, matching the WASM build of the same version (CMake
`-DWEBP_ENABLE_SIMD_DEFAULT=0 -DWEBP_USE_THREAD=0`), so every x86-64 host
produces the same bytes and no runtime CPU dispatch happens:

* `libwebp/src/dsp/cpu.h` only defines `WEBP_USE_SSE2`/`SSE41`/`AVX2`/`NEON`
  when the matching `WEBP_HAVE_*` is defined, and `-DHAVE_CONFIG_H` makes that
  the only way in. `config.h` defines none of them, so no `Init*SSE*` runs and
  the plain C implementations stay in the function pointers.
* `WEBP_USE_THREAD` is left undefined in `config.h`. libwebp tests it with
  `#ifdef`, so `-DWEBP_USE_THREAD=0` would still enable threads; it must not be
  defined at all.

The C files are compiled with `-O3 -DHAVE_CONFIG_H -fno-strict-aliasing`
(see the cgo preamble in `../webp.go`).
