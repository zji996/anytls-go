/* Declarations the cgo preamble of webp.go needs. Kept in a plain header so
 * that both the C glue and the Go preamble see the same prototype.
 */

#ifndef ANYTLS_WEBP_NATIVE_H_
#define ANYTLS_WEBP_NATIVE_H_

#include <stddef.h>
#include <stdint.h>

#include "src/webp/encode.h"

/* Encodes width x height non-premultiplied RGBA pixels, read from in with the
 * given row stride in bytes, as a lossy WebP file. Returns a malloc'd buffer
 * of *size bytes, which the caller must free(), or NULL with *error_code set
 * to a WebPEncodingError. */
uint8_t* anytlsWebpEncodeRGBA(const uint8_t* in, int width, int height,
                              int stride, int quality, int method,
                              size_t* size, int* error_code);

#endif /* ANYTLS_WEBP_NATIVE_H_ */
