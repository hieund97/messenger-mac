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

    @Test func testOlderCursorAndRestoreReset() async throws {
        let (state, link, _) = try await loggedInState()
        await state.open(conversationID: "u:2")
        await link.enqueue(CoreResponse(ok: true, data: .object([
            "has_more": .bool(true), "cursor": .string("opaque/+=="),
        ])))
        state.loadOlder(conversationID: "u:2")
        state.loadOlder(conversationID: "u:2")
        #expect(state.loadingOlder.contains("u:2"))
        await state.lastCall?.value
        await link.enqueue(CoreResponse(ok: true, data: .object([
            "has_more": .bool(false), "cursor": .string("end"), "needs_restore": .bool(true),
        ])))
        try await Task.sleep(for: .seconds(5))
        state.loadOlder(conversationID: "u:2", retry: true)
        await state.lastCall?.value
        let pages = await link.commands.filter { if case .fetchMessages = $0 { true } else { false } }
        #expect(pages == [
            .fetchMessages(conversationID: "u:2", cursor: nil, limit: 30),
            .fetchMessages(conversationID: "u:2", cursor: "opaque/+==", limit: 30),
        ])
        #expect(state.needsPINForOlder(conversationID: "u:2"))
        let count = await link.commands.count
        state.loadOlder(conversationID: "u:2")
        await state.lastCall?.value
        #expect(await link.commands.count == count)

        await link.enqueue(CoreResponse(ok: true, data: .object([
            "has_more": .bool(false), "cursor": .string("other-end"),
        ])))
        state.loadOlder(conversationID: "u:1")
        await state.lastCall?.value

        await state.apply(.historyRestoreState(HistoryRestoreState(state: "done")))
        await state.lastCall?.value
        #expect(!state.needsPINForOlder(conversationID: "u:2"))
        #expect(await link.commands.last == .fetchMessages(conversationID: "u:2", cursor: nil, limit: 30))
        state.loadOlder(conversationID: "u:1")
        await state.lastCall?.value
        #expect(await link.commands.last == .fetchMessages(conversationID: "u:1", cursor: nil, limit: 30))
    }

    /// Với lõi thật, response của fetch_messages về trước khi các message_upsert của batch hợp nhất xong
    /// (hai đường khác nhau). Batch phải chờ đủ `count` tin, và chỉ tạm dừng khi lõi báo 0 tin.
    @Test func testOlderWaitsForLateMessagesAndPausesOnlyOnZeroCount() async throws {
        let (state, link, _) = try await loggedInState()
        await state.open(conversationID: "u:2")
        await link.enqueue(CoreResponse(ok: true, data: .object([
            "has_more": .bool(true), "cursor": .string("c1"), "count": .int(2),
        ])))
        state.loadOlder(conversationID: "u:2")
        let batch = state.lastCall
        try await Task.sleep(for: .milliseconds(100))
        #expect(state.loadingOlder.contains("u:2"))
        for (id, ts) in [("old-1", 10), ("old-2", 20)] {
            await state.apply(.messageUpsert(Message(id: id, conversationID: "u:2", threadID: "u:2",
                                                   senderID: "2", isOutgoing: false, ts: Int64(ts), text: id)))
        }
        await batch?.value
        #expect(!state.loadingOlder.contains("u:2"))
        #expect(!state.olderPaused.contains("u:2"))
        #expect(state.messages(for: "u:2").map(\.id) == ["old-1", "old-2"])

        await link.enqueue(CoreResponse(ok: true, data: .object([
            "has_more": .bool(true), "cursor": .string("c2"), "count": .int(0),
        ])))
        state.loadOlder(conversationID: "u:2")
        await state.lastCall?.value
        #expect(state.olderPaused.contains("u:2"))
    }

    @Test func testRestoreCanRetryAfterSkipAndFailure() async throws {
        let (state, link, keychain) = try await loggedInState()
        state.skipHistoryRestore()
        await state.lastCall?.value
        await state.apply(.historyRestoreState(HistoryRestoreState(state: "skipped")))
        state.restoreHistory(pin: "000000")
        state.restoreHistory(pin: "000000")
        #expect(state.historyRestore.state == "in_progress")
        await state.lastCall?.value
        await state.apply(.historyRestoreState(HistoryRestoreState(state: "failed", reason: "wrong_pin", attemptsRemaining: 4)))
        #expect(state.historyRestore.attemptsRemaining == 4)
        state.restoreHistory(pin: "123456")
        await state.lastCall?.value
        let restores = await link.commands.filter { if case .restoreHistory = $0 { true } else { false } }
        #expect(restores == [.restoreHistory(pin: "000000"), .restoreHistory(pin: "123456")])
        #expect(try SessionStore(keychain: keychain).loadSecret(key: "pin", cUser: "100") == nil)
    }

    @Test func testConversationPaginationStopsAtEnd() async throws {
        let (state, link, _) = try await loggedInState()
        await link.enqueue(CoreResponse(ok: true, data: .object(["has_more": .bool(true), "cursor": .string("page-2")])))
        state.loadMoreConversations()
        state.loadMoreConversations()
        #expect(state.isLoadingConversations)
        await state.lastCall?.value
        await link.enqueue(CoreResponse(ok: true, data: .object(["has_more": .bool(false)])))
        state.loadMoreConversations()
        await state.lastCall?.value
        state.loadMoreConversations()
        let pages = await link.commands.filter { if case .fetchConversations = $0 { true } else { false } }
        #expect(pages == [.fetchConversations(cursor: nil), .fetchConversations(cursor: "page-2")])
        #expect(!state.hasMoreConversations)
        #expect(!state.isLoadingConversations)
    }

    @Test func testSecretStoredOverwrittenSentOnLoginAndDeletedOnLogout() async throws {
        let env = ResumeEnv()
        defer { env.cleanUp() }
        let first = env.makeState(MockLink())
        await first.login(cookies: sessionCookies)
        await first.apply(.secretStore(SecretStore(key: "secure_history_device", value: "old-device")))
        await first.apply(.secretStore(SecretStore(key: "secure_history_device", value: "new-device")))
        let session = SessionStore(keychain: env.keychain)
        #expect(try session.loadSecret(key: "secure_history_device", cUser: "100") == "new-device")
        let link = MockLink()
        let resumed = env.makeState(link)
        #expect(await resumed.resumeSavedSession() == .resumed)
        let secrets = await link.commands.compactMap { cmd -> [String: String]? in
            if case .login(_, _, let secrets) = cmd { secrets } else { nil }
        }
        #expect(secrets == [["secure_history_device": "new-device"]])
        await resumed.logout()
        #expect(try session.loadSecret(key: "secure_history_device", cUser: "100") == nil)
    }

    @Test func testEmptySecretEventDeletesKeychainItem() async throws {
        let (state, _, keychain) = try await loggedInState()
        let session = SessionStore(keychain: keychain)
        await state.apply(.secretStore(SecretStore(key: SessionStore.historySecretKey, value: "test-device")))
        #expect(try session.loadSecret(key: SessionStore.historySecretKey, cUser: "100") != nil)
        await state.apply(.secretStore(SecretStore(key: SessionStore.historySecretKey, value: "")))
        #expect(try session.loadSecret(key: SessionStore.historySecretKey, cUser: "100") == nil)
    }

    @Test func testAccountSwitchDeletesPreviousSecretsAndCookies() async throws {
        let env = ResumeEnv()
        defer { env.cleanUp() }
        let state = env.makeState(MockLink())
        await state.login(cookies: sessionCookies)
        await state.apply(.secretStore(SecretStore(key: SessionStore.historySecretKey, value: "test-device")))
        // Một AppState mới vẫn phải dọn tài khoản lưu từ phiên trước.
        let switched = env.makeState(MockLink())
        await switched.login(cookies: otherAccountCookies)
        #expect(switched.isLoggedIn)
        let session = SessionStore(keychain: env.keychain)
        #expect(try session.loadSecret(key: SessionStore.historySecretKey, cUser: "100") == nil)
        #expect(try session.loadCookies(cUser: "100") == nil)
        #expect(try session.loadCookies(cUser: "200") != nil)
    }

    @Test func testSecretReadFailureStillLogsInWithoutSecrets() async throws {
        let keychain = MemoryKeychain()
        keychain.rejectSecretReads()
        let (state, link, _) = try await loggedInState(keychain: keychain)
        #expect(state.isLoggedIn)
        #expect(state.loginError == nil)
        let secrets = await link.commands.compactMap { cmd -> [String: String]?? in
            if case .login(_, _, let s) = cmd { return .some(s) } else { return nil }
        }
        #expect(secrets.count == 1)
        #expect(secrets.first == .some(nil))
    }

    @Test func testNeedsRestoreErrorExhaustsUntilRestoreDone() async throws {
        let (state, link, _) = try await loggedInState()
        await state.open(conversationID: "u:2")
        await link.enqueue(CoreResponse(ok: false, error: CoreError(code: "needs_restore", message: "restore required")))
        state.loadOlder(conversationID: "u:2")
        await state.lastCall?.value
        #expect(state.needsPINForOlder(conversationID: "u:2"))
        #expect(state.olderError["u:2"] == nil)
        let count = await link.commands.count
        state.loadOlder(conversationID: "u:2", retry: true)
        await state.lastCall?.value
        #expect(await link.commands.count == count)
        await state.apply(.historyRestoreState(HistoryRestoreState(state: "done")))
        await state.lastCall?.value
        #expect(await link.commands.count == count + 1)
        await state.apply(.historyRestoreState(HistoryRestoreState(state: "needed")))
        #expect(state.historyRestore.state == "needed")
        #expect(state.needsPINForOlder(conversationID: "u:2"))
    }

    @Test func testOlderFailureIsPerConversationAndDoesNotAutoRetry() async throws {
        let (state, link, _) = try await loggedInState()
        await link.enqueue(CoreResponse(ok: false, error: CoreError(code: "timeout", message: "timeout")))
        state.loadOlder(conversationID: "u:2")
        await state.lastCall?.value
        #expect(state.olderError["u:2"]?.code == "timeout")
        #expect(state.olderError["u:1"] == nil)
        let count = await link.commands.count
        state.loadOlder(conversationID: "u:2")
        state.loadOlder(conversationID: "u:2", retry: true)
        await state.lastCall?.value
        #expect(await link.commands.count == count)
    }

    @Test func testMixedPagesSortByTimestampAndID() async throws {
        let (state, _, _) = try await loggedInState()
        await state.open(conversationID: "u:1")
        for (id, ts) in [("b", 20), ("z", 10), ("a", 20), ("c", 30)] {
            await state.apply(.messageUpsert(Message(id: id, conversationID: "u:1", threadID: "u:1",
                                                   senderID: "1", isOutgoing: false, ts: Int64(ts), text: id)))
        }
        #expect(state.messages(for: "u:1").map(\.id) == ["z", "a", "b", "c"])
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

    @Test func testMissedMessageDuringSleepNotifies() async throws {
        let (state, _, _) = try await loggedInState()
        let t1 = nowMillis() - 60_000
        await state.apply(.connectionState(ConnectionState(state: "connected")))
        await state.apply(.conversationUpsert(Conversation(id: "u:1", peerID: "1", name: "Alice", lastActivityTS: t1, unreadCount: 0)))
        await state.apply(.connectionState(ConnectionState(state: "disconnected")))
        await state.apply(.connectionState(ConnectionState(state: "connected")))

        // Tin đến lúc ngủ: sau tin cuối đã biết (t1), trước lúc kết nối lại.
        let missed = Message(id: "m-missed", conversationID: "u:1", threadID: "u:1", senderID: "1",
                             isOutgoing: false, ts: t1 + 30_000, text: "missed")
        // Backlog cũ: không mới hơn t1.
        let old = Message(id: "m-old", conversationID: "u:1", threadID: "u:1", senderID: "1",
                          isOutgoing: false, ts: t1 - 1_000, text: "old")
        await state.apply(.messageUpsert(missed))
        await state.apply(.messageUpsert(old))
        func notify(_ m: Message) -> Bool {
            NotificationPolicy.shouldNotify(message: m, activeConversationID: nil, appIsActive: false,
                                            connectedAt: state.connectedAt, lastKnownTs: state.lastKnownTS(before: m))
        }
        #expect(state.connectedAt > missed.ts)
        #expect(notify(missed) == true)
        #expect(notify(old) == false)
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

    @Test func testResumeUsesSavedCookies() async throws {
        let env = ResumeEnv()
        defer { env.cleanUp() }
        // Chưa từng đăng nhập: không resume được.
        #expect(await env.makeState(MockLink()).resumeSavedSession() == .needsLogin)

        await env.makeState(MockLink()).login(cookies: sessionCookies)
        #expect(env.lastUserID == "100")

        let link = MockLink()
        let state = env.makeState(link)
        #expect(await state.resumeSavedSession() == .resumed)
        let sent = await link.commands.compactMap { cmd -> [CookieParam]? in
            if case .login(let cookies, _, _) = cmd { cookies } else { nil }
        }
        #expect(sent.first?.first(where: { $0.name == "xs" })?.value == "xs-old")

        await state.logout()
        #expect(env.lastUserID == nil)
        #expect(await env.makeState(MockLink()).resumeSavedSession() == .needsLogin)
    }

    @Test func testSafeDownloadURLRejectsTraversal() {
        #expect(safeDownloadResult("../../.ssh/authorized_keys").name == "authorized_keys")
        #expect(safeDownloadResult("/etc/passwd").name == "passwd")
        #expect(safeDownloadResult(".hidden").name == "hidden")
        #expect(safeDownloadResult("a:b\u{1}c.txt").name == "a_b_c.txt")
        for bad in [nil, "", ".", ".."] {
            let r = safeDownloadResult(bad)
            #expect(r.inside && r.name.count == 36)
        }
        #expect(safeDownloadResult(String(repeating: "x", count: 300) + ".pdf").name.count <= 200)
        #expect(safeDownloadResult("a.txt", existing: ["a.txt"]).name == "a (1).txt")
        #expect(safeDownloadResult("a.txt", existing: ["a.txt", "a (1).txt"]).name == "a (2).txt")
        #expect(safeDownloadResult("README", existing: ["README"]).name == "README (1)")
        // Ký tự Cf (U+202E đảo chiều) bị thay.
        #expect(safeDownloadResult("a\u{202E}b.txt").name == "a_b.txt")
        // Tên Unicode dài: cắt theo byte, đúng ranh giới ký tự, giữ phần mở rộng.
        let long = safeDownloadResult(String(repeating: "đ", count: 150) + ".pdf").name
        #expect(long.utf8.count <= 200 && long.hasSuffix(".pdf") && long.dropLast(4).allSatisfy { $0 == "đ" })
        let longDup = safeDownloadResult(String(repeating: "界", count: 100) + ".txt", existing: [String(repeating: "界", count: 64) + ".txt"]).name
        #expect(longDup.utf8.count <= 200)
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
