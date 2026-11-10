#!/bin/bash
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"
# Không có xcodebuild/xcbuild nên `--arch a --arch b` không chạy được: build từng triple rồi lipo.
BINS=()
for ARCH in arm64 x86_64; do
    swift build --package-path App -c release --triple "$ARCH-apple-macosx14.0"
    BINS+=("$(swift build --package-path App -c release --triple "$ARCH-apple-macosx14.0" --show-bin-path)/MacMess")
done
APP="build/MacMess.app"
rm -rf "$APP"
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Frameworks"
lipo -create "${BINS[@]}" -output "$APP/Contents/MacOS/MacMess"
cp App/Resources/Info.plist "$APP/Contents/Info.plist"
mkdir -p "$APP/Contents/Resources"
cp App/Resources/MacMess.icns "$APP/Contents/Resources/MacMess.icns"

# Sparkle.framework (universal) lấy từ artifact SwiftPM; ditto giữ symlink Versions/Current.
SPARKLE_FW="$(ls -d App/.build/artifacts/sparkle/Sparkle/Sparkle.xcframework/macos-*/Sparkle.framework | head -1)"
ditto "$SPARKLE_FW" "$APP/Contents/Frameworks/Sparkle.framework"
# Chỉ giữ rpath @executable_path/../Frameworks (bỏ @loader_path nếu có), làm trước khi ký.
install_name_tool -delete_rpath "@loader_path" "$APP/Contents/MacOS/MacMess" 2>/dev/null || true
if ! otool -l "$APP/Contents/MacOS/MacMess" | grep -q "@executable_path/../Frameworks"; then
    install_name_tool -add_rpath "@executable_path/../Frameworks" "$APP/Contents/MacOS/MacMess"
fi

# Ký ad-hoc tạm (release.sh ký lại bằng chứng chỉ thật).
codesign --force --deep -s - "$APP"
echo "Built $APP"
