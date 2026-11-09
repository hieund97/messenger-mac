# MacChat Phase 1 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** App macOS native (SwiftUI) để nhắn tin Messenger 1-1, gồm text, ảnh, file, thông báo, có hỗ trợ E2EE và khôi phục lịch sử bằng PIN.

**Architecture:** Lõi Go `MessengerCore` bọc `messagix` + `whatsmeow`, build `-buildmode=c-archive` thành `MessengerCore.xcframework` (arm64 + x86_64). Swift gọi qua C API: lệnh JSON đi vào, sự kiện JSON trả ra qua callback. Logic Swift nằm trong SwiftPM package `MessengerKit` để chạy test bằng `make kit-test`. App được build bằng SwiftPM rồi đóng thành `.app` bằng script, không cần Xcode.

**Tech Stack:** Go ≥ 1.26, `go.mau.fi/mautrix-meta` @ `a8ffd21`, `go.mau.fi/whatsmeow`, Swift 6.2, SwiftUI, macOS 14+, GRDB 7, Sparkle 2, `create-dmg`.

**Spec:** `docs/superpowers/specs/2026-11-09-messenger-mac-design.md`

## Global Constraints

- Không dùng WebView (`WKWebView`, `WebView`, `SFSafariViewController`) ở bất kỳ đâu.
- macOS 14+, universal binary arm64 + x86_64. Không sandbox.
- Giấy phép AGPL-3.0 cho toàn repo.
- Tên app tạm là `MacChat`, bundle id `dev.hieund.macchat`. Không dùng tên hay logo Messenger/Meta.
- Cookie bắt buộc: `c_user`, `xs`, `datr`. Thứ tự ưu tiên domain: `.messenger.com` rồi đến `.facebook.com`.
- Dữ liệu đặt trong `~/Library/Application Support/MacChat/<c_user>/`, gồm `e2ee.db` (whatsmeow sqlstore) và `app.db` (GRDB).
- Không lưu mật khẩu Facebook và PIN dạng thô. Bí mật chỉ nằm trong Keychain của app, service `dev.hieund.macchat`.
- `client_id` do Swift tạo, giữ nguyên khi retry. Lõi dùng nó làm OTID/message ID. Không tự gửi lại khi trạng thái là `unknown`.
- Backfill chỉ chạy khi người dùng cuộn lên. Mỗi batch cách nhau ≥ 1 giây.
- Commit message không có dòng Co-Authored-By Claude.
- Máy dev chỉ có Command Line Tools, không có Xcode. Mọi bước build phải chạy được bằng `go`, `swift build` và script.
- Không có XCTest. Test Swift viết bằng swift-testing (`import Testing`, `@Test`, `#expect`) và chạy bằng `make kit-test [FILTER=...]`. Không chạy `swift test` trần (thiếu flag framework của CLT). Không đưa flag test vào `Package.swift`, vì như vậy test chạy 0 case mà vẫn báo pass.

## Phân công agent

Quy ước theo `~/.claude/CLAUDE.md`. Gọi agent bằng `~/.claude/bin/ext-agent <gpt|gpt-heavy> <investigate|execute> <workdir>`. Sau mỗi task: `gpt investigate` chạy test và đọc diff, sau đó `opus` review diff (chỉ đưa mục tiêu và diff, không đưa kết luận). Session chính tự kiểm `git diff` rồi chốt.

| Task | Nội dung | Thực thi | Song song với |
|---|---|---|---|
| 0 | Spike lõi Go với tài khoản thật (cổng quyết định) | `gpt-heavy` execute, user chạy với cookie thật | |
| 1 | Khung repo + build xcframework + package Swift | `gpt` execute | |
| 2 | Giao thức lệnh/sự kiện + C API + backend giả | `gpt-heavy` execute | 7 |
| 3 | Lõi: phiên đăng nhập, reconnect, lỗi auth | `gpt-heavy` execute | 7, 8 |
| 4 | Lõi: E2EE device + gộp hội thoại + sự kiện tin | `gpt-heavy` execute | 8 |
| 5 | Lõi: gửi text/media, mark read, tải media | `gpt-heavy` execute | 9 |
| 6 | Lõi: lịch sử + PIN secure history + upload backup | `gpt-heavy` execute | 9 |
| 7 | Swift `CookieImporter` | `gpt` execute | 2, 3 |
| 8 | Swift `LocalStore` + `CoreBridge` + `AppState` | `gpt` execute | 3, 4 |
| 9 | UI SwiftUI | `gpt` execute | 5, 6 |
| 10 | `Notifier` + `Lifecycle` | `gpt` execute | |
| 11 | Đóng gói: ký, `.dmg`, Sparkle, README | `gpt` execute | |

