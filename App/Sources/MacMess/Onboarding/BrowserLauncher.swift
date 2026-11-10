import AppKit
import CookieImport

/// Mở messenger.com bằng đúng trình duyệt/profile người dùng chọn; ghi nhớ lựa chọn gần nhất cho AuthBanner.
enum BrowserLauncher {
    private static let defaultsKey = "lastBrowserProfile"
    private static let messengerURL = URL(string: "https://www.messenger.com/")!

    static func remember(_ profile: BrowserProfile) {
        UserDefaults.standard.set(
            ["browser": profile.browser.rawValue, "path": profile.cookiesPath.path], forKey: defaultsKey)
    }

    static func lastProfile() -> BrowserProfile? {
        guard let d = UserDefaults.standard.dictionary(forKey: defaultsKey) as? [String: String],
              let raw = d["browser"], let browser = Browser(rawValue: raw), let path = d["path"] else { return nil }
        return BrowserProfile(browser: browser, name: "", cookiesPath: URL(fileURLWithPath: path))
    }

    /// Thư mục profile = thư mục chứa `Cookies`, bỏ `Network` nếu có.
    static func profileDirectoryName(_ profile: BrowserProfile) -> String {
        var dir = profile.cookiesPath.deletingLastPathComponent()
        if dir.lastPathComponent == "Network" { dir = dir.deletingLastPathComponent() }
        return dir.lastPathComponent
    }

    /// Không có profile thì mở bằng trình duyệt mặc định.
    static func openMessenger(in profile: BrowserProfile?) {
        guard let profile,
              let appURL = NSWorkspace.shared.urlForApplication(withBundleIdentifier: profile.browser.bundleIdentifier)
        else {
            NSWorkspace.shared.open(messengerURL)
            return
        }
        let p = Process()
        p.executableURL = URL(fileURLWithPath: "/usr/bin/open")
        p.arguments = ["-na", appURL.path, "--args",
                       "--profile-directory=\(profileDirectoryName(profile))", messengerURL.absoluteString]
        do { try p.run() } catch {
            NSWorkspace.shared.open([messengerURL], withApplicationAt: appURL, configuration: NSWorkspace.OpenConfiguration())
        }
    }

    static func openFullDiskAccessSettings() {
        if let url = URL(string: "x-apple.systempreferences:com.apple.preference.security?Privacy_AllFiles") {
            NSWorkspace.shared.open(url)
        }
    }
}
