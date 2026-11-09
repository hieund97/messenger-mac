import Testing
@testable import CoreBridge

struct TestTimeout: Error {}

/// Chạy op, quá hạn thì huỷ và ném TestTimeout (test fail thay vì treo).
func withTimeout<T: Sendable>(_ seconds: UInt64, _ op: @escaping @Sendable () async throws -> T) async throws -> T {
    try await withThrowingTaskGroup(of: T.self) { group in
        group.addTask { try await op() }
        group.addTask {
            try await Task.sleep(nanoseconds: seconds * 1_000_000_000)
            throw TestTimeout()
        }
        defer { group.cancelAll() }
        return try await group.next()!
    }
}

/// Tạo bridge fake, luôn shutdown dù body ném lỗi (defer không await được).
func withFakeBridge(_ body: (CoreBridge) async throws -> Void) async throws {
    let bridge = try CoreBridge(config: CoreConfig(backend: "fake"))
    do { try await body(bridge) } catch {
        await bridge.shutdown()
        throw error
    }
    await bridge.shutdown()
}

extension CoreSerialTests {
    @Test func testFakeLoginFlow() async throws {
        try await withFakeBridge { bridge in
            let collector = Task { () -> [CoreEvent] in
                var got: [CoreEvent] = []
                for await e in bridge.events {
                    got.append(e)
                    if got.count == 3 { break }
                }
                return got
            }
            let login = CoreCommand.login(cookies: [CookieParam(name: "c_user", value: "1", domain: ".facebook.com")], platform: "facebook")
            let resp = try await withTimeout(2) { try await bridge.call(login) }
            #expect(resp.ok)
            let events: [CoreEvent]
            do { events = try await withTimeout(2) { await collector.value } } catch {
                collector.cancel()
                throw error
            }
            #expect(events.count == 3)
            if events.count == 3 {
                if case .connectionState(let s) = events[0] { #expect(s.state == "connected") } else { Issue.record("event 0: \(events[0])") }
                for i in 1...2 {
                    if case .conversationUpsert = events[i] {} else { Issue.record("event \(i): \(events[i])") }
                }
            }
        }
    }

    @Test func testUnknownCommandReturnsError() async throws {
        try await withFakeBridge { bridge in
            let resp = try await withTimeout(2) { try await bridge.callRaw(cmd: "nope", args: [:]) }
            #expect(!resp.ok)
            #expect(resp.error?.code == "unknown_cmd")
        }
    }

    @Test func testSecondInitWhileRunningThrows() async throws {
        try await withFakeBridge { _ in
            #expect(throws: CoreBridgeError.alreadyInitialized) {
                _ = try CoreBridge(config: CoreConfig(backend: "fake"))
            }
        }
    }

    // Không có testShutdownResumesPendingCalls: FakeBackend trả ngay mọi lệnh, không có lệnh treo để giữ pending.

    @Test func testMessageUpsertWithNullAttachmentsDecodes() throws {
        let json = #"{"type":"message_upsert","request_id":7,"data":{"id":"1","conversation_id":"u:1","thread_id":"u:1","e2ee":false,"sender_id":"1","is_outgoing":false,"ts":1,"text":"Hi","attachments":null}}"#
        let event = try CoreEvent.parse(json)
        guard case .messageUpsert(let m) = event else {
            Issue.record("event: \(event)")
            return
        }
        #expect(m.attachments.isEmpty)
        #expect(m.text == "Hi")
    }
}
