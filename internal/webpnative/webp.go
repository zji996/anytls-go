//go:build webpnative && cgo

package webpnative

/*
#cgo CFLAGS: -O3 -DHAVE_CONFIG_H -fno-strict-aliasing
#cgo CFLAGS: -I${SRCDIR}/libwebp
#cgo LDFLAGS: -lm

// -DHAVE_CONFIG_H picks up libwebp/src/webp/config.h, which leaves every
// WEBP_HAVE_{SSE2,SSE41,AVX2,NEON,MIPS*,MSA} undefined, so src/dsp/cpu.h never
// defines WEBP_USE_* and the plain C code stays in charge; it also leaves
// WEBP_USE_THREAD undefined, which is what thread_utils.c and cpu.h test with
// #ifdef (defining it to 0 would turn threads on).
//
// -fno-strict-aliasing matches the CMake and autotools builds; libwebp does
// type-pun in a few places.
#include <stdlib.h>

#include "webp_native.h"
*/
import "C"

import (
	"errors"
	"fmt"
	"unsafe"
)

// maxDimension is libwebp's WEBP_MAX_DIMENSION: the largest width or height a
// WebP file can store.
const maxDimension = 16383

// ErrEncode stands for a failure libwebp reported without a more specific
// reason.
var ErrEncode = errors.New("webp: encode failed")

// encodingError names explain libwebp's WebPEncodingError, so that an error
// says what libwebp complained about instead of just "failed".
var encodingError = map[int]error{
	int(C.VP8_ENC_ERROR_OUT_OF_MEMORY):           errors.New("webp: out of memory"),
	int(C.VP8_ENC_ERROR_BITSTREAM_OUT_OF_MEMORY): errors.New("webp: bitstream out of memory"),
	int(C.VP8_ENC_ERROR_NULL_PARAMETER):          errors.New("webp: nil parameter"),
	int(C.VP8_ENC_ERROR_INVALID_CONFIGURATION):   errors.New("webp: invalid configuration"),
	int(C.VP8_ENC_ERROR_BAD_DIMENSION):           errors.New("webp: bad dimension"),
	int(C.VP8_ENC_ERROR_PARTITION0_OVERFLOW):     errors.New("webp: partition 0 overflow"),
	int(C.VP8_ENC_ERROR_PARTITION_OVERFLOW):      errors.New("webp: partition overflow"),
	int(C.VP8_ENC_ERROR_BAD_WRITE):               errors.New("webp: bad write"),
	int(C.VP8_ENC_ERROR_FILE_TOO_BIG):            errors.New("webp: file too big"),
	int(C.VP8_ENC_ERROR_USER_ABORT):              errors.New("webp: user abort"),
}

// Encode compresses width x height RGBA pixels, the first of each row at
// pix[y*stride], into a lossy WebP file. The pixels are non-premultiplied, as
// libwebp's WebPPictureImportRGBA expects; quality is 0..100 and method 0..6
// trades speed for size.
//
// The result is a copy in Go memory, so the caller may keep it and reuse pix.
// Encode keeps no state and is safe for concurrent use.
func Encode(pix []byte, width, height, stride, quality, method int) ([]byte, error) {
	if width <= 0 || height <= 0 {
		return nil, fmt.Errorf("webp: bad size %dx%d", width, height)
	}
	if width > maxDimension || height > maxDimension {
		return nil, fmt.Errorf("webp: size %dx%d over %d", width, height, maxDimension)
	}
	if stride < width*4 {
		return nil, fmt.Errorf("webp: stride %d shorter than %d", stride, width*4)
	}
	// The last row needs no padding, so the buffer may stop at the last pixel.
	if int64(stride)*int64(height-1)+int64(width)*4 > int64(len(pix)) {
		return nil, fmt.Errorf("webp: %d bytes are too few for %dx%d at stride %d",
			len(pix), width, height, stride)
	}
	if quality < 0 || quality > 100 {
		return nil, fmt.Errorf("webp: bad quality %d", quality)
	}
	if method < 0 || method > 6 {
		return nil, fmt.Errorf("webp: bad method %d", method)
	}

	var size C.size_t
	var code C.int
	out := C.anytlsWebpEncodeRGBA(
		(*C.uint8_t)(unsafe.Pointer(&pix[0])),
		C.int(width), C.int(height), C.int(stride),
		C.int(quality), C.int(method), &size, &code)
	if out == nil {
		if err, ok := encodingError[int(code)]; ok {
			return nil, err
		}
		return nil, ErrEncode
	}
	defer C.free(unsafe.Pointer(out))
	if size == 0 {
		return nil, ErrEncode
	}
	// Copy out of libwebp's malloc'd buffer before freeing it.
	result := make([]byte, int(size))
	copy(result, unsafe.Slice((*byte)(unsafe.Pointer(out)), len(result)))
	return result, nil
}