Nếu `gpt-heavy` trả exit 75 thì chuyển cho agent `opus`. Nếu `gpt` hết quota thì chuyển cho `sonnet`.

## Review Focus

1. **Mất mạng đúng lúc gửi tin:** tin hiện trạng thái "Không chắc đã gửi", không tự gửi lại. Người dùng bấm "Gửi lại" thì dùng lại cùng `client_id`. Test ở Task 5 và Task 8.
2. **Cookie hết hạn giữa phiên** (Meta lỗi `1357001` hoặc DGW close unauthorized): phát đúng một sự kiện `auth_required`, không reconnect vòng lặp, UI hiện banner. Test ở Task 3.
3. **Trình duyệt đang mở khóa file Cookies, hoặc người dùng bấm Deny ở hộp thoại Keychain:** báo lỗi rõ ràng, không crash. Test ở Task 7.
4. **File quá 25 MB hoặc file không đọc được:** từ chối trước khi upload, báo lỗi ngay trên bubble tin. Test ở Task 5.
5. **Máy wake sau sleep nhận một loạt tin cũ:** không bắn thông báo cho tin có timestamp trước thời điểm kết nối lại, trừ tin chưa đọc mới hơn tin cuối đã biết. Test ở Task 10.

---

### Task 0: Spike lõi Go với tài khoản thật (cổng quyết định)

Mục đích: chứng minh `messagix` + `whatsmeow` chạy được khi nhúng qua cgo, đăng nhập bằng cookie, đọc được tin E2EE. Code spike là code bỏ đi, nằm trong `spike/` và không có test.

**Files:**
- Create: `spike/go.mod`, `spike/main.go` (CLI Go), `spike/capi.go` (`//export`), `spike/swift-harness/Package.swift`, `spike/swift-harness/Sources/Harness/main.swift`

- [ ] **Step 1:** Không nâng Go hệ thống (đang 1.22.3, các project khác còn dùng). Khai báo `go 1.26` trong `go.mod` để `GOTOOLCHAIN=auto` tự tải toolchain. Kiểm bằng `go version` trong thư mục spike. Tạo module `spike`, require `go.mau.fi/mautrix-meta` tại commit `a8ffd21` và các version trong `go.mod` của commit đó.
- [ ] **Step 2:** CLI `go run . --cookies cookies.json --platform messenger` làm các việc: tải inbox, in số thread, đăng ký device E2EE vào `spike-e2ee.db` theo `pkg/connector/client.go`, kết nối DGW và E2EE, in mọi tin đến (thread id, cờ e2ee, text) trong 5 phút. `cookies.json` do user tự xuất, nằm trong `.gitignore`.
- [ ] **Step 3:** User chạy với tài khoản phụ, nhờ người khác gửi 1 tin vào chat 1-1 đã E2EE và 1 tin vào chat chưa E2EE (nếu có). Kỳ vọng: in ra cả hai tin.
- [ ] **Step 4:** Build `go build -buildmode=c-archive` với một hàm `spike_start(cookies_json, cb)` gọi callback cho mỗi tin. Swift harness link file `.a` và in tin nhận qua callback. Kỳ vọng: giống Step 3 và không crash sau 5 phút.
- [ ] **Step 5:** Ghi kết quả vào `spike/RESULT.md`: chạy được hay không, kích thước `.a`, lỗi gặp phải, tin E2EE có đọc được không. Nếu Step 3 hoặc 4 thất bại thì **dừng toàn bộ plan** và báo user.

### Task 1: Khung repo và build

**Files:**
- Create: `LICENSE` (AGPL-3.0), `.gitignore`, `README.md` (1 đoạn), `Makefile`
- Create: `core/go.mod` (module `macchat/core`), `core/capi/capi.go`, `core/capi/core.h`
- Create: `MessengerKit/Package.swift`, `MessengerKit/Sources/CoreBridge/CoreBridge.swift`, `MessengerKit/Tests/CoreBridgeTests/SmokeTests.swift`
- Create: `App/Package.swift`, `App/Sources/MacChat/MacChatApp.swift`, `App/Resources/Info.plist`, `scripts/make-app.sh`

