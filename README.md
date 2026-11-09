# MacMess

Ứng dụng Messenger native cho macOS (SwiftUI + lõi Go nhúng qua cgo). Yêu cầu macOS 14 trở lên, chạy trên cả Apple Silicon và Intel.

Nội dung thông báo tin nhắn hiện trên màn hình khóa theo cài đặt "Show previews" của macOS.

## Cài đặt

1. Tải `MacMess-<version>.dmg` ở trang [Releases](https://github.com/hieund97/messenger-mac/releases).
2. Mở dmg, kéo MacMess vào thư mục Applications.
3. Lần đầu mở, macOS chặn vì app không được notarize. Vào **System Settings → Privacy & Security**, kéo xuống mục Security và bấm **Open Anyway** cạnh MacMess, rồi xác nhận. Nút này chỉ hiện khoảng 1 giờ sau lần mở bị chặn; quá hạn thì mở app lại một lần nữa để nút hiện lại.
4. Các bản sau tự cập nhật qua Sparkle (menu MacMess → Kiểm tra cập nhật…).

## Cảnh báo rủi ro

- Đây không phải ứng dụng chính thức của Meta.
- Tài khoản có thể bị Meta hạn chế hoặc khóa khi dùng client không chính thức. Nên dùng tài khoản phụ.
- Phần mềm cung cấp nguyên trạng, không bảo hành.

## Dữ liệu trên máy

- Tin nhắn chỉ nằm trong bộ nhớ, không ghi ra đĩa.
- Chỉ lưu `e2ee.db` (khóa mã hóa đầu cuối) và `core.log` trong thư mục dữ liệu của app.
- Cookie phiên đăng nhập lưu trong Keychain.

## Build từ mã nguồn

- Cần Command Line Tools (`xcode-select --install`). Không cần Xcode.
- Go tự tải toolchain theo `go.mod` khi build lõi.
- `make core` build lõi Go, `make kit-test` chạy test MessengerKit, `make app` build `build/MacMess.app`.
- `scripts/release.sh <version>` đóng gói dmg đã ký (xem `scripts/create-signing-cert.md`).
- Khi thêm dependency messagix vào `core/go.mod`, chép version từ `spike/go.mod`.

## Giấy phép

AGPL-3.0, xem [LICENSE](LICENSE). Lõi dùng [mautrix-meta](https://github.com/mautrix/meta) (messagix) và [whatsmeow](https://github.com/tulir/whatsmeow), cùng giấy phép copyleft của các dự án này; mọi thay đổi trên bản phân phối phải công bố mã nguồn theo AGPL-3.0.
