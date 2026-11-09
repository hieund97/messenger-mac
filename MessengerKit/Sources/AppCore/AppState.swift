import CookieImport
import CoreBridge
import Foundation
import LocalStore
import Observation
import UniformTypeIdentifiers

/// Phần của CoreBridge mà AppState dùng; tách ra để test thay bằng bản giả.
/// (Lõi Go là singleton toàn tiến trình, test chạy song song giữa các target nên không dùng lõi thật ở AppCoreTests.)
public protocol CoreLink: Sendable {
    var events: AsyncStream<CoreEvent> { get }
    func call(_ cmd: CoreCommand) async throws -> CoreResponse
    func shutdown() async
}

extension CoreBridge: CoreLink {}

extension ConnectionState {
    public static let disconnected = ConnectionState(state: "disconnected")
    public var isAuthRequired: Bool { state == "auth_required" }
    public var authRequiredReason: String? { isAuthRequired ? (reason ?? "") : nil }
}

public enum AppError: Error, Equatable, Sendable {
    case core(CoreError)
    case loginInProgress
}

@MainActor @Observable
public final class AppState {
    public static let maxFileSize: Int64 = 25 * 1024 * 1024
    /// Số tin cuối nạp vào cache khi mở hội thoại.
    public static let windowSize = 50
    /// Dung sai lệch đồng hồ (ms) khi so ts tin với `connectedAt`.
    static let clockSkewTolerance: Int64 = 5_000
    /// Khoảng cách tối thiểu giữa hai batch `loadOlder` của cùng hội thoại.
    public static let olderBatchInterval: Duration = .seconds(1)

    public private(set) var conversations: [Conversation] = []
    public private(set) var connection: ConnectionState = .disconnected
    public private(set) var historyRestore = HistoryRestoreState(state: "none")
    public private(set) var loginError: (any Error)?
    public private(set) var lastError: CoreError?
    /// Thời điểm (ms, đã trừ dung sai 5 giây) kết nối chuyển sang `connected` gần nhất; 0 nếu chưa kết nối.
    public private(set) var connectedAt: Int64 = 0

    @ObservationIgnored private var store: LocalStore?
    @ObservationIgnored private let storeFactory: @Sendable (String) throws -> LocalStore
    @ObservationIgnored private let session: SessionStore
    @ObservationIgnored private let importer: CookieImporter
    @ObservationIgnored private let backend: String
    @ObservationIgnored private let defaults: UserDefaults
    @ObservationIgnored private let dataDir: URL
    @ObservationIgnored private let bridgeFactory: @Sendable (CoreConfig) throws -> any CoreLink
    @ObservationIgnored private var link: (any CoreLink)?
    @ObservationIgnored private var eventTask: Task<Void, Never>?
    @ObservationIgnored private var cUser: String?
    /// Đang chạy import/login (UI dùng để không nhảy màn hình giữa chừng).
    public private(set) var isLoggingIn = false
    /// Đã đăng nhập vào lõi (có phiên đang chạy).
    public private(set) var isLoggedIn = false
    @ObservationIgnored private var exhausted: Set<String> = []
    @ObservationIgnored private var downloading: Set<String> = []
    @ObservationIgnored private var downloaded: [String: URL] = [:]
    @ObservationIgnored private var loadingOlder: Set<String> = []
    @ObservationIgnored private var lastOlderAt: [String: ContinuousClock.Instant] = [:]
    @ObservationIgnored private var observers: [UUID: AsyncStream<CoreEvent>.Continuation] = [:]
    /// Task của lệnh gần nhất (test dùng để chờ).
    @ObservationIgnored var lastCall: Task<Void, Never>?
    private var messageCache: [String: [Message]] = [:]
    /// ts lớn nhất đã thấy theo hội thoại trong phiên này.
    @ObservationIgnored private var knownTS: [String: Int64] = [:]
    /// ts cuối đã biết của hội thoại ngay trước khi nhận tin (theo id tin), cho `NotificationPolicy`.
    @ObservationIgnored private var knownTSBefore: [String: Int64] = [:]