**Interfaces:**
- Produces: target `make core` → `core/build/MessengerCore.xcframework` (lipo arm64 + amd64, header `core.h`). SwiftPM `binaryTarget(name: "MessengerCore", path: "../core/build/MessengerCore.xcframework")`. Target `make app` → `build/MacChat.app`.
- `core.h` khai báo đúng 4 hàm sau (Task 2 sẽ cài):
  ```c
  typedef void (*core_event_cb)(const char *event_json);
  int32_t core_init(const char *config_json, core_event_cb cb);   // 0 = OK
  int64_t core_call(const char *request_json);                    // trả request_id
  void    core_free(char *ptr);
  void    core_shutdown(void);
  ```

- [ ] **Step 1:** `git init`, tạo các file trên. `capi.go` tạm cài 4 hàm rỗng: `core_init` trả 0, `core_call` trả 1.
- [ ] **Step 2:** Viết test `SmokeTests.testInitReturnsZero`: `#expect(core_init("{}", nil) == 0)`.
- [ ] **Step 3:** Chạy `make core kit-test`. Kỳ vọng: PASS.
- [ ] **Step 4:** `make app`. Kỳ vọng: `build/MacChat.app` mở được, hiện một cửa sổ trống. Kiểm tra bằng `lipo -archs build/MacChat.app/Contents/MacOS/MacChat`, kết quả phải là `x86_64 arm64`.
- [ ] **Step 5:** Commit `chore: khởi tạo repo MacChat với build lõi Go universal và app SwiftPM`.

### Task 2: Giao thức lệnh/sự kiện, C API và backend giả

**Files:**
- Create: `core/engine/protocol.go`, `core/engine/engine.go`, `core/engine/fake_backend.go`, `core/engine/engine_test.go`
- Modify: `core/capi/capi.go`
- Create: `MessengerKit/Sources/CoreBridge/Protocol.swift`, `MessengerKit/Tests/CoreBridgeTests/BridgeRoundTripTests.swift`

**Interfaces:**
- Request JSON: `{"request_id": Int64, "cmd": String, "args": Object}`. `core_call` gán `request_id` rồi trả về.
- Event JSON: `{"type": String, "request_id": Int64?, "data": Object}`.
- Cmd: `login{cookies:[{name,value,domain,expires}], platform:"messenger"|"facebook"}`, `restore_history{pin}`, `skip_history_restore{}`, `logout{}`, `reconnect{}`, `list_conversations{cursor?}`, `fetch_messages{conversation_id, before_ts?, limit}`, `send_text{conversation_id, text, client_id}`, `send_media{conversation_id, file_path, mime, client_id}`, `mark_read{conversation_id, until_ts}`, `download_media{attachment_ref, dest_path}`.
- Event type: `response{ok, error?}`, `connection_state{state:"connecting"|"connected"|"disconnected"|"auth_required", reason?}`, `history_restore_state{state:"needed"|"in_progress"|"done"|"failed", reason?}`, `conversation_upsert{Conversation}`, `message_upsert{Message}`, `message_send_result{client_id, status:"sent"|"failed"|"unknown", message_id?, error?}`, `read_receipt{conversation_id, reader_id, until_ts}`, `error{code, message}`.
- `Conversation{id, peer_id, name, avatar_url?, last_message_preview?, last_activity_ts, unread_count, history_restored:Bool}`.
- `Message{id, conversation_id, thread_id, e2ee:Bool, sender_id, is_outgoing, ts, text?, client_id?, attachments:[Attachment], reply_to_id?}`.
- `Attachment{ref, kind:"image"|"file"|"video"|"audio", mime, filename?, size?, width?, height?, preview_url?}`.
- Interface Go `Backend` có một method cho mỗi cmd. `engine.New(cfg Config, emit func(Event)) *Engine`. `cfg.Backend == "fake"` thì dùng `FakeBackend`, backend này phát dữ liệu cố định (2 hội thoại, 3 tin) và echo mọi lệnh gửi.
- Swift: `public actor CoreBridge { init(config: CoreConfig); func call(_ cmd: CoreCommand) async throws -> CoreResponse; var events: AsyncStream<CoreEvent> }`. Struct Swift dùng `Codable` khớp 1-1 với các struct trên, JSON dùng snake_case.

