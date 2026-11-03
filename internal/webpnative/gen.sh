#!/bin/sh
# Regenerates the wrapper .c files next to this script, one per vendored
# libwebp source file, so that cgo - which only compiles .c files that sit in
# the package directory - builds every vendored source as its own translation
# unit. One unit per file is required: many libwebp sources define file-static
# symbols with the same names, which would collide if several were #included
# into a single unit.
#
# Run after bumping the vendored tree:
#     ./gen.sh && gofmt -l . && go build -tags webpnative .
set -eu
cd "$(dirname "$0")"

tag='//go:build webpnative && cgo'

rm -f c_src_*.c c_sharpyuv_*.c

find libwebp/src/enc libwebp/src/dsp libwebp/src/utils libwebp/sharpyuv \
	-name '*.c' | LC_ALL=C sort | while read -r f; do
	wrapper="c_$(echo "$f" | sed -e 's|^libwebp/||' -e 's|/|_|g')"
	printf '%s\n\n#include "%s"\n' "$tag" "$f" >"$wrapper"
done

ls c_*.c | wc -l
