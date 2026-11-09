import CoreBridge

public enum NotificationPolicy {
    /// Có nên bắn thông báo cho tin này không. `connectedAt` và `lastKnownTs` tính bằng ms.
    /// - Tin của mình, tin thuộc hội thoại đang mở khi app active: không báo.
    /// - Tin cũ hơn lúc kết nối lại (`ts < connectedAt`) và không mới hơn tin cuối đã biết của hội thoại
    ///   (`ts <= lastKnownTs`) là backlog sau sleep/backfill: không báo.
    public static func shouldNotify(message: Message, activeConversationID: String?, appIsActive: Bool,
                                    connectedAt: Int64, lastKnownTs: Int64) -> Bool {
        if message.isOutgoing { return false }
        if appIsActive, message.conversationID == activeConversationID { return false }
        if message.ts < connectedAt, message.ts <= lastKnownTs { return false }
        return true
    }
}