- [ ] **Step 1:** Viết test Go: `TestUnknownCmdReturnsError` (response `ok=false`, `error.code="unknown_cmd"`), `TestRequestIDEchoed`, `TestFakeLoginEmitsConnectedThenConversations` (thứ tự: `connection_state=connected`, rồi 2 `conversation_upsert`), `TestMalformedJSONDoesNotPanic`.
- [ ] **Step 2:** `cd core && go test ./engine/...`. Kỳ vọng: FAIL.
- [ ] **Step 3:** Cài `protocol.go`, `engine.go` (dispatch chạy trên goroutine, mỗi lệnh một goroutine, `emit` an toàn khi gọi đồng thời), `fake_backend.go`. `capi.go` giữ callback C, chuyển event thành `C.CString` rồi gọi callback. Phía Swift copy chuỗi rồi gọi `core_free`.
- [ ] **Step 4:** `go test ./engine/... -race`. Kỳ vọng: PASS.
- [ ] **Step 5:** Viết test Swift `BridgeRoundTripTests.testFakeLoginFlow`: init với `{"backend":"fake"}`, gọi `login`, đợi `events` nhận `connected` và 2 `conversationUpsert` (timeout 2 giây).
- [ ] **Step 6:** `make core kit-test`. Kỳ vọng: PASS.
- [ ] **Step 7:** Commit `feat(core): thêm giao thức lệnh/sự kiện JSON, C API và backend giả để test`.

### Task 3: Lõi — phiên đăng nhập, reconnect, lỗi auth

**Files:**
- Create: `core/engine/session.go`, `core/engine/errors.go`, `core/engine/session_test.go`
- Tham khảo: `pkg/messagix/client.go`, `pkg/messagix/loginerrors/errors.go`, `pkg/messagix/types/error.go`, `pkg/connector/login.go`

**Interfaces:**
- Consumes: `Backend`, `Event` (Task 2).
- Produces: `type MetaBackend struct` cài `Backend`. `func classifyError(err error) (state string, code string)` trả `("auth_required","token_invalid")` cho lỗi `1357001` hoặc DGW unauthorized, `("disconnected","rate_limited")` cho HTTP 429, `("auth_required","checkpoint")` cho checkpoint/consent/challenge.

- [ ] **Step 1:** Viết test: `TestClassify1357001IsAuthRequired`, `TestClassify429IsRateLimited`, `TestClassifyCheckpoint`, `TestAuthRequiredEmittedOnceAndNoReconnect` (backend giả trả lỗi unauthorized 3 lần thì chỉ phát 1 sự kiện `auth_required` và dừng reconnect), `TestMissingRequiredCookie` (thiếu `xs` thì response `error.code="missing_cookies"`).
- [ ] **Step 2:** `go test ./engine/ -run 'Classify|AuthRequired|MissingRequired'`. Kỳ vọng: FAIL.
- [ ] **Step 3:** Cài `session.go`: login tạo `messagix.Client` theo platform, chạy `connection_state` connecting → connected. Reconnect dùng logic có sẵn của messagix (backoff tối đa 5 phút). Lệnh `reconnect` ép kết nối lại ngay. Đã ở `auth_required` thì không tự reconnect. Cookie mới mà messagix trả về được phát qua event `cookies_updated{cookies}` để Swift lưu lại.
- [ ] **Step 4:** Chạy lại test. Kỳ vọng: PASS. Chạy `-race`.
- [ ] **Step 5:** Kiểm thủ công bằng CLI `core/cmd/devcli` (tạo mới, dùng chung engine): đăng nhập bằng cookie thật thì thấy `connected`. Sửa giá trị `xs` thành sai thì thấy đúng một `auth_required`.
- [ ] **Step 6:** Commit `feat(core): đăng nhập bằng cookie, tự kết nối lại và báo auth_required khi phiên hết hạn`.

### Task 4: Lõi — E2EE device, gộp hội thoại, sự kiện tin

**Files:**
- Create: `core/engine/e2ee.go`, `core/engine/conversations.go`, `core/engine/convert.go`, tests `conversations_test.go`, `convert_test.go`
- Tham khảo: `pkg/connector/client.go:562-620`, `pkg/messagix/e2ee-register.go`, `pkg/connector/handlemeta.go`, `pkg/connector/handlewhatsapp.go`, `pkg/connector/history_cutover.go`