    /// `storeFactory` nhận c_user và trả store của tài khoản đó; mặc định là store in-memory (tin nhắn không ghi ra đĩa).
    public init(storeFactory: (@Sendable (String) throws -> LocalStore)? = nil,
                session: SessionStore = SessionStore(),
                importer: CookieImporter = CookieImporter(),
                backend: String = "meta",
                defaults: UserDefaults = .standard,
                dataDir: URL = FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask)[0]
                    .appendingPathComponent("MacMess"),
                bridgeFactory: @escaping @Sendable (CoreConfig) throws -> any CoreLink = { try CoreBridge(config: $0) }) {
        self.storeFactory = storeFactory ?? { _ in try LocalStore() }
        self.session = session
        self.importer = importer
        self.backend = backend
        self.defaults = defaults
        self.dataDir = dataDir
        self.bridgeFactory = bridgeFactory
        Self.cleanUpLegacyFiles(dataDir: dataDir)
    }

    /// Thư mục tạm cho bản xem trước (Quick Look). Dọn mỗi lần khởi động.
    public nonisolated static var previewDirectory: URL {
        FileManager.default.temporaryDirectory.appendingPathComponent("MacMess-preview", isDirectory: true)
    }

    /// Xóa `app.db*` của bản trước (tin nhắn không còn lưu ra đĩa) và thư mục xem trước cũ.
    private static func cleanUpLegacyFiles(dataDir: URL) {
        Task.detached(priority: .utility) {
            let fm = FileManager.default
            try? fm.removeItem(at: previewDirectory)
            guard let accounts = try? fm.contentsOfDirectory(at: dataDir, includingPropertiesForKeys: nil) else { return }
            for account in accounts {
                guard let files = try? fm.contentsOfDirectory(atPath: account.path) else { continue }
                for f in files where f == "app.db" || f.hasPrefix("app.db-") {
                    try? fm.removeItem(at: account.appendingPathComponent(f))
                }
            }
        }
    }

    /// Tin trong cache của hội thoại (cửa sổ cuối đã nạp bằng `open`, cộng tin mới đến sau đó).
    public func messages(for conversationID: String) -> [Message] {
        messageCache[conversationID] ?? []
    }

    /// Nạp 50 tin cuối của hội thoại vào cache. UI gọi khi người dùng mở hội thoại.
    public func open(conversationID: String) async {
        guard let store else { return }
        if let list = try? await store.messages(conversationID: conversationID, limit: Self.windowSize) {
            messageCache[conversationID] = list
        }
    }

    /// Phát lại mọi sự kiện của lõi (sau khi đã hợp nhất vào store) cho thành phần khác, ví dụ Notifier.
    public func eventStream() -> AsyncStream<CoreEvent> {
        let (stream, cont) = AsyncStream<CoreEvent>.makeStream()
        let id = UUID()
        observers[id] = cont
        cont.onTermination = { [weak self] _ in
            Task { @MainActor in self?.observers[id] = nil }
        }
        return stream
    }

    /// ts cuối đã biết của hội thoại tại thời điểm tin này đến. Hội thoại chưa có tin nào trong phiên
    /// (đang nạp backlog đầu tiên) trả `Int64.max` để tin cũ không bị coi là mới.
    public func lastKnownTS(before message: Message) -> Int64 {
        knownTSBefore[message.id] ?? .max
    }

    /// Yêu cầu lõi kết nối lại (sau wake/mạng trở lại). Bỏ qua khi chưa đăng nhập hoặc cần đăng nhập lại.
    public func reconnect() {
        guard let link, isLoggedIn, !connection.isAuthRequired else { return }
        lastCall = Task { _ = try? await link.call(.reconnect) }
    }

    // MARK: Đăng nhập

    /// Import cookie từ trình duyệt (ngoài main actor vì Keychain có thể mở hộp thoại) rồi đăng nhập.
    public func importLogin(profile: BrowserProfile) async {
        guard !isLoggingIn else { loginError = AppError.loginInProgress; return }
        isLoggingIn = true
        defer { isLoggingIn = false }
        loginError = nil
        let importer = importer
        do {
            let cookies = try await Task.detached { try importer.importSession(profile) }.value
            await performLogin(cookies)
        } catch {
            loginError = error
        }
    }

    /// Lưu cookie vào Keychain, mở store của tài khoản, khởi tạo lõi và gửi `login`.
    /// Gọi khi đang login dở thì đặt `loginError = .loginInProgress` và không làm gì.
    public func login(cookies: [Cookie]) async {
        guard !isLoggingIn else { loginError = AppError.loginInProgress; return }
        isLoggingIn = true
        defer { isLoggingIn = false }
        loginError = nil
        await performLogin(cookies)
    }

    private enum LoginOutcome { case ok, offline, failed }

    /// `keepOnOffline`: lõi báo lỗi mạng (network/rate_limited) khi login thì giữ phiên ở trạng thái disconnected
    /// thay vì tearDown, để UI vào màn chính và lõi tự reconnect (dùng cho resume lúc khởi động).
    @discardableResult
    private func performLogin(_ cookies: [Cookie], keepOnOffline: Bool = false) async -> LoginOutcome {
        guard let user = cookies.first(where: { $0.name == "c_user" })?.value else {
            loginError = CookieImportError.notLoggedIn
            return .failed
        }
        // Đang ở auth_required thì giữ trạng thái này (AuthBanner) cho tới khi login mới thành công.
        let previousAuth: ConnectionState? = connection.isAuthRequired ? connection : nil
        func restoreAuth() { if let previousAuth { connection = previousAuth } }
        await tearDown()
        restoreAuth()
        cUser = user
        let params = cookies.map {
            CookieParam(name: $0.name, value: $0.value, domain: $0.domain, expires: $0.expires.map { Int64($0.timeIntervalSince1970) })
        }
        let platform = cookies.contains { $0.domain.hasSuffix("messenger.com") } ? "messenger" : "facebook"
        do {
            try await persistCookies(params, user: user)
            let factory = storeFactory
            let opened = try await Task.detached { try factory(user) }.value
            store = opened
            conversations = (try? await opened.conversations()) ?? []
            try FileManager.default.createDirectory(at: dataDir, withIntermediateDirectories: true)
            let link = try bridgeFactory(CoreConfig(dataDir: dataDir.path, backend: backend))
            self.link = link
            // Phải đọc events trước khi gửi login để không lỡ sự kiện đầu.
            eventTask = Task { [weak self] in
                for await event in link.events { await self?.apply(event) }
            }
            let resp = try await link.call(.login(cookies: params, platform: platform))
            if resp.ok {
                isLoggedIn = true
                defaults.set(user, forKey: Self.lastUserKey)
                return .ok
            }
            let err = resp.error ?? CoreError(code: "login_failed", message: "login failed")
            if keepOnOffline, ["network", "rate_limited"].contains(err.code) {
                isLoggedIn = true
                connection = ConnectionState(state: "disconnected", reason: err.code)
                defaults.set(user, forKey: Self.lastUserKey)
                return .offline
            }
            loginError = AppError.core(err)
            await tearDown()
            restoreAuth()
            return .failed
        } catch {
            loginError = error
            await tearDown()
            restoreAuth()
            return .failed
        }
    }

    static let lastUserKey = "lastUserID"

    public enum ResumeOutcome: Sendable, Equatable {
        /// Phiên đã khởi động (kể cả khi đang offline: lõi sẽ reconnect, trạng thái hiện ở `connection`).
        case resumed
        /// Không có phiên đã lưu, thiếu cookie hoặc lõi từ chối: người dùng phải chọn trình duyệt.
        case needsLogin
    }

    /// Đăng nhập lại bằng cookie đã lưu của tài khoản đăng nhập gần nhất.
    /// Lỗi mạng thì vẫn `.resumed` (offline); chỉ lỗi auth/thiếu cookie/không khởi tạo được lõi mới `.needsLogin`.
    /// Lỗi auth xảy ra sau khi login (Meta từ chối token) đến qua `connection` = `auth_required` → AuthBanner.
    public func resumeSavedSession() async -> ResumeOutcome {
        guard !isLoggingIn, let user = defaults.string(forKey: Self.lastUserKey) else { return .needsLogin }
        let session = session
        guard let saved = try? await Task.detached(operation: { try session.loadCookies(cUser: user) }).value,
              !saved.isEmpty else { return .needsLogin }
        let cookies = saved.map {
            Cookie(name: $0.name, value: $0.value, domain: $0.domain, expires: $0.expires.map { Date(timeIntervalSince1970: TimeInterval($0)) })
        }
        isLoggingIn = true
        defer { isLoggingIn = false }
        loginError = nil
        return await performLogin(cookies, keepOnOffline: true) == .failed ? .needsLogin : .resumed
    }

    public func logout() async {
        if let link { _ = try? await link.call(.logout) }
        await tearDown()
        if let user = cUser {
            let session = session
            try? await Task.detached { try session.deleteCookies(cUser: user) }.value
        }
        cUser = nil
        defaults.removeObject(forKey: Self.lastUserKey)
    }

    public func submitPIN(_ pin: String) {
        guard let link else { return }
        lastCall = Task { _ = try? await link.call(.restoreHistory(pin: pin)) }
    }

    /// Bỏ qua khôi phục lịch sử E2EE (nhập PIN sau ở Settings).
    public func skipHistoryRestore() {
        guard let link else { return }
        lastCall = Task { _ = try? await link.call(.skipHistoryRestore) }
    }

    // MARK: Gửi / đọc

    /// Tạo tin optimistic `.sending` (hiện ngay trong cache), ghi DB rồi gửi sang lõi.
    @discardableResult
    public func send(text: String, to conversationID: String) -> String {
        let clientID = UUID().uuidString
        let m = optimistic(conversationID: conversationID, clientID: clientID, text: text, attachments: [], status: .sending)
        patch(MessageChange(message: m, removedIDs: []))
        lastCall = Task {
            await persist(m)
            await call(.sendText(conversationID: conversationID, text: text, clientID: clientID), clientID: clientID)
        }
        return clientID
    }

    @discardableResult
    public func send(file: URL, to conversationID: String) -> String {
        let clientID = UUID().uuidString
        let mime = UTType(filenameExtension: file.pathExtension)?.preferredMIMEType ?? "application/octet-stream"
        let kind = mime.hasPrefix("image/") ? "image" : mime.hasPrefix("video/") ? "video" : "file"
        let size = (try? file.resourceValues(forKeys: [.fileSizeKey]).fileSize).map(Int64.init)
        let attachment = Attachment(ref: file.path, kind: kind, mime: mime, filename: file.lastPathComponent, size: size)
        // Không đọc được hoặc quá 25 MB: báo lỗi ngay trên bubble, không gửi sang lõi.
        let problem = Self.fileProblem(file)
        if let problem { lastError = problem }
        let m = optimistic(conversationID: conversationID, clientID: clientID, text: nil, attachments: [attachment],
                           status: problem == nil ? .sending : .failed)
        patch(MessageChange(message: m, removedIDs: []))
        lastCall = Task {
            await persist(m)
            if problem == nil {
                await call(.sendMedia(conversationID: conversationID, filePath: file.path, mime: mime, clientID: clientID), clientID: clientID)
            }
        }
        return clientID
    }

    /// Gửi lại tin `failed`/`unknown` bằng đúng `client_id` cũ. Tin file được kiểm lại kích thước/đọc được trước.
    public func retry(clientID: String) {
        guard var m = cachedMessage(clientID: clientID), m.status == .failed || m.status == .unknown else { return }
        let cmd: CoreCommand
        if let a = m.attachments.first {
            if let problem = Self.fileProblem(URL(fileURLWithPath: a.ref)) {
                lastError = problem
                return
            }
            cmd = .sendMedia(conversationID: m.conversationID, filePath: a.ref, mime: a.mime, clientID: clientID)
        } else {
            cmd = .sendText(conversationID: m.conversationID, text: m.text ?? "", clientID: clientID)
        }
        m.status = .sending
        patch(MessageChange(message: m, removedIDs: []))
        lastCall = Task {
            await setStatus(clientID: clientID, status: .sending)
            await call(cmd, clientID: clientID)
        }
    }

    /// Mỗi hội thoại chỉ một batch tại một thời điểm, cách nhau ≥ 1 giây.
    public func loadOlder(conversationID: String) {
        guard let link, let store, !loadingOlder.contains(conversationID), !exhausted.contains(conversationID) else { return }
        loadingOlder.insert(conversationID)
        lastCall = Task {
            if let last = lastOlderAt[conversationID] {
                let wait = Self.olderBatchInterval - (ContinuousClock.now - last)
                if wait > .zero { try? await Task.sleep(for: wait) }
            }
            let before = try? await store.oldestTS(conversationID: conversationID)
            let resp = try? await link.call(.fetchMessages(conversationID: conversationID, beforeTS: before, limit: 30))
            // Lõi báo hết lịch sử thì không gọi nữa.
            if case .object(let o)? = resp?.data, case .bool(false)? = o["has_more"] { exhausted.insert(conversationID) }
            lastOlderAt[conversationID] = .now
            loadingOlder.remove(conversationID)
        }
    }

    public enum DownloadError: Error, Equatable, Sendable { case failed, inProgress }

    /// Tải media vào thư mục tạm riêng (0700) rồi chuyển vào `directory` với tên an toàn (không đè file có sẵn),
    /// đặt quarantine sau khi chuyển. Đã tải trong phiên và file còn thì trả lại file cũ. Đang tải cùng attachment thì `.inProgress`.
    public func downloadMedia(_ attachment: Attachment, into directory: URL) async -> Result<URL, DownloadError> {
        let key = attachment.ref
        if let old = downloaded[key], FileManager.default.fileExists(atPath: old.path) { return .success(old) }
        guard let link else { return .failure(.failed) }
        guard downloading.insert(key).inserted else { return .failure(.inProgress) }
        defer { downloading.remove(key) }
        let fm = FileManager.default
        let tmp = URL(fileURLWithPath: NSTemporaryDirectory()).appendingPathComponent(UUID().uuidString, isDirectory: true)
        defer { try? fm.removeItem(at: tmp) }
        do {
            try fm.createDirectory(at: tmp, withIntermediateDirectories: false, attributes: [.posixPermissions: 0o700])
            let payload = tmp.appendingPathComponent("payload")
            let resp = try await link.call(.downloadMedia(attachmentRef: attachment.ref, destPath: payload.path))
            guard resp.ok, fm.fileExists(atPath: payload.path) else { return .failure(.failed) }
            try fm.createDirectory(at: directory, withIntermediateDirectories: true)
            let dest = try moveWithUniqueName(from: payload, toDirectory: directory, filename: attachment.filename)
            markQuarantined(dest)
            downloaded[key] = dest
            return .success(dest)
        } catch {
            return .failure(.failed)
        }
    }

    public func markRead(conversationID: String) {
        let lastTS = max(messages(for: conversationID).last?.ts ?? 0,
                         conversations.first { $0.id == conversationID }?.lastActivityTS ?? 0)
        lastCall = Task {
            if let store, let c = try? await store.markRead(conversationID: conversationID, untilTS: lastTS) { put(c) }
            if let link { _ = try? await link.call(.markRead(conversationID: conversationID, untilTS: lastTS)) }
        }
    }

    // MARK: Hợp nhất sự kiện

    /// Hợp nhất một sự kiện của lõi vào store và state, rồi phát lại cho observers.
    func apply(_ event: CoreEvent) async {
        switch event {
        case .connectionState(let s):
            if s.state == "connected", connection.state != "connected" {
                // Mốc "đã biết" của hội thoại chưa có tin nào trong phiên = hoạt động cuối trước lần kết nối này,
                // để tin bị lỡ lúc ngủ (mới hơn mốc) vẫn được báo còn backlog cũ thì không.
                for c in conversations where knownTS[c.id] == nil { knownTS[c.id] = c.lastActivityTS }
                // Trừ 5 giây dung sai lệch đồng hồ giữa máy và server.
                connectedAt = Int64(Date().timeIntervalSince1970 * 1000) - Self.clockSkewTolerance
            }
            connection = s
        case .historyRestoreState(let s):
            historyRestore = s
        case .conversationUpsert(let c):
            if let store, let stored = try? await store.upsert(c) { put(stored) }
        case .messageUpsert(let m):
            if knownTSBefore[m.id] == nil {
                if knownTSBefore.count > 500 { knownTSBefore.removeAll() }
                knownTSBefore[m.id] = knownTS[m.conversationID] ?? .max
            }
            knownTS[m.conversationID] = max(knownTS[m.conversationID] ?? 0, m.ts)
            if let store, let change = try? await store.upsert(m) { patch(change) }
        case .messageSendResult(let r):
            let status = MessageStatus(rawValue: r.status) ?? .unknown
            await setStatus(clientID: r.clientID, status: status, messageID: status == .sent ? r.messageID : nil)
        case .readReceipt(let r):
            // Đọc từ thiết bị khác của chính mình: chỉ trừ tin đến có ts ≤ until_ts.
            if r.readerID == cUser, let store, let c = try? await store.markRead(conversationID: r.conversationID, untilTS: r.untilTS) {
                put(c)
            }
        case .cookiesUpdated(let cookies):
            if let user = cUser { try? await persistCookies(cookies, user: user) }
        case .error(let e):
            lastError = e
        case .response, .unknown:
            break
        }
        for cont in observers.values { cont.yield(event) }
    }

    // MARK: Nội bộ

    public static func fileProblem(_ url: URL) -> CoreError? {
        guard FileManager.default.isReadableFile(atPath: url.path),
              let size = (try? url.resourceValues(forKeys: [.fileSizeKey]).fileSize) else {
            return CoreError(code: "file_unreadable", message: "cannot read file")
        }
        return Int64(size) > maxFileSize ? CoreError(code: "file_too_large", message: "file exceeds 25 MB") : nil
    }

    /// Keychain có thể chặn (hộp thoại): luôn chạy ngoài main actor.
    private func persistCookies(_ cookies: [CookieParam], user: String) async throws {
        let session = session
        try await Task.detached { try session.saveCookies(cookies, cUser: user) }.value
    }

    private func optimistic(conversationID: String, clientID: String, text: String?, attachments: [Attachment], status: MessageStatus) -> Message {
        Message(id: LocalStore.optimisticPrefix + clientID, conversationID: conversationID, threadID: conversationID,
                senderID: cUser ?? "", isOutgoing: true, ts: Int64(Date().timeIntervalSince1970 * 1000),
                text: text, clientID: clientID, attachments: attachments, status: status)
    }

    private func persist(_ m: Message) async {
        guard let store, let change = try? await store.upsert(m) else { return }
        patch(change)
    }

    private func setStatus(clientID: String, status: MessageStatus, messageID: String? = nil) async {
        guard let store, let change = try? await store.setStatus(clientID: clientID, status: status, messageID: messageID) else { return }
        patch(change)
    }

    /// Gửi lệnh send. Lõi từ chối hoặc không nhận lệnh → failed (chắc chắn chưa gửi);
    /// call đã đăng ký rồi bị huỷ/shutdown → unknown (không biết lõi đã gửi chưa, không tự retry).
    private func call(_ cmd: CoreCommand, clientID: String) async {
        guard let link else {
            await setStatus(clientID: clientID, status: .failed)
            return
        }
        do {
            if try await link.call(cmd).ok == false { await setStatus(clientID: clientID, status: .failed) }
        } catch CoreBridgeError.closedWhilePending {
            await setStatus(clientID: clientID, status: .unknown)
        } catch is CancellationError {
            await setStatus(clientID: clientID, status: .unknown)
        } catch {
            await setStatus(clientID: clientID, status: .failed)
        }
    }

    private func cachedMessage(clientID: String) -> Message? {
        for list in messageCache.values {
            if let m = list.first(where: { $0.clientID == clientID }) { return m }
        }
        return nil
    }

    /// Chèn/thay tin trong cache theo id, giữ thứ tự `ts`. Hội thoại chưa mở thì bỏ qua (`open` sẽ nạp từ DB).
    private func patch(_ change: MessageChange) {
        let m = change.message
        guard var list = messageCache[m.conversationID] else {
            if m.isOutgoing, m.id.hasPrefix(LocalStore.optimisticPrefix) { messageCache[m.conversationID] = [m] }
            return
        }
        list.removeAll { change.removedIDs.contains($0.id) || $0.id == m.id }
        let index = list.lastIndex { $0.ts <= m.ts }.map { $0 + 1 } ?? 0
        list.insert(m, at: index)
        messageCache[m.conversationID] = list
    }

    private func put(_ c: Conversation) {
        if let i = conversations.firstIndex(where: { $0.id == c.id }) { conversations[i] = c } else { conversations.append(c) }
        conversations.sort { $0.lastActivityTS != $1.lastActivityTS ? $0.lastActivityTS > $1.lastActivityTS : $0.id < $1.id }
    }

    /// Dừng lõi, đóng store và xoá state của phiên.
    private func tearDown() async {
        let link = link
        self.link = nil
        if let link { await link.shutdown() }
        eventTask?.cancel()
        eventTask = nil
        try? store?.close()
        store = nil
        conversations = []
        messageCache = [:]
        knownTS = [:]
        knownTSBefore = [:]
        connectedAt = 0
        historyRestore = HistoryRestoreState(state: "none")
        isLoggedIn = false
        exhausted = []
        downloading = []
        downloaded = [:]
        connection = .disconnected
        loadingOlder = []
        lastOlderAt = [:]
    }
}
