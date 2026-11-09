# Tạo chứng chỉ ký và khóa Sparkle (làm một lần)

MacMess không có Apple Developer account nên không notarize. Hai thứ cần tạo trên máy phát hành:

1. Chứng chỉ tự ký "MacMess Self-Signed" để ký app. Cùng một chứng chỉ qua mọi bản phát hành thì macOS không hỏi lại quyền Keychain và thông báo sau khi cập nhật. Không có chứng chỉ này, `release.sh` từ chối build (trừ khi `SKIP_SPARKLE_SIGN=1`, chỉ để thử, dmg tên `-unsigned`).
2. Cặp khóa EdDSA của Sparkle để ký file cập nhật.

## 1. Chứng chỉ tự ký

1. Mở **Keychain Access** (Spotlight: "Keychain Access").
2. Menu **Keychain Access → Certificate Assistant → Create a Certificate…**
3. Điền:
   - Name: `MacMess Self-Signed` (đúng từng chữ, script tìm theo tên này)
   - Identity Type: **Self Signed Root**
   - Certificate Type: **Code Signing**
   - Bật **Let me override defaults**
4. Bấm **Continue** qua các bước: Validity (đặt 3650 ngày), giữ mặc định các bước còn lại, Keychain chọn **login**, rồi **Create**.
5. Trong Keychain Access (mục **login → My Certificates**), bấm đúp chứng chỉ → mở **Trust** → **Code Signing: Always Trust** → đóng và nhập mật khẩu máy.
6. Kiểm:
   ```
   security find-identity -v -p codesigning
   ```
   Phải thấy dòng `"MacMess Self-Signed"`.
7. Sao lưu: chuột phải chứng chỉ → **Export…** (.p12, đặt mật khẩu), cất ngoài repo (vd `~/MacMess-keys/`). Mất khóa riêng thì bản sau ký bằng chứng chỉ khác và người dùng bị hỏi lại quyền.

## 2. Khóa Sparkle

1. Từ gốc repo, sau khi đã `swift package resolve --package-path App`:
   ```
   App/.build/artifacts/sparkle/Sparkle/bin/generate_keys
   ```
   Lệnh lưu khóa riêng vào Keychain (login) và in public key (chuỗi base64).
2. Dán public key vào `App/Resources/Info.plist`, thay `REPLACE_WITH_SPARKLE_PUBLIC_KEY` ở `SUPublicEDKey`. Lệnh này cũng in lại public key khi cần:
   ```
   App/.build/artifacts/sparkle/Sparkle/bin/generate_keys -p
   ```
3. Sao lưu khóa riêng ra ngoài repo:
   ```
   mkdir -p ~/MacMess-keys && generate_keys -x ~/MacMess-keys/sparkle_private.key
   ```
   (`generate_keys` ở `App/.build/artifacts/sparkle/Sparkle/bin/`.) **Không để file này trong repo**, không commit, không gửi qua chat; ai có file này ký được bản cập nhật giả. Mất khóa thì người dùng đang cài không cập nhật được nữa.
4. `release.sh` từ chối chạy khi `SUPublicEDKey` còn placeholder hoặc khác `generate_keys -p`.

## 3. Phát hành

```
scripts/release.sh 0.1.0
```

Script in đoạn `<item>`. Dán vào `appcast.xml`, rồi tạo GitHub release `v0.1.0` và upload `build/MacMess-0.1.0.dmg` cùng `appcast.xml`.