**Interfaces:**
- Produces: `func conversationID(selfID, peerID int64) string` trả `"u:<peerID>"`. `type ThreadIndex` ánh xạ `thread_id` (thường và E2EE) sang `conversation_id`, và lưu link từ `LSCreateOpenToE2EEThreadLink`. `ThreadIndex.SendRoute(convID) (threadID string, e2ee bool)` ưu tiên E2EE nếu đã có link.
- `convert.go`: `fromLS(*table.LSUpsertMessage, ...) Message` và `fromWA(*events.FBMessage, ...) Message`, cả hai trả về `Message` của Task 2.
- Bỏ qua thread nhóm (theo loại thread), không phát sự kiện cho thread nhóm.

- [ ] **Step 1:** Viết test: `TestNormalAndE2EEThreadMapToSameConversation`, `TestSendRoutePrefersE2EEAfterLink`, `TestGroupThreadIgnored`, `TestDuplicateMessageIDEmittedOnce` (cùng message id đến từ 2 nguồn thì chỉ phát 1 `message_upsert`), `TestFromLSImageAttachment`, `TestFromWAFileAttachment`. Dữ liệu mẫu là struct dựng tay.
- [ ] **Step 2:** Chạy test. Kỳ vọng: FAIL.
- [ ] **Step 3:** Cài. Device E2EE lưu trong `<data_dir>/e2ee.db`. Có device rồi thì khôi phục theo JID, chưa có thì đăng ký mới. CAT refresh qua `MAWCatQuery`.
- [ ] **Step 4:** Chạy test với `-race`. Kỳ vọng: PASS.
- [ ] **Step 5:** Kiểm thủ công bằng devcli: một người vừa có tin cũ không mã hóa vừa có tin E2EE thì chỉ hiện 1 hội thoại. Tin đến realtime ở cả hai loại thread đều in ra.
- [ ] **Step 6:** Commit `feat(core): đăng ký device E2EE, gộp thread thường và E2EE của cùng một người thành một hội thoại`.

### Task 5: Lõi — gửi text/media, mark read, tải media

**Files:**
- Create: `core/engine/send.go`, `core/engine/media.go`, tests `send_test.go`, `media_test.go`
- Tham khảo: `pkg/connector/handlematrix.go:68-200, 613`, `pkg/messagix/httpclient/mercury.go`, `pkg/msgconv/to-whatsapp.go:284`, `pkg/connector/directmedia.go`

**Interfaces:**
- Produces: `func otidFromClientID(clientID string) int64`, xác định duy nhất từ `client_id` (dùng hash 63 bit). `const maxUploadBytes = 25 * 1024 * 1024`.
- Kết quả gửi: thấy `LSReplaceOptimsiticMessage` khớp OTID hoặc ack E2EE thì `sent`. Thấy `LSMarkOptimisticMessageFailed`/`LSHandleFailedTask` thì `failed`. Sau 30 giây không có xác nhận hoặc đứt kết nối sau khi đã ghi task thì `unknown`.

- [ ] **Step 1:** Viết test: `TestOTIDStableForSameClientID`, `TestSendTimeoutYieldsUnknownNotRetry` (backend giả không ack, sau timeout thì phát `unknown` và chỉ gọi gửi đúng 1 lần), `TestFileOver25MBRejectedBeforeUpload` (response `error.code="file_too_large"`, không gọi upload), `TestUnreadableFileRejected`, `TestMarkReadRoutesByThreadType`, `TestExpiredMediaURLRefreshedOn403`.
- [ ] **Step 2:** Chạy test. Kỳ vọng: FAIL.
- [ ] **Step 3:** Cài. Media không mã hóa đi qua Mercury upload (`farr`) rồi đưa id vào `attachment_fbids`. Media E2EE đi qua `whatsmeow.Upload`/`DownloadFB`. `download_media` ghi file vào `dest_path`.
- [ ] **Step 4:** Chạy test với `-race`. Kỳ vọng: PASS.
- [ ] **Step 5:** Kiểm thủ công bằng devcli: gửi text, ảnh, PDF vào 1 chat E2EE và 1 chat thường. Trên điện thoại thấy đủ, không trùng.
- [ ] **Step 6:** Commit `feat(core): gửi text/ảnh/file với chống gửi trùng theo client_id, đánh dấu đã đọc và tải media`.

### Task 6: Lõi — lịch sử và PIN secure history

