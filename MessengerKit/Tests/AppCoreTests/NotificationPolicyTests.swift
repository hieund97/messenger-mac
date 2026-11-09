import CoreBridge
import Testing
@testable import AppCore

struct NotificationPolicyTests {
    private func msg(ts: Int64, outgoing: Bool = false, conversation: String = "u:1") -> Message {
        Message(id: "m\(ts)", conversationID: conversation, threadID: conversation, senderID: "1",
                isOutgoing: outgoing, ts: ts, text: "hi")
    }

    private func notify(_ m: Message, active: String? = nil, appIsActive: Bool = false,
                        connectedAt: Int64 = 1_000, lastKnownTs: Int64 = 500) -> Bool {
        NotificationPolicy.shouldNotify(message: m, activeConversationID: active, appIsActive: appIsActive,
                                        connectedAt: connectedAt, lastKnownTs: lastKnownTs)
    }

    @Test func testOwnMessageNotNotified() {
        #expect(notify(msg(ts: 2_000, outgoing: true)) == false)
    }

    @Test func testActiveConversationNotNotified() {
        #expect(notify(msg(ts: 2_000), active: "u:1", appIsActive: true) == false)
        // App không active thì hội thoại đang chọn vẫn báo; hội thoại khác vẫn báo.
        #expect(notify(msg(ts: 2_000), active: "u:1", appIsActive: false) == true)
        #expect(notify(msg(ts: 2_000), active: "u:2", appIsActive: true) == true)
    }

    @Test func testBacklogAfterWakeNotNotified() {
        // Tin cũ hơn lúc kết nối lại và không mới hơn tin cuối đã biết.
        #expect(notify(msg(ts: 400)) == false)
        #expect(notify(msg(ts: 500)) == false)
    }

    @Test func testNewMessageAfterWakeNotified() {
        // Sau thời điểm kết nối lại.
        #expect(notify(msg(ts: 1_500)) == true)
        // Tin bỏ lỡ lúc ngủ: trước lúc kết nối lại nhưng mới hơn tin cuối đã biết.
        #expect(notify(msg(ts: 800)) == true)
    }
}
