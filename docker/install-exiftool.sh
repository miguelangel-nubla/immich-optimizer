#!/bin/sh
# Installs a pinned ExifTool together with the Perl Brotli module, so that
# every image (Debian or Alpine based) has the same exiftool with the same
# abilities. The Brotli module is what lets exiftool read the compressed
# metadata that cjxl and vips write into JPEG XL files; Immich's own exiftool
# has it too.
#
#   EXIFTOOL_VERSION=13.59 sh install-exiftool.sh
set -eu

: "${EXIFTOOL_VERSION:?set EXIFTOOL_VERSION}"
BROTLI_MODULE_VERSION="${BROTLI_MODULE_VERSION:-0.023}"

if command -v apk >/dev/null 2>&1; then
    # Alpine has no package for the module: build it, then drop the toolchain.
    apk add --no-cache perl brotli-libs curl ca-certificates
    apk add --no-cache --virtual .exiftool-build \
        perl-dev make gcc g++ musl-dev cmake brotli-dev perl-app-cpanminus
    cpanm --notest "IO::Compress::Brotli@${BROTLI_MODULE_VERSION}"
    cleanup() { apk del .exiftool-build; rm -rf /root/.cpanm; }
elif command -v apt-get >/dev/null 2>&1; then
    apt-get update
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        perl libio-compress-brotli-perl make curl ca-certificates
    cleanup() { apt-get clean; rm -rf /var/lib/apt/lists/*; }
else
    echo "install-exiftool.sh: unsupported base image (no apk or apt-get)" >&2
    exit 1
fi

work="$(mktemp -d)"
curl -fsSL -o "$work/exiftool.tar.gz" \
    "https://github.com/exiftool/exiftool/archive/refs/tags/${EXIFTOOL_VERSION}.tar.gz"
tar xzf "$work/exiftool.tar.gz" -C "$work"
cd "$work/exiftool-${EXIFTOOL_VERSION}"
perl Makefile.PL >/dev/null
make install >/dev/null
cd /
rm -rf "$work"
cleanup

# Fail the build unless the result is what the bundled profiles rely on.
test "$(exiftool -ver)" = "${EXIFTOOL_VERSION}"
perl -MIO::Uncompress::Brotli -e 1
echo "exiftool ${EXIFTOOL_VERSION} with Brotli support installed"
