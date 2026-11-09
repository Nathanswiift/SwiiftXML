#!/usr/bin/env bash
# Builds the SwiiftXML release files into dist/:
#   dist/SwiiftXML.exe          Windows (64-bit Intel/AMD)
#   dist/SwiiftXML-linux        Linux (64-bit Intel/AMD)
#   dist/SwiiftXML-linux-arm64  Linux on ARM (e.g. Chromebooks, Raspberry Pi)
# Requires Go 1.27 or newer (https://go.dev/dl/). No other dependencies.
set -euo pipefail
cd "$(dirname "$0")"

# Use the Go from go.dev in /usr/local/go when the one on the PATH is older.
go_minor() { "$1" env GOVERSION 2>/dev/null | sed -E 's/^go1\.([0-9]+).*/\1/'; }
if ! command -v go >/dev/null || [ "$(go_minor go)" -lt 27 ] 2>/dev/null; then
  if [ -x /usr/local/go/bin/go ] && [ "$(go_minor /usr/local/go/bin/go)" -ge 27 ]; then
    export PATH="/usr/local/go/bin:$PATH"
  else
    echo "SwiiftXML needs Go 1.27 or newer. Get it from https://go.dev/dl/" >&2
    exit 1
  fi
fi

VERSION="${VERSION:-1.0.0}"
out="$(mktemp -d)"
trap 'rm -rf "$out"' EXIT

build() { # GOOS GOARCH output-name
  echo "  dist/$3"
  CGO_ENABLED=0 GOOS="$1" GOARCH="$2" \
    go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o "$out/$3" .
}

echo "Building SwiiftXML $VERSION with $(go version)"
build windows amd64 SwiiftXML.exe
build linux   amd64 SwiiftXML-linux
build linux   arm64 SwiiftXML-linux-arm64
(cd "$out" && sha256sum SwiiftXML.exe SwiiftXML-linux SwiiftXML-linux-arm64 > SHA256SUMS.txt)

# Replace dist/ only now, so a failed build never removes working files.
rm -rf dist
mkdir dist
mv "$out"/* dist/
echo "Done. The files are in dist/"
