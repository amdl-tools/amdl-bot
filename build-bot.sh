#!/usr/bin/env sh
# build-bot.sh  -  Cross-compile amdl-bot for all supported platforms.
# Usage: ./build-bot.sh [version]
# Outputs binaries to ./dist/

VERSION=${1:-dev}
LDFLAGS="-s -w -X amdl/internal/version.Version=${VERSION}"
OUTDIR=dist

mkdir -p "$OUTDIR"

build() {
    GOOS=$1 GOARCH=$2 go build -ldflags="${LDFLAGS}" -o "${OUTDIR}/amdl-bot_${1}_${2}${3}" ./cmd/bot
    echo "Built: ${OUTDIR}/amdl-bot_${1}_${2}${3}"
}

build linux   amd64  ""
build linux   arm64  ""
build darwin  amd64  ""
build darwin  arm64  ""
build windows amd64  ".exe"
build windows arm64  ".exe"

# Android is not cross-compiled here: it needs cgo and the NDK's clang so that DNS
# goes through bionic (see the android entry in .github/workflows/release.yml).
# On a phone, run "go build ./cmd/bot" inside Termux instead.

echo "Done. Binaries in ./${OUTDIR}/"
