# Thiết kế: Client Messenger native cho macOS

Ngày: 2026-11-09 · Trạng thái: chờ duyệt

## Mục đích

- Meta đã gỡ app Messenger trên macOS. Người dùng cần một app desktop để nhắn tin Messenger mà không phải mở tab trình duyệt.
- Người dùng chính là chủ dự án. Sau đó sẽ chia sẻ file `.dmg` cho người khác cài.
- UI native (SwiftUI/AppKit), **không dùng WebView** ở bất cứ chỗ nào, kể cả màn hình đăng nhập.

## Phạm vi

### Phase 1 (mục tiêu của thiết kế này)

- Đăng nhập bằng phiên sẵn có trên trình duyệt nhân Chromium (Chrome, Arc, Brave, Edge), cho chọn profile.
- Danh sách hội thoại 1-1: tên, ảnh đại diện, tin cuối, thời gian, trạng thái chưa đọc.
- Chat 1-1: đọc lịch sử, gửi/nhận text, gửi/nhận ảnh và file, kéo-thả file vào ô chat.
- Áp dụng cho cả thread **có mã hóa đầu cuối (E2EE)** lẫn **không mã hóa**.
- Thông báo macOS khi có tin mới, bấm vào thì mở đúng hội thoại. Badge số tin chưa đọc trên Dock.
- Đánh dấu đã đọc khi mở hội thoại.
- Khôi phục lịch sử E2EE bằng PIN 6 số của Messenger (secure history). Người dùng được bỏ qua bước này. Tin E2EE gửi từ app được đẩy lên bản sao lưu secure history để thiết bị khác vẫn thấy.
- Một người = một hội thoại: thread thường và thread E2EE của cùng một người gộp thành một dòng trong sidebar, lịch sử ghép theo thời gian.
- Chạy nền: đóng cửa sổ thì app vẫn ở Dock và giữ kết nối để nhận thông báo. Có tùy chọn tự mở khi đăng nhập macOS. Tự kết nối lại sau khi máy sleep/wake hoặc đổi mạng.
- Tự cập nhật bằng Sparkle.

### Phase sau (không làm ở phase 1)

- Gọi thoại/video.
- Nhóm chat.
- Đăng nhập từ Safari (cần Full Disk Access).
- Typing indicator, presence, reaction, sửa/thu hồi tin, sticker/GIF, tin thoại.

### Không làm

- Không phát hành trên App Store, không notarize (không có Apple Developer account).
- Không lưu mật khẩu Facebook, không có form nhập mật khẩu Facebook. PIN secure history không phải mật khẩu tài khoản. App không lưu PIN dạng thô, chỉ lưu khóa mà secure history cần, trong Keychain.
- Không hỗ trợ nhiều tài khoản cùng lúc. Data dir, `e2ee.db` và mục Keychain vẫn đặt theo user ID (`<data>/<c_user>/`) để đăng nhập tài khoản khác không lẫn dữ liệu.

### Anti-goal (thất bại dù chạy được)

- Thiếu tin hoặc trùng tin trong hội thoại.
- Gửi một tin thành hai tin khi mạng chập chờn.
- Tự động gửi request dồn dập làm tài khoản bị checkpoint.
- Mỗi lần cập nhật app, macOS lại hỏi quyền Keychain và quyền thông báo.

## Ràng buộc

- Meta không có API chính thức cho tin nhắn cá nhân. App dùng giao thức nội bộ của web messenger.com, giao thức này có thể đổi bất cứ lúc nào.
- Tài khoản có rủi ro bị Meta đánh dấu. Màn hình đăng nhập phải cảnh báo rõ điều này.
- **Giấy phép AGPL-3.0:** lõi dùng lại mautrix-meta (AGPL-3.0) và whatsmeow (MPL-2.0), nên app phát hành mã nguồn công khai theo AGPL-3.0.
- Không sandbox (cần đọc cookie trình duyệt và Keychain của trình duyệt).
- macOS 14 trở lên, universal binary (arm64 và x86_64).
- Máy dev có Swift 6.2 / Xcode tương ứng.

## Kiến trúc (đã chốt)

```
┌──────────────────────── App (Swift) ───────────────────────┐
│ SwiftUI UI ── AppState/ViewModels ── LocalStore (GRDB)    │
│        │                   ▲                              │
│ CookieImporter        CoreBridge (Swift, gọi C API)       │
│ Notifications · Sparkle                                    │
└────────────────────────────┬──────────────────────────────┘
                             │ C ABI: lệnh JSON vào, sự kiện JSON ra (callback)
┌────────────────────────────▼──────────────────────────────┐
│ MessengerCore (Go, build -buildmode=c-archive)            │
│  messagix (DGW/Lightspeed, bootstrap, upload)             │
│  whatsmeow + libsignal (E2EE), sqlstore cho khóa Signal   │
└───────────────────────────────────────────────────────────┘
```