**Files:**
- Create: `core/engine/history.go`, `core/engine/securehistory.go`, tests `history_test.go`
- Tham khảo: `pkg/connector/backfill.go`, `backfill_e2ee.go`, `login_history.go`, `history.go`, `history_upload.go`, `pkg/messagix/securehistory/`

**Interfaces:**
- `fetch_messages` trả về các `message_upsert` theo thứ tự thời gian giảm dần, cuối cùng là `response{ok, data:{has_more:Bool}}`. Lịch sử thường dùng `FetchMessagesTask` (label 228). Lịch sử E2EE đọc từ secure history đã khôi phục.
- Sau khi đăng nhập: chưa khôi phục thì phát `history_restore_state=needed`. `restore_history{pin}` chuyển sang `in_progress` rồi `done`, hoặc `failed{reason:"wrong_pin"|"no_backup"|"other"}`. Bí mật cần giữ lại phát qua event `secret_store{key, value}` để Swift lưu Keychain. Lõi không ghi PIN ra đĩa.
- Tin E2EE gửi đi được đẩy lên backup theo `history_upload.go`.

- [ ] **Step 1:** Viết test: `TestFetchPagingStopsWhenNoMore`, `TestFetchBatchesSpacedAtLeast1s`, `TestWrongPinEmitsFailedWrongPin`, `TestPinNeverWrittenToDisk` (quét data dir sau `restore_history` thì không thấy chuỗi PIN), `TestSentE2EEMessageQueuedForUpload`.
- [ ] **Step 2:** Chạy test. Kỳ vọng: FAIL.
- [ ] **Step 3:** Cài.
- [ ] **Step 4:** Chạy test với `-race`. Kỳ vọng: PASS.
- [ ] **Step 5:** Kiểm thủ công: nhập PIN đúng thì chat E2EE cũ hiện lịch sử. Nhập sai thì báo `wrong_pin`.
- [ ] **Step 6:** Commit `feat(core): tải lịch sử theo trang, khôi phục lịch sử E2EE bằng PIN và đẩy tin đã gửi lên backup`.

### Task 7: Swift `CookieImporter`

**Files:**
- Create: `MessengerKit/Sources/CookieImport/{BrowserCatalog.swift, ChromiumCookieReader.swift, ChromiumDecryptor.swift}`, `MessengerKit/Tests/CookieImportTests/*.swift`

**Interfaces:**
- `struct BrowserProfile { browser: Browser; name: String; cookiesPath: URL }`. `enum Browser { chrome, arc, brave, edge }`, mỗi trình duyệt có tên Keychain service riêng: `Chrome Safe Storage`, `Arc Safe Storage`, `Brave Safe Storage`, `Microsoft Edge Safe Storage`.
- `BrowserCatalog.installedProfiles() -> [BrowserProfile]`. Đọc `Local State` → `profile.info_cache` để lấy tên hiển thị.
- `ChromiumDecryptor(password: String).decrypt(_ blob: Data, metaVersion: Int) throws -> String`: blob có prefix `v10`, AES-128-CBC, key = PBKDF2-SHA1(password, `saltysalt`, 1003 vòng, 16 byte), IV = 16 byte `0x20`. Nếu `metaVersion >= 24` thì bỏ 32 byte đầu của plaintext.
- `ChromiumCookieReader.read(profile, password) throws -> [Cookie]`: copy `Cookies` cùng `-wal`/`-journal` ra thư mục tạm, đọc `meta.version`, lọc host `.messenger.com` và `.facebook.com`.
- `CookieImporter.importSession(profile) throws -> [Cookie]`: thiếu `c_user`, `xs` hoặc `datr` ở cả hai domain thì ném `.notLoggedIn`. Người dùng Deny Keychain thì ném `.keychainDenied`. DB không đọc được thì ném `.databaseUnreadable`.

- [ ] **Step 1:** Viết test: `testDecryptKnownVectorV10` (vector tạo trong test bằng CommonCrypto với password `"peanuts"`), `testStripsDomainHashWhenMetaVersion24`, `testPrefersMessengerDomain`, `testMissingXsThrowsNotLoggedIn`, `testLockedDatabaseIsCopiedNotOpenedInPlace` (fixture SQLite đang mở bởi một connection khác vẫn đọc được), `testKeychainDeniedMapsToError` (inject `KeychainReading` giả trả `errSecAuthFailed`).
- [ ] **Step 2:** `make kit-test FILTER=CookieImportTests`. Kỳ vọng: FAIL.
- [ ] **Step 3:** Cài. Đọc Keychain bằng `SecItemCopyMatching` qua protocol `KeychainReading` để test inject được. Đọc SQLite bằng GRDB.
- [ ] **Step 4:** Chạy test. Kỳ vọng: PASS.
- [ ] **Step 5:** Commit `feat(kit): đọc và giải mã cookie đăng nhập từ Chrome/Arc/Brave/Edge theo profile`.

