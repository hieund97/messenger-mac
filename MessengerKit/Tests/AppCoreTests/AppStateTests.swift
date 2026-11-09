import CoreBridge
import Testing
@testable import AppCore

// Dùng lõi giả (MockLink) thay vì backend "fake" của Go: lõi là singleton toàn tiến trình và swift-testing
// chạy các target test song song, nên dùng lõi thật ở đây có thể đụng CoreSerialTests.
// Sự kiện được bơm vào AppState qua `apply(_:)` (@testable).
@MainActor
struct AppStateTests {
    private func result(_ id: String, _ status: String, messageID: String? = nil) -> CoreEvent {
        .messageSendResult(MessageSendResult(clientID: id, status: status, messageID: messageID))
    }

    @Test func testUnknownStatusShownAndNotAutoRetried() async throws {
        let (state, link, _) = try await loggedInState()
        await state.open(conversationID: "u:1")
        let id = state.send(text: "hi", to: "u:1")
        await state.lastCall?.value

        await state.apply(result(id, "unknown"))
        #expect(state.messages(for: "u:1").map(\.status) == [.unknown])

        let before = await link.commands.count
        for _ in 0..<20 { await Task.yield() }
        try await Task.sleep(for: .milliseconds(100))
        #expect(await link.commands.count == before)
        #expect(await link.sendTextClientIDs == [id])
        #expect(await link.sendMediaCount == 0)
        #expect(state.messages(for: "u:1").map(\.status) == [.unknown])
    }

    @Test func testRetryReusesClientID() async throws {
        let (state, link, _) = try await loggedInState()
        await state.open(conversationID: "u:1")
        let id = state.send(text: "hi", to: "u:1")
        await state.lastCall?.value
        await state.apply(result(id, "failed"))
        #expect(state.messages(for: "u:1").map(\.status) == [.failed])

        state.retry(clientID: id)
        await state.lastCall?.value
        #expect(await link.sendTextClientIDs == [id, id])
        #expect(state.messages(for: "u:1").count == 1)
        #expect(state.messages(for: "u:1").first?.status == .sending)
    }

    @Test func testUnreadCountDecrementsOnMarkRead() async throws {
        let (state, link, _) = try await loggedInState()
        await state.apply(.conversationUpsert(Conversation(id: "u:1", peerID: "1", name: "Alice", lastActivityTS: 5, unreadCount: 2)))
        await state.apply(.messageUpsert(Message(id: "m1", conversationID: "u:1", threadID: "u:1", senderID: "1",
                                                 isOutgoing: false, ts: 5, text: "yo")))
        #expect(state.conversations.first?.unreadCount == 2)

        state.markRead(conversationID: "u:1")
        await state.lastCall?.value
        #expect(state.conversations.first?.unreadCount == 0)
        #expect(await link.commands.contains(.markRead(conversationID: "u:1", untilTS: 5)))

        // conversation_upsert không mới hơn mốc đã đọc không được bật lại unread.
        await state.apply(.conversationUpsert(Conversation(id: "u:1", peerID: "1", name: "Alice", lastActivityTS: 5, unreadCount: 2)))
        #expect(state.conversations.first?.unreadCount == 0)
    }

    @Test func testCookiesUpdatedPersistedToKeychain() async throws {
        let (state, _, keychain) = try await loggedInState()
        #expect(try storedCookies(keychain, cUser: "100")?.first(where: { $0.name == "xs" })?.value == "xs-old")

        let fresh = [CookieParam(name: "c_user", value: "100", domain: ".messenger.com"),
                     CookieParam(name: "xs", value: "xs-new", domain: ".messenger.com")]
        await state.apply(.cookiesUpdated(fresh))
        #expect(try storedCookies(keychain, cUser: "100") == fresh)
    }

    @Test func testEchoBeforeSendResultKeepsServerData() async throws {
        let (state, _, _) = try await loggedInState()
        await state.open(conversationID: "u:1")
        let id = state.send(text: "hi", to: "u:1")
        await state.lastCall?.value

        // Echo đến trước, chưa mang client_id nên đứng thành dòng riêng.
        await state.apply(.messageUpsert(Message(id: "m9", conversationID: "u:1", threadID: "u:1", senderID: "100",
                                                 isOutgoing: true, ts: 777, text: "hi (server)")))
        #expect(state.messages(for: "u:1").count == 2)

        await state.apply(result(id, "sent", messageID: "m9"))
        let list = state.messages(for: "u:1")
        #expect(list.map(\.id) == ["m9"])
        #expect(list.first?.text == "hi (server)")
        #expect(list.first?.ts == 777)
        #expect(list.first?.clientID == id)
        #expect(list.first?.status == .sent)
    }

    @Test func testSendResultUnknownAfterEchoDoesNotDowngrade() async throws {
        let (state, _, _) = try await loggedInState()
        await state.open(conversationID: "u:1")
        let id = state.send(text: "hi", to: "u:1")
        await state.lastCall?.value

        await state.apply(.messageUpsert(Message(id: "m9", conversationID: "u:1", threadID: "u:1", senderID: "100",
                                                 isOutgoing: true, ts: 777, text: "hi", clientID: id)))
        await state.apply(result(id, "unknown"))
        let list = state.messages(for: "u:1")
        #expect(list.map(\.id) == ["m9"])
        #expect(list.first?.status == .sent)
    }

    @Test func testRetryRejectsOversizedFile() async throws {
        let (state, link, _) = try await loggedInState()
        await state.open(conversationID: "u:1")
        let file = try makeTempFile(bytes: 1024)
        let id = state.send(file: file, to: "u:1")
        await state.lastCall?.value
        #expect(await link.sendMediaCount == 1)
        await state.apply(result(id, "failed"))

        // File phình to sau lần gửi đầu: retry phải kiểm lại, giữ failed và không gửi sang lõi.
        try resizeFile(file, to: 26 * 1024 * 1024)
        state.retry(clientID: id)
        await state.lastCall?.value
        #expect(await link.sendMediaCount == 1)
        #expect(state.messages(for: "u:1").first?.status == .failed)
        #expect(state.lastError?.code == "file_too_large")
    }
}
