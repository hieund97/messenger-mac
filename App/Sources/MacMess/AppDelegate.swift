import AppKit

final class AppDelegate: NSObject, NSApplicationDelegate {
    /// Đóng cửa sổ chính thì app vẫn chạy nền (còn trên Dock, tiếp tục nhận tin).
    func applicationShouldTerminateAfterLastWindowClosed(_ sender: NSApplication) -> Bool { false }

    /// Bấm icon Dock khi không còn cửa sổ → mở lại cửa sổ chính.
    @MainActor
    func applicationShouldHandleReopen(_ sender: NSApplication, hasVisibleWindows flag: Bool) -> Bool {
        // Cửa sổ Settings đang mở cũng làm `flag` = true nên tự kiểm cửa sổ chính.
        if Lifecycle.shared.mainWindowIsVisible { return true }
        return !Lifecycle.shared.showMainWindow()
    }
}
