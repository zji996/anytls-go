/* Config header for the vendored libwebp v1.6.0 build.
 *
 * libwebp's CMake build generates this file from cmake/config.h.in and then
 * compiles with -DHAVE_CONFIG_H. The values below are the ones that build
 * would produce for a CGO build, with SIMD and threads switched off. They are
 * written out here so that no configure step is needed: any value not
 * mentioned is left undefined, exactly as an empty #cmakedefine would leave
 * it.
 *
 * Everything that could make the encoder depend on the machine is off:
 *   - WEBP_HAVE_SSE2 / WEBP_HAVE_SSE41 / WEBP_HAVE_AVX2 undefined: no
 *     IntrinsicInit, so the function pointers stay on the C code (see
 *     src/dsp/cpu.h, where HAVE_CONFIG_H makes a defined WEBP_HAVE_* the only
 *     way in).
 *   - WEBP_HAVE_NEON undefined: same for Arm.
 *   - WEBP_USE_THREAD undefined: no worker threads in src/utils/thread_utils.c
 *     and no pthread dependency.
 * WEBP_NEAR_LOSSLESS stays as the release build has it so that the lossless
 * path matches the WASM build byte for byte.
 *
 * Do not add macros here without checking that every host still encodes to
 * the same bytes.
 */

#ifndef WEBP_WEBP_CONFIG_H_
#define WEBP_WEBP_CONFIG_H_

/* Set to 1 if __builtin_bswap16 is available */
#define HAVE_BUILTIN_BSWAP16 1

/* Set to 1 if __builtin_bswap32 is available */
#define HAVE_BUILTIN_BSWAP32 1

/* Set to 1 if __builtin_bswap64 is available */
#define HAVE_BUILTIN_BSWAP64 1

/* Define to 1 if you have the <unistd.h> header file. */
#define HAVE_UNISTD_H 1

/* Enable near lossless encoding */
#define WEBP_NEAR_LOSSLESS 1

#endif /* WEBP_WEBP_CONFIG_H_ */