### Lõi Go: `MessengerCore`

- Module Go riêng, import `go.mau.fi/mautrix-meta/pkg/messagix` và `go.mau.fi/whatsmeow`. Ghim version cùng với mautrix-meta commit `a8ffd21` (8/11/2026).
- **Không** import `pkg/connector` vì nó gắn với Matrix bridge. Logic tương đương (đăng ký device E2EE, phân loại thread, gửi theo E2EE hay Lightspeed, xác nhận OTID, refresh URL media) viết lại gọn trong lõi, tham khảo các file:
  - `pkg/connector/client.go`: khởi tạo, đăng ký device, kết nối E2EE.
  - `pkg/connector/handlematrix.go`: gửi tin, OTID, mark read.
  - `pkg/connector/handlemeta.go`, `pkg/connector/handlewhatsapp.go`: chuyển sự kiện vào thành tin nhắn.
  - `pkg/connector/backfill.go`, `pkg/connector/backfill_e2ee.go`: tải lịch sử.
  - `pkg/connector/directmedia.go`: refresh URL media.
  - `pkg/connector/login_history.go`, `history.go`, `history_upload.go`, `pkg/messagix/securehistory`: PIN, khôi phục và đẩy lịch sử E2EE.
  - `pkg/connector/history_cutover.go`, bảng `LSCreateOpenToE2EEThreadLink`: nối thread thường với thread E2EE.
- Lõi trả cho Swift một `conversation_id` ổn định cho mỗi người (theo user ID của đối phương). Mỗi tin vẫn mang `thread_id` gốc và cờ `e2ee`. Lõi tự chọn đường gửi: thread E2EE nếu đã có link, nếu chưa thì thread thường.
- Khóa Signal và device E2EE lưu bằng sqlstore của whatsmeow, file SQLite riêng trong `~/Library/Application Support/<App>/e2ee.db`.

### Hợp đồng C API (Swift ↔ Go)

| Hàm | Mô tả |
|---|---|
| `core_init(config_json, event_cb)` | Đường dẫn data dir, log level. Đăng ký callback nhận sự kiện. |
| `core_call(request_json) -> request_id` | Lệnh bất đồng bộ. Kết quả trả về qua sự kiện `response` cùng `request_id`. |
| `core_free(ptr)` | Giải phóng chuỗi Go đã cấp phát. |
| `core_shutdown()` | Đóng kết nối, flush store. |

- Lệnh: `login(cookies, platform)`, `restore_history(pin)`, `skip_history_restore`, `logout`, `reconnect`, `list_threads(cursor)`, `fetch_messages(thread, before, limit)`, `send_text(thread, text, client_id)`, `send_media(thread, file_path, mime, client_id)`, `mark_read(thread, until_ts)`, `download_media(attachment_ref, dest_path)`.
- Sự kiện: `connection_state`, `history_restore_state(needed|in_progress|done|failed, reason)`, `thread_upsert`, `message_upsert`, `message_send_result(client_id, status: sent|failed|unknown)`, `read_receipt`, `auth_required(reason)`, `error`.
- `client_id` do Swift tạo và giữ nguyên khi retry. Lõi dùng nó làm OTID/message ID để tránh gửi trùng.
- Callback chạy trên goroutine của Go. Phía Swift chuyển sự kiện về một actor rồi mới cập nhật UI.

### App Swift

| Thành phần | Trách nhiệm |
|---|---|
| `CookieImporter` | Liệt kê trình duyệt/profile đã cài. Đọc SQLite `Cookies`, lấy khóa "<Browser> Safe Storage" từ Keychain, giải mã `v10` (AES-128-CBC, PBKDF2-SHA1 với salt `saltysalt`, 1003 vòng), bỏ 32 byte hash domain ở DB schema mới. Copy file DB ra chỗ tạm trước khi đọc vì trình duyệt đang khóa file. |
| `CoreBridge` | Bọc C API thành `async` Swift, giải mã sự kiện JSON thành `Codable`. |
| `LocalStore` | GRDB/SQLite: threads, messages, attachments, contacts. Lưu bền để mở app thấy ngay dữ liệu cũ. Unique key theo message ID để không trùng. |
| `AppState` | Nguồn dữ liệu duy nhất cho UI. Hợp nhất sự kiện từ lõi vào LocalStore. |
| UI | `NavigationSplitView`: sidebar danh sách hội thoại có ô tìm kiếm, khung chat dạng bubble, ô nhập có nút đính kèm và kéo-thả, xem ảnh lớn bằng Quick Look. |
| `Notifier` | `UNUserNotificationCenter`. Không bắn thông báo khi đang xem đúng hội thoại đó, không bắn cho tin do mình gửi hoặc tin từ backfill. |
| `Updater` | Sparkle 2, ký EdDSA, `appcast.xml` trên GitHub Releases. |
| `Lifecycle` | `applicationShouldTerminateAfterLastWindowClosed = false`, mở lại cửa sổ khi bấm icon Dock. Login item qua `SMAppService`. Nghe `NSWorkspace` sleep/wake và `NWPathMonitor` để gọi `reconnect`. |

