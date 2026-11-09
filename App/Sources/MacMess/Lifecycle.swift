import AppCore
import AppKit
import Network
import ServiceManagement
import SwiftUI

/// Vòng đời app: mở lại cửa sổ chính, login item, tự kết nối lại sau wake/mạng trở lại.
@MainActor
final class Lifecycle {
    static let shared = Lifecycle()
    static let windowID = "main"
    static let reconnectDebounce: Duration = .seconds(2)
    static let maxBackoff: Duration = .seconds(60)

    private var openWindow: (() -> Void)?
    private weak var state: AppState?
    private weak var mainWindow: NSWindow?
    private var closeObserver: (any NSObjectProtocol)?
    /// Gọi khi cửa sổ chính đóng (xóa hội thoại đang chọn để tin mới của nó vẫn được báo).
    var onMainWindowClosed: (() -> Void)?
    private var pathMonitor: NWPathMonitor?
    private var wasSatisfied: Bool?
    private var reconnectTask: Task<Void, Never>?
    private var wakeObserver: (any NSObjectProtocol)?

    /// `RootView` gọi khi xuất hiện: lưu hành động mở cửa sổ (`openWindow(id:)` vẫn dùng được sau khi cửa sổ đóng).
    func register(openWindow: @escaping () -> Void) { self.openWindow = openWindow }

    func register(window: NSWindow) {
        guard mainWindow !== window else { return }
        mainWindow = window
        if let closeObserver { NotificationCenter.default.removeObserver(closeObserver) }
        closeObserver = NotificationCenter.default.addObserver(
            forName: NSWindow.willCloseNotification, object: window, queue: .main) { [weak self] _ in
            Task { @MainActor in self?.onMainWindowClosed?() }
        }
    }

    var mainWindowIsVisible: Bool { mainWindow.map { $0.isVisible && !$0.isMiniaturized } ?? false }
    /// Cửa sổ chính đang hiện (không đóng/thu nhỏ). Kết hợp với `NSApp.isActive` để biết người dùng có đang nhìn hội thoại.
    var mainWindowIsFront: Bool { mainWindowIsVisible }

    func start(state: AppState) {
        guard self.state == nil else { return }
        self.state = state
        wakeObserver = NSWorkspace.shared.notificationCenter.addObserver(
            forName: NSWorkspace.didWakeNotification, object: nil, queue: .main) { [weak self] _ in
            Task { @MainActor in self?.scheduleReconnect() }
        }
        let monitor = NWPathMonitor()
        monitor.pathUpdateHandler = { [weak self] path in
            let satisfied = path.status == .satisfied
            Task { @MainActor in self?.pathChanged(satisfied: satisfied) }
        }
        monitor.start(queue: DispatchQueue(label: "dev.hieund.macmess.path"))
        pathMonitor = monitor
    }

    private func pathChanged(satisfied: Bool) {
        defer { wasSatisfied = satisfied }
        // Chỉ khi chuyển từ mất mạng sang có mạng (bỏ qua lần báo đầu tiên lúc khởi động).
        if satisfied, wasSatisfied == false { scheduleReconnect() }
    }

    /// Wake và mạng thường đến cùng lúc: gộp 2 giây rồi reconnect. Nếu chưa `connected` (và không cần đăng nhập lại,
    /// chưa đăng xuất) thì thử lại với backoff 2→4→8…→60 giây. Trigger mới hủy vòng cũ và đặt lại backoff.
    private func scheduleReconnect() {
        reconnectTask?.cancel()
        reconnectTask = Task { [weak self] in
            try? await Task.sleep(for: Self.reconnectDebounce)
            var wait = Self.reconnectDebounce
            while !Task.isCancelled, let state = self?.state, state.isLoggedIn, !state.connection.isAuthRequired {
                state.reconnect()
                try? await Task.sleep(for: wait)
                guard !Task.isCancelled else { return }
                if state.connection.state == "connected" { return }
                wait = min(wait * 2, Self.maxBackoff)
            }
        }
    }

    /// Đưa cửa sổ chính lên trước (mở lại nếu đã đóng). Trả true nếu đã xử lý.
    @discardableResult
    func showMainWindow() -> Bool {
        NSApp.setActivationPolicy(.regular)
        NSApp.activate()
        if let w = mainWindow, w.isVisible || w.isMiniaturized {
            if w.isMiniaturized { w.deminiaturize(nil) }
            w.makeKeyAndOrderFront(nil)
            return true
        }
        guard let openWindow else { return false }
        openWindow()
        DispatchQueue.main.async { self.mainWindow?.makeKeyAndOrderFront(nil) }
        return true
    }

    // MARK: Login item

    /// Bật hoặc đang chờ người dùng cho phép trong System Settings đều tính là "bật".
    static var launchAtLoginOn: Bool {
        let s = SMAppService.mainApp.status
        return s == .enabled || s == .requiresApproval
    }
    static var launchAtLoginNeedsApproval: Bool { SMAppService.mainApp.status == .requiresApproval }

    /// Đăng ký/hủy login item. Trả thông báo lỗi (đúng chiều bật/tắt) nếu thất bại, ví dụ app chưa nằm trong /Applications.
    static func setLaunchAtLogin(_ enabled: Bool) -> String? {
        let service = SMAppService.mainApp
        do {
            if enabled {
                if !launchAtLoginOn { try service.register() }
            } else if service.status != .notRegistered {
                try service.unregister()
            }
            return nil
        } catch {
            return "Không \(enabled ? "bật" : "tắt") được: \(error.localizedDescription)"
        }
    }
}

/// Lấy `NSWindow` chứa view để Lifecycle theo dõi cửa sổ chính.
struct WindowAccessor: NSViewRepresentable {
    func makeNSView(context: Context) -> NSView {
        let v = NSView()
        DispatchQueue.main.async { if let w = v.window { Lifecycle.shared.register(window: w) } }
        return v
    }
    func updateNSView(_ v: NSView, context: Context) {
        if let w = v.window { Lifecycle.shared.register(window: w) }
    }
}
