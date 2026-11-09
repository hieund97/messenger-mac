import AppCore
import AppKit
import CoreBridge
import Observation
import UserNotifications

/// Thông báo tin mới + badge Dock. Một consumer của `AppState.eventStream()`.
/// Tin nhắn chỉ nằm trong bộ nhớ nên nội dung thông báo cũng không lưu ở đâu ngoài Notification Center của hệ thống.
@MainActor
final class Notifier: NSObject, UNUserNotificationCenterDelegate {
    static let shared = Notifier()

    private var state: AppState?
    private var flow: UIFlow?
    private var task: Task<Void, Never>?
    private var askedPermission = false
    private var notified: Set<String> = []

    /// Chỉ gọi UNUserNotificationCenter khi chạy trong app bundle (binary trần không có bundle id sẽ crash).
    private var canNotify: Bool { !isFakeMode && Bundle.main.bundleIdentifier != nil }

    func start(state: AppState, flow: UIFlow) {
        guard self.state == nil else { return }
        self.state = state
        self.flow = flow
        // Fake mode hoặc không có bundle: không đụng vào thông báo của hệ thống (chỉ cập nhật badge).
        if canNotify { UNUserNotificationCenter.current().delegate = self }
        let events = state.eventStream()
        task = Task { [weak self] in
            for await event in events { self?.handle(event) }
        }
        trackBadge()
    }

    private func handle(_ event: CoreEvent) {
        guard let state else { return }
        switch event {
        case .connectionState(let s) where s.state == "connected":
            requestPermissionOnce()
        case .messageUpsert(let m):
            guard canNotify, !notified.contains(m.id) else { return }
            let allow = NotificationPolicy.shouldNotify(
                message: m, activeConversationID: flow?.selectedConversationID, appIsActive: NSApp.isActive && Lifecycle.shared.mainWindowIsFront,
                connectedAt: state.connectedAt, lastKnownTs: state.lastKnownTS(before: m))
            guard allow else { return }
            if notified.count > 1000 { notified.removeAll() }
            notified.insert(m.id)
            post(m)
        default:
            break
        }
    }

    /// Xin quyền lần đầu sau khi đăng nhập thành công (kết nối lần đầu).
    private func requestPermissionOnce() {
        guard !askedPermission, canNotify else { return }
        askedPermission = true
        UNUserNotificationCenter.current().requestAuthorization(options: [.alert, .sound, .badge]) { _, _ in }
    }

    private func post(_ m: Message) {
        let name = state?.conversations.first { $0.id == m.conversationID }?.name ?? "Messenger"
        let content = UNMutableNotificationContent()
        content.title = name
        content.body = Self.body(for: m)
        content.sound = .default
        content.threadIdentifier = m.conversationID
        content.userInfo = ["conversation_id": m.conversationID]
        let request = UNNotificationRequest(identifier: m.id, content: content, trigger: nil)
        UNUserNotificationCenter.current().add(request) { _ in }
    }

    nonisolated static func body(for m: Message) -> String {
        if let text = m.text, !text.isEmpty { return text }
        return m.attachments.contains { $0.kind == "image" } ? "📷 Ảnh" : "📎 File"
    }

    /// Badge Dock = tổng unread; theo dõi `conversations` nên cũng tự xóa khi đăng xuất.
    private func trackBadge() {
        withObservationTracking {
            let total = state?.conversations.reduce(0) { $0 + $1.unreadCount } ?? 0
            NSApp?.dockTile.badgeLabel = total > 0 ? String(total) : ""
        } onChange: { [weak self] in
            Task { @MainActor in self?.trackBadge() }
        }
    }

    // MARK: UNUserNotificationCenterDelegate

    nonisolated func userNotificationCenter(_ center: UNUserNotificationCenter, didReceive response: UNNotificationResponse,
                                            withCompletionHandler completionHandler: @escaping @Sendable () -> Void) {
        let id = response.notification.request.content.userInfo["conversation_id"] as? String
        Task { @MainActor in
            if let id { self.flow?.selectedConversationID = id }
            Lifecycle.shared.showMainWindow()
            completionHandler()
        }
    }

    /// App đang ở foreground: vẫn hiện banner cho hội thoại khác (policy đã lọc hội thoại đang mở).
    nonisolated func userNotificationCenter(_ center: UNUserNotificationCenter, willPresent notification: UNNotification,
                                            withCompletionHandler completionHandler: @escaping @Sendable (UNNotificationPresentationOptions) -> Void) {
        completionHandler([.banner, .sound])
    }
}
