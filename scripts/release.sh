#!/bin/bash
# Dùng: scripts/release.sh <version>   (vd 0.1.0)
# Biến môi trường: SKIP_SPARKLE_SIGN=1 bỏ qua bước sign_update (chỉ để thử dmg + codesign).
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

VERSION="${1:-}"
[[ "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo "Dùng: $0 <version> (dạng 0.1.0)" >&2; exit 1; }
PLIST="App/Resources/Info.plist"
CERT="MacMess Self-Signed"
REPO="hieund97/messenger-mac"
SKIP_SPARKLE_SIGN="${SKIP_SPARKLE_SIGN:-0}"

SPARKLE_BIN="App/.build/artifacts/sparkle/Sparkle/bin"
SIGN_UPDATE="$SPARKLE_BIN/sign_update"
GENERATE_KEYS="$SPARKLE_BIN/generate_keys"

if [[ "$SKIP_SPARKLE_SIGN" != 1 ]]; then
    # Khóa công khai trong Info.plist phải khớp khóa riêng trong Keychain, nếu không app build ra không nhận được bản cập nhật nào.
    [[ -x "$GENERATE_KEYS" ]] || { echo "Lỗi: không thấy generate_keys; chạy swift package resolve --package-path App." >&2; exit 1; }
    PLIST_KEY="$(/usr/libexec/PlistBuddy -c "Print :SUPublicEDKey" "$PLIST" 2>/dev/null || true)"
    KEYCHAIN_KEY="$("$GENERATE_KEYS" -p 2>/dev/null || true)"
    if [[ -z "$PLIST_KEY" || "$PLIST_KEY" == *REPLACE_WITH* ]]; then
        echo "Lỗi: SUPublicEDKey trong $PLIST còn là placeholder." >&2
        echo "Chạy generate_keys, dán public key (generate_keys -p) vào đó (xem scripts/create-signing-cert.md)." >&2
        exit 1
    fi
    if [[ -z "$KEYCHAIN_KEY" || "$PLIST_KEY" != "$KEYCHAIN_KEY" ]]; then
        echo "Lỗi: SUPublicEDKey trong $PLIST khác public key của khóa riêng trong Keychain." >&2
        echo "  Info.plist: $PLIST_KEY" >&2
        echo "  Keychain  : ${KEYCHAIN_KEY:-<không có khóa>}" >&2
        echo "Sửa Info.plist cho khớp \`generate_keys -p\`, hoặc tạo khóa mới (xem scripts/create-signing-cert.md)." >&2
        exit 1
    fi
fi

# Chứng chỉ tự ký. Không dùng -v: chứng chỉ chưa "Always Trust" vẫn ký được.
if security find-identity -p codesigning | grep -q "\"$CERT\""; then
    IDENTITY="$CERT"
    if ! security find-identity -v -p codesigning | grep -q "\"$CERT\""; then
        echo "CẢNH BÁO: chứng chỉ \"$CERT\" chưa được tin cậy (Always Trust); vẫn dùng để ký." >&2
    fi
elif [[ "$SKIP_SPARKLE_SIGN" == 1 ]]; then
    IDENTITY="-"
    echo "CẢNH BÁO: chưa có chứng chỉ \"$CERT\", ký ad-hoc (-) vì SKIP_SPARKLE_SIGN=1. Bản này không để phát hành." >&2
else
    echo "Lỗi: chưa có chứng chỉ \"$CERT\" trong Keychain. Bản phát hành không được ký ad-hoc" >&2
    echo "(người dùng sẽ bị hỏi lại quyền Keychain/thông báo sau mỗi lần cập nhật). Xem scripts/create-signing-cert.md." >&2
    exit 1
fi

BUILD_NUMBER="$(date +%Y%m%d%H%M)"

# make app (phụ thuộc target core) build bundle từ Info.plist nguồn; version chỉ ghi vào bản build.
make app

APP="build/MacMess.app"
# Ghi version vào Info.plist của bản build, sau đó ký lại (make-app.sh chỉ ký ad-hoc tạm nên phải ký lại bundle).
/usr/libexec/PlistBuddy -c "Set :CFBundleShortVersionString $VERSION" "$APP/Contents/Info.plist"
/usr/libexec/PlistBuddy -c "Set :CFBundleVersion $BUILD_NUMBER" "$APP/Contents/Info.plist"

FW="$APP/Contents/Frameworks/Sparkle.framework/Versions/B"
SIGN=(codesign --force --options runtime --timestamp=none -s "$IDENTITY")

# Ký từ trong ra ngoài.
for X in "$FW"/XPCServices/*.xpc "$FW/Updater.app" "$FW/Autoupdate"; do
    [[ -e "$X" ]] && "${SIGN[@]}" "$X"
done
"${SIGN[@]}" "$APP/Contents/Frameworks/Sparkle.framework"
"${SIGN[@]}" --entitlements App/Resources/MacMess.entitlements "$APP"
codesign --verify --deep --strict --verbose=2 "$APP"
codesign -dv "$APP" 2>&1 | grep -E "Authority|Signature|Identifier" || true

# DMG: có symlink /Applications, không nền ảnh.
if [[ "$SKIP_SPARKLE_SIGN" == 1 ]]; then
    DMG="build/MacMess-$VERSION-unsigned.dmg"
else
    DMG="build/MacMess-$VERSION.dmg"
fi
STAGE="$(mktemp -d)"
chmod 755 "$STAGE"
cp -R "$APP" "$STAGE/"
ln -s /Applications "$STAGE/Applications"
rm -f "$DMG"
hdiutil create -volname "MacMess" -srcfolder "$STAGE" -ov -format UDZO "$DMG" >/dev/null
rm -rf "$STAGE"
hdiutil verify "$DMG"
echo "Đã tạo $DMG"

if [[ "$SKIP_SPARKLE_SIGN" == 1 ]]; then
    echo "SKIP_SPARKLE_SIGN=1: bỏ qua sign_update, không in item appcast."
    exit 0
fi

[[ -x "$SIGN_UPDATE" ]] || { echo "Không thấy sign_update; chạy swift package resolve --package-path App." >&2; exit 1; }
# sign_update đọc khóa EdDSA riêng từ Keychain (do generate_keys tạo).
SIG_ATTRS="$("$SIGN_UPDATE" "$DMG")" || { echo "sign_update lỗi: chưa có khóa Sparkle trong Keychain? Chạy generate_keys (xem scripts/create-signing-cert.md)." >&2; exit 1; }

cat <<XML

Dán vào appcast.xml (trong <channel>), rồi upload $DMG và appcast.xml lên GitHub release v$VERSION:

        <item>
            <title>Phiên bản $VERSION</title>
            <pubDate>$(LC_ALL=C date -u "+%a, %d %b %Y %H:%M:%S +0000")</pubDate>
            <sparkle:version>$BUILD_NUMBER</sparkle:version>
            <sparkle:shortVersionString>$VERSION</sparkle:shortVersionString>
            <sparkle:minimumSystemVersion>14.0</sparkle:minimumSystemVersion>
            <enclosure url="https://github.com/$REPO/releases/download/v$VERSION/MacMess-$VERSION.dmg"
                       type="application/octet-stream"
                       $SIG_ATTRS />
        </item>
XML
