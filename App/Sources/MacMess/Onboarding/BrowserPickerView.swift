import AppCore
import AppKit
import CookieImport
import SwiftUI

/// Mã lỗi/lý do của lõi (login response hoặc connection_state.reason) → tiếng Việt.
func friendlyCoreReason(_ code: String) -> String {
    switch code {
    case "token_invalid": "Phiên đăng nhập không còn hợp lệ."
    case "checkpoint": "Meta yêu cầu xác minh tài khoản (checkpoint). Hãy mở messenger.com trên trình duyệt để xác minh."
    case "account_suspended": "Tài khoản đang bị Meta hạn chế hoặc khóa."
    case "network": "Không có kết nối mạng."
    case "rate_limited": "Meta đang giới hạn tần suất truy cập, hãy thử lại sau."
    case "missing_cookies": "Thiếu cookie đăng nhập. Hãy đăng nhập messenger.com trên trình duyệt."
    default: "Lỗi: \(code)"
    }
}

func friendlyLoginError(_ error: any Error) -> String {
    if let e = error as? CookieImportError {
        switch e {
        case .notLoggedIn: return "Chưa đăng nhập messenger.com trên trình duyệt này. Hãy đăng nhập rồi thử lại."
        case .keychainDenied: return "Bạn đã từ chối quyền truy cập Keychain của trình duyệt. Hãy thử lại và chọn Cho phép."
        case .keychainUnavailable: return "Không truy cập được Keychain của trình duyệt."
        case .databaseUnreadable: return "Không đọc được dữ liệu cookie của trình duyệt. Hãy thử lại."
        case .permissionDenied: return "MacMess cần quyền Full Disk Access để đọc phiên đăng nhập của trình duyệt."
        case .decryptFailed: return "Không giải mã được cookie của trình duyệt."
        }
    }
    if let e = error as? AppError {
        switch e {
        case .loginInProgress: return "Đang đăng nhập, vui lòng chờ."
        case .core(let c): return "Đăng nhập thất bại. " + friendlyCoreReason(c.code)
        }
    }
    return "Đăng nhập thất bại: \(error.localizedDescription)"
}

struct BrowserPickerView: View {
    @Environment(AppState.self) private var state
    var onDone: () -> Void
    var onCancel: (() -> Void)?

    @State private var profiles: [BrowserProfile] = BrowserCatalog.installedProfiles()
    @State private var selection: Int = 0
    @State private var busy = false
    @State private var noAccess = !isFakeMode && !BrowserCatalog.hasDataAccess()

    var body: some View {
        VStack(spacing: 16) {
            Text("Chọn trình duyệt").font(.title2.bold())
            Text("Hãy đăng nhập messenger.com trên trình duyệt trước. Ứng dụng chỉ đọc cookie phiên đăng nhập, không lưu mật khẩu.")
                .font(.callout).foregroundStyle(.secondary).multilineTextAlignment(.center)
                .frame(maxWidth: 420)
            if noAccess || isPermissionDenied {
                permissionGuide
            } else if profiles.isEmpty && !isFakeMode {
                Text("Không tìm thấy trình duyệt nào (Chrome, Arc, Brave, Edge).").foregroundStyle(.secondary)
            } else {
                Picker("Profile", selection: $selection) {
                    ForEach(profiles.indices, id: \.self) { i in
                        Text("\(profiles[i].browser.displayName) – \(profiles[i].name)").tag(i)
                    }
                }
                .pickerStyle(.radioGroup).labelsHidden()
                .disabled(busy)
            }
            if let err = state.loginError, !busy, !noAccess, !isPermissionDenied {
                Text(friendlyLoginError(err)).foregroundStyle(.red).font(.callout)
                    .multilineTextAlignment(.center).frame(maxWidth: 420)
                if showsOpenMessenger(err), profiles.indices.contains(selection) {
                    let profile = profiles[selection]
                    VStack(spacing: 6) {
                        Button("Mở messenger.com trong \(profile.browser.displayName)") {
                            BrowserLauncher.openMessenger(in: profile)
                        }
                        Text("Đăng nhập xong, quay lại đây bấm Đăng nhập.")
                            .font(.caption).foregroundStyle(.secondary)
                    }
                }
            }
            HStack {
                if let onCancel { Button("Hủy", action: onCancel).disabled(busy) }
                Button {
                    login()
                } label: {
                    if busy { ProgressView().controlSize(.small) } else { Text("Đăng nhập") }
                }
                .keyboardShortcut(.defaultAction)
                .disabled(busy || noAccess || (profiles.isEmpty && !isFakeMode))
            }
        }
        .padding(32)
        .frame(maxWidth: .infinity, maxHeight: .infinity)
        .frame(minWidth: 460, minHeight: 320)
    }

    private var isPermissionDenied: Bool {
        (state.loginError as? CookieImportError) == .permissionDenied
    }

    private func showsOpenMessenger(_ error: any Error) -> Bool {
        if (error as? CookieImportError) == .notLoggedIn { return true }
        if case .core(let c)? = error as? AppError { return c.code == "missing_cookies" }
        return false
    }

    private var permissionGuide: some View {
        VStack(spacing: 10) {
            Text("MacMess cần quyền Full Disk Access để đọc phiên đăng nhập của trình duyệt.")
                .font(.callout.bold()).multilineTextAlignment(.center)
            Text("Bật MacMess trong danh sách (nếu chưa có thì bấm + và chọn MacMess.app). macOS có thể yêu cầu thoát và mở lại app.")
                .font(.callout).foregroundStyle(.secondary).multilineTextAlignment(.center)
            HStack {
                Button("Mở cài đặt quyền") { BrowserLauncher.openFullDiskAccessSettings() }
                Button("Thử lại") { retryAfterPermission() }.disabled(busy)
            }
        }
        .frame(maxWidth: 420)
    }

    /// Làm mới danh sách profile rồi thử đăng nhập lại nếu đã có quyền.
    private func retryAfterPermission() {
        profiles = BrowserCatalog.installedProfiles()
        selection = 0
        noAccess = !BrowserCatalog.hasDataAccess()
        if !noAccess, !profiles.isEmpty { login() }
    }

    private func login() {
        busy = true
        Task {
            if isFakeMode {
                let fake = [Cookie(name: "c_user", value: "100", domain: ".messenger.com"),
                            Cookie(name: "xs", value: "x", domain: ".messenger.com"),
                            Cookie(name: "datr", value: "d", domain: ".messenger.com")]
                await state.login(cookies: fake)
            } else if profiles.indices.contains(selection) {
                BrowserLauncher.remember(profiles[selection])
                await state.importLogin(profile: profiles[selection])
            }
            busy = false
            if state.loginError == nil { onDone() }
        }
    }
}
