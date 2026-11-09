#!/bin/bash
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"
# Không có xcodebuild/xcbuild nên `--arch a --arch b` không chạy được: build từng triple rồi lipo.
BINS=()
for ARCH in arm64 x86_64; do
    swift build --package-path App -c release --triple "$ARCH-apple-macosx14.0"
    BINS+=("$(swift build --package-path App -c release --triple "$ARCH-apple-macosx14.0" --show-bin-path)/MacChat")
done
APP="build/MacChat.app"
rm -rf "$APP"
mkdir -p "$APP/Contents/MacOS"
lipo -create "${BINS[@]}" -output "$APP/Contents/MacOS/MacChat"
cp App/Resources/Info.plist "$APP/Contents/Info.plist"
codesign --force -s - "$APP"
echo "Built $APP"
