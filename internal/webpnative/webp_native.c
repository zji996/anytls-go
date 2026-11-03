/* Encoding entry point for the vendored libwebp, mirroring what
 * github.com/gen2brain/webp's lib/webp.c encode() does for the lossy RGBA
 * case, so that both builds produce the same bytes.
 *
 * Build tags keep this file out of builds that do not ask for the native
 * codec; cgo only compiles the .c files of a package when the package is
 * built with the cgo tag, and the tag below spells that out.
 */

//go:build webpnative && cgo

#include <stdlib.h>

#include "webp_native.h"

uint8_t* anytlsWebpEncodeRGBA(const uint8_t* in, int width, int height,
                              int stride, int quality, int method, size_t* size,
                              int* error_code) {
  WebPConfig config;
  WebPPicture picture;
  WebPMemoryWriter writer;
  uint8_t* out = NULL;

  *size = 0;
  *error_code = VP8_ENC_OK;

  if (!WebPConfigInit(&config)) {
    *error_code = VP8_ENC_ERROR_INVALID_CONFIGURATION;
    return NULL;
  }
  config.quality = (float)quality;
  config.method = method;
  config.lossless = 0;
  config.exact = 0;

  if (!WebPPictureInit(&picture)) {
    *error_code = VP8_ENC_ERROR_INVALID_CONFIGURATION;
    return NULL;
  }
  picture.width = width;
  picture.height = height;
  picture.use_argb = 1;

  if (!WebPPictureImportRGBA(&picture, in, stride)) {
    *error_code = picture.error_code;
    WebPPictureFree(&picture);
    return NULL;
  }

  WebPMemoryWriterInit(&writer);
  picture.writer = WebPMemoryWrite;
  picture.custom_ptr = &writer;

  if (!WebPEncode(&config, &picture)) {
    *error_code = picture.error_code;
    WebPPictureFree(&picture);
    WebPMemoryWriterClear(&writer);
    return NULL;
  }

  *size = writer.size;
  out = writer.mem;

  WebPPictureFree(&picture);
  /* Keep WebPMemoryWriterClear() from freeing the buffer: it now belongs to
   * the caller, who releases it with free(). */
  writer.mem = NULL;
  WebPMemoryWriterClear(&writer);

  return out;
}