### Đăng nhập và phiên

1. Lần đầu mở app: màn hình cảnh báo rủi ro, chọn trình duyệt và profile (mặc định là cái có cookie `c_user` còn hạn).
2. Đọc cookie của `.messenger.com`, nếu không có thì `.facebook.com`. Cookie bắt buộc: `c_user`, `xs`, `datr`.
3. Gửi cookie cho lõi → lõi tải inbox, đăng ký device E2EE nếu chưa có → kết nối.
4. Nếu lõi báo `history_restore_state=needed`: màn hình nhập PIN 6 số, có nút "Bỏ qua". Sai PIN thì báo lỗi và cho nhập lại. Bỏ qua thì vào Settings để nhập sau.
5. Cookie lưu trong Keychain của app. Khi lõi báo `auth_required`, app hiện banner "Mở messenger.com trên trình duyệt rồi bấm Đăng nhập lại", sau đó import lại cookie.
6. Checkpoint/consent: báo người dùng xử lý trên web, app không tự xử lý.

### Ký và phát hành

- Ký bằng chứng chỉ tự ký cố định (tạo một lần trong Keychain Access) để quyền Keychain và thông báo giữ được qua các bản cập nhật.
- Đóng `.dmg`, README hướng dẫn mở lần đầu qua System Settings → Privacy & Security → Open Anyway.
- Go build riêng arm64 và amd64, gộp bằng `lipo`, link vào target Xcode.

## Rủi ro và cách kiểm sớm

| Rủi ro | Cách giảm |
|---|---|
| Lõi Go không chạy ổn khi nhúng (cgo, callback, goroutine) | Bước 1 là spike: Go c-archive + app Swift tối thiểu, đăng nhập bằng cookie thật, in danh sách thread và nhận một tin E2EE. Spike lỗi thì dừng để quyết lại. |
| Khôi phục secure history thất bại (PIN sai, tài khoản chưa bật sao lưu) | Vẫn dùng được app, chỉ thấy tin E2EE từ lúc đăng ký device. Hội thoại hiện ghi chú "Chưa khôi phục lịch sử" kèm nút nhập PIN. |
| Gộp thread thường và E2EE gây trùng/hổng tin ở chỗ nối | Spike kiểm trên một hội thoại thật đã chuyển E2EE. Dedupe theo message ID và OTID. |
| Meta đổi giao thức | Lõi ghim mautrix-meta, khi hỏng thì cập nhật dependency rồi phát bản mới qua Sparkle. |
| Gửi trùng/mất tin | `client_id` cố định khi retry, trạng thái `unknown` hiển thị rõ, không tự retry. |
| Rate limit/checkpoint | Backfill có độ trễ giữa các batch, chỉ tải lịch sử khi người dùng cuộn lên. |

## Phần để builder tự quyết

- Tên app, icon (không dùng tên/logo Messenger hay Meta).
- Cấu trúc thư mục, cách build Go trong Xcode (Run Script hoặc Makefile).
- Chi tiết schema GRDB, cách phân trang UI.
- Mức log, vị trí file log.
- Bố cục và style giao diện chi tiết, miễn là native và giống cảm giác app chat macOS.

## Các lựa chọn do Claude đề xuất (người dùng chưa trực tiếp quyết)

- macOS 14 trở lên.
- GRDB cho LocalStore.
- Lưu cookie trong Keychain của app.
- Ký bằng chứng chỉ tự ký thay vì ad-hoc.
- Appcast đặt trên GitHub Releases.
- Gộp thread thường và E2EE của cùng một người thành một hội thoại.
- Một tài khoản, nhưng dữ liệu đặt theo user ID.
- Không lưu PIN thô.
- Chạy nền trên Dock (người dùng chọn), login item để tùy chọn, mặc định tắt (Claude chọn).
