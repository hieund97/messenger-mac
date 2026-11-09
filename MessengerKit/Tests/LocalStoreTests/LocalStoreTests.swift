import CoreBridge
import Testing
@testable import LocalStore

private func message(id: String, clientID: String? = nil, text: String, ts: Int64, status: MessageStatus = .sent) -> Message {
    Message(id: id, conversationID: "u:1", threadID: "u:1", senderID: "1", isOutgoing: clientID != nil,
            ts: ts, text: text, clientID: clientID, status: status)
}

struct LocalStoreTests {
    @Test func testOptimisticReplacedByServerMessage() async throws {
        let store = try LocalStore(path: ":memory:")
        _ = try await store.upsert(message(id: "local-c1", clientID: "c1", text: "hi", ts: 10, status: .sending))
        #expect(try await store.messages(conversationID: "u:1", limit: 50).map(\.id) == ["local-c1"])

        let change = try await store.upsert(message(id: "m100", clientID: "c1", text: "hi", ts: 11))
        #expect(change.removedIDs == ["local-c1"])
        let rows = try await store.messages(conversationID: "u:1", limit: 50)
        #expect(rows.map(\.id) == ["m100"])
        #expect(rows.first?.status == .sent)
        #expect(rows.first?.clientID == "c1")
    }

    @Test func testDuplicateUpsertKeepsOneRow() async throws {
        let store = try LocalStore(path: ":memory:")
        _ = try await store.upsert(message(id: "m1", text: "a", ts: 1))
        _ = try await store.upsert(message(id: "m1", text: "a edited", ts: 1))
        let rows = try await store.messages(conversationID: "u:1", limit: 50)
        #expect(rows.count == 1)
        #expect(rows.first?.text == "a edited")
    }

    @Test func testSendingBecomesUnknownOnReopen() async throws {
        let path = tempDBPath()
        let first = try LocalStore(path: path)
        _ = try await first.upsert(message(id: "local-c1", clientID: "c1", text: "hi", ts: 10, status: .sending))
        try first.close()

        let second = try LocalStore(path: path)
        #expect(try await second.messages(conversationID: "u:1", limit: 50).map(\.status) == [.unknown])
        try second.close()
    }
}
