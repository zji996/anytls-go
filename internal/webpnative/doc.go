// Package webpnative encodes images to WebP with libwebp v1.6.0 compiled from
// the C source vendored under libwebp/ through cgo.
//
// It exists so that the edge-dedup binary can produce WebP without a system
// libwebp: the C library is compiled in, so a static binary needs a C
// compiler at build time and nothing else. Its output is byte-identical to
// github.com/gen2brain/webp's WASM build of the same libwebp version, because
// both run the same C code with SIMD and threads switched off; see
// libwebp/src/webp/config.h.
//
// The package is only built with -tags webpnative and CGO_ENABLED=1. Its tests
// also need nodynamic: they check against gen2brain/webp, which without it
// dlopens the system libwebp next to the vendored one, and that test binary
// crashes now and then.
package webpnative