### Task 8: Swift `LocalStore`, `CoreBridge` thật, `AppState`

**Files:**
- Create: `MessengerKit/Sources/LocalStore/{Schema.swift, LocalStore.swift}`, `MessengerKit/Sources/AppCore/{AppState.swift, SessionStore.swift}`, tests tương ứng

**Interfaces:**
- Consumes: `CoreBridge`, `CoreEvent` (Task 2). `CookieImporter` (Task 7).
- `LocalStore(path:)`: bảng `conversation(id PK)`, `message(id PK, conversation_id, client_id UNIQUE NULL, ts, status, ...)`, `attachment(message_id, ref, ...)`. `upsert(_ Message)`: nếu có `client_id` trùng với tin optimistic thì thay tin đó bằng `id` thật.
- `SessionStore`: lưu cookie và các `secret_store` trong Keychain service `dev.hieund.macchat`, account `<c_user>/<key>`.
- `@MainActor @Observable final class AppState`: `conversations: [Conversation]`, `messages(for:) -> [Message]`, `connection: ConnectionState`, `historyRestore: HistoryRestoreState`, `func send(text:to:)`, `func send(file:to:)`, `func retry(clientID:)`, `func loadOlder(conversationID:)`, `func markRead(conversationID:)`, `func importLogin(profile:)`, `func submitPIN(_:)`.
- `send` thêm tin optimistic `status=.sending` với `client_id = UUID()`. `retry` dùng lại `client_id` cũ.

- [ ] **Step 1:** Viết test (dùng backend giả của lõi): `testOptimisticReplacedByServerMessage`, `testDuplicateUpsertKeepsOneRow`, `testUnknownStatusShownAndNotAutoRetried`, `testRetryReusesClientID`, `testUnreadCountDecrementsOnMarkRead`, `testCookiesUpdatedPersistedToKeychain` (Keychain giả).
- [ ] **Step 2:** `make kit-test FILTER='LocalStoreTests|AppStateTests'`. Kỳ vọng: FAIL.
- [ ] **Step 3:** Cài.
- [ ] **Step 4:** Chạy test. Kỳ vọng: PASS.
- [ ] **Step 5:** Commit `feat(kit): lưu hội thoại/tin nhắn bằng GRDB và trạng thái app hợp nhất sự kiện từ lõi`.

### Task 9: UI SwiftUI

**Files:**
- Create trong `App/Sources/MacChat/`: `Onboarding/{RiskWarningView, BrowserPickerView, PINEntryView}.swift`, `Main/{SidebarView, ConversationRow, ChatView, MessageBubble, ComposerView, AuthBanner}.swift`, `Settings/SettingsView.swift`

**Interfaces:**
- Consumes: `AppState` (Task 8).
- Luồng màn hình: chưa có phiên → `RiskWarningView` → `BrowserPickerView` → nếu `historyRestore == .needed` thì `PINEntryView` (nút "Bỏ qua") → `NavigationSplitView`.
- `ComposerView`: Enter để gửi, Shift+Enter để xuống dòng. Nút 📎 mở `NSOpenPanel`. Kéo-thả file vào khung chat thì gửi file.
- `MessageBubble`: bấm ảnh để mở Quick Look (`QLPreviewPanel`). Tin `unknown` hiện chữ "Không chắc đã gửi" và nút "Gửi lại". Tin `failed` hiện chữ "Gửi lỗi" và nút "Gửi lại".
- Cuộn tới đầu danh sách thì gọi `loadOlder`. Hội thoại có `history_restored=false` và là E2EE thì hiện dòng "Chưa khôi phục lịch sử" kèm nút nhập PIN.
- `SidebarView` có ô tìm kiếm (`.searchable`), lọc theo tên hội thoại, không phân biệt dấu và hoa thường.
- `AuthBanner` hiện khi `connection == .authRequired`, có nút "Đăng nhập lại" mở lại `BrowserPickerView`.
- `SettingsView`: nhập PIN, chọn lại trình duyệt, đăng xuất, bật/tắt "Mở khi đăng nhập macOS".

