import CookieImport
import CoreBridge
import Foundation
@testable import AppCore
import LocalStore

final class MemoryKeychain: KeychainStoring, @unchecked Sendable {
    // @unchecked: bản giả cho test, mọi truy cập qua lock.
    private let lock = NSLock()
    private var items: [String: Data] = [:]

    func set(_ data: Data, service: String, account: String) throws {
        lock.lock(); defer { lock.unlock() }
        items["\(service)|\(account)"] = data
    }
    func get(service: String, account: String) throws -> Data? {
        lock.lock(); defer { lock.unlock() }
        return items["\(service)|\(account)"]
    }
    func delete(service: String, account: String) throws {
        lock.lock(); defer { lock.unlock() }
        items["\(service)|\(account)"] = nil
    }
}

/// Lõi giả: ghi lại lệnh, test tự bơm sự kiện qua `emit`.
actor MockLink: CoreLink {
    nonisolated let events: AsyncStream<CoreEvent>
    private let continuation: AsyncStream<CoreEvent>.Continuation
    private(set) var commands: [CoreCommand] = []

    init() {
        (events, continuation) = AsyncStream<CoreEvent>.makeStream()
    }

    nonisolated func call(_ cmd: CoreCommand) async throws -> CoreResponse {
        await record(cmd)
        return CoreResponse(ok: true)
    }

    private func record(_ cmd: CoreCommand) { commands.append(cmd) }

    nonisolated func shutdown() async { continuation.finish() }

    var sendMediaCount: Int {
        commands.filter { if case .sendMedia = $0 { true } else { false } }.count
    }

    var sendTextClientIDs: [String] {
        commands.compactMap { if case .sendText(_, _, let id) = $0 { id } else { nil } }
    }
}

let sessionCookies = [
    Cookie(name: "c_user", value: "100", domain: ".messenger.com"),
    Cookie(name: "xs", value: "xs-old", domain: ".messenger.com"),
    Cookie(name: "datr", value: "d", domain: ".messenger.com"),
]

/// AppState đã đăng nhập vào lõi giả.
@MainActor
func loggedInState(keychain: MemoryKeychain = MemoryKeychain()) async throws -> (AppState, MockLink, MemoryKeychain) {
    let link = MockLink()
    let state = AppState(storeFactory: { _ in try LocalStore(path: ":memory:") },
                         session: SessionStore(keychain: keychain),
                         backend: "fake",
                         dataDir: FileManager.default.temporaryDirectory.appendingPathComponent("macchat-test"),
                         bridgeFactory: { _ in link })
    await state.login(cookies: sessionCookies)
    return (state, link, keychain)
}

func storedCookies(_ keychain: MemoryKeychain, cUser: String) throws -> [CookieParam]? {
    try SessionStore(keychain: keychain).loadCookies(cUser: cUser)
}

/// Tạo file tạm có `bytes` byte (sparse).
func makeTempFile(bytes: Int) throws -> URL {
    let url = FileManager.default.temporaryDirectory.appendingPathComponent("macchat-\(UUID().uuidString).bin")
    FileManager.default.createFile(atPath: url.path, contents: nil)
    try resizeFile(url, to: bytes)
    return url
}

func resizeFile(_ url: URL, to bytes: Int) throws {
    let h = try FileHandle(forWritingTo: url)
    defer { try? h.close() }
    try h.truncate(atOffset: UInt64(bytes))
}