- [ ] **Step 1:** Cài các view, chạy app bằng `{"backend":"fake"}` (biến môi trường `MACCHAT_FAKE=1`).
- [ ] **Step 2:** `make app && MACCHAT_FAKE=1 open build/MacChat.app`. Kiểm từng mục: luồng onboarding, gửi text, kéo-thả file, xem ảnh, tin `unknown` có nút gửi lại, banner auth. Chụp màn hình lưu vào `docs/screenshots/` (không commit).
- [ ] **Step 3:** `grep -rE 'WKWebView|WebKit|SFSafari' App MessengerKit`. Kỳ vọng: không có kết quả.
- [ ] **Step 4:** Commit `feat(app): giao diện đăng nhập, PIN, danh sách hội thoại và khung chat có gửi file`.

### Task 10: `Notifier` và `Lifecycle`

**Files:**
- Create: `MessengerKit/Sources/AppCore/NotificationPolicy.swift` + test, `App/Sources/MacChat/{Notifier.swift, Lifecycle.swift, AppDelegate.swift}`

**Interfaces:**
- `NotificationPolicy.shouldNotify(message:, activeConversationID:, appIsActive:, connectedAt:, lastKnownTs:) -> Bool`: trả false nếu tin do mình gửi, tin thuộc hội thoại đang mở khi app active, tin đến từ backfill, hoặc `ts < connectedAt` và `ts <= lastKnownTs`.
- `Notifier`: `UNUserNotificationCenter`, `userInfo["conversation_id"]`. Bấm vào thông báo thì mở hội thoại đó. Badge Dock = tổng `unread_count`.
- `Lifecycle`: `applicationShouldTerminateAfterLastWindowClosed` trả false. `applicationShouldHandleReopen` mở lại cửa sổ. `SMAppService.mainApp` cho login item, mặc định tắt. Nghe `NSWorkspace.didWakeNotification` và `NWPathMonitor` (chuyển sang `.satisfied`) rồi gọi `reconnect`.

- [ ] **Step 1:** Viết test cho `NotificationPolicy`: `testOwnMessageNotNotified`, `testActiveConversationNotNotified`, `testBacklogAfterWakeNotNotified`, `testNewMessageAfterWakeNotified`.
- [ ] **Step 2:** Chạy test. Kỳ vọng: FAIL. Cài code. Chạy lại. Kỳ vọng: PASS.
- [ ] **Step 3:** Kiểm thủ công: đóng cửa sổ thì app còn trên Dock. Nhờ người gửi tin thì có thông báo, bấm vào mở đúng hội thoại. Sleep 1 phút rồi wake thì kết nối lại và không có thông báo trùng.
- [ ] **Step 4:** Commit `feat(app): thông báo tin mới, badge Dock, chạy nền khi đóng cửa sổ và tự kết nối lại sau sleep`.

### Task 11: Đóng gói và phát hành

**Files:**
- Create: `scripts/create-signing-cert.md`, `scripts/release.sh`, `appcast.xml` (template), `README.md` (cài đặt, mở lần đầu, cảnh báo rủi ro, giấy phép)
- Modify: `App/Package.swift` (thêm Sparkle 2), `App/Resources/Info.plist` (`SUFeedURL`, `SUPublicEDKey`, `LSMinimumSystemVersion=14.0`)

- [ ] **Step 1:** Hướng dẫn tạo chứng chỉ tự ký "MacChat Self-Signed" (Keychain Access → Certificate Assistant, loại Code Signing). User tự làm một lần.
- [ ] **Step 2:** `release.sh <version>` làm các bước: `make core app` → `codesign --deep --force --options runtime -s "MacChat Self-Signed"` → `create-dmg` → `sign_update` (Sparkle EdDSA) → in đoạn XML cho `appcast.xml`. Khóa EdDSA riêng nằm trong Keychain của user, không commit.
- [ ] **Step 3:** Kiểm: `codesign -dv build/MacChat.app` hiện đúng authority. Cài từ `.dmg` trên một user macOS khác thì mở được qua "Open Anyway". Cập nhật từ 0.1.0 lên 0.1.1 qua Sparkle thì không bị hỏi lại quyền Keychain và quyền thông báo.
- [ ] **Step 4:** Commit `build: đóng gói dmg ký bằng chứng chỉ tự ký và tự cập nhật qua Sparkle`.
