import AppCore
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
        case .databaseUnreadable: return "Không đọc được dữ liệu cookie. Hãy đóng trình duyệt rồi thử lại."
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

    var body: some View {
        VStack(spacing: 16) {
            Text("Chọn trình duyệt").font(.title2.bold())
            Text("Hãy đăng nhập messenger.com trên trình duyệt trước. Ứng dụng chỉ đọc cookie phiên đăng nhập, không lưu mật khẩu.")
                .font(.callout).foregroundStyle(.secondary).multilineTextAlignment(.center)
                .frame(maxWidth: 420)
            if profiles.isEmpty && !isFakeMode {
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
            if let err = state.loginError, !busy {
                Text(friendlyLoginError(err)).foregroundStyle(.red).font(.callout)
                    .multilineTextAlignment(.center).frame(maxWidth: 420)
            }
            HStack {
                if let onCancel { Button("Hủy", action: onCancel).disabled(busy) }
                Button {
                    login()
                } label: {
                    if busy { ProgressView().controlSize(.small) } else { Text("Đăng nhập") }
                }
                .keyboardShortcut(.defaultAction)
                .disabled(busy || (profiles.isEmpty && !isFakeMode))
            }
        }
        .padding(32)
        .frame(maxWidth: .infinity, maxHeight: .infinity)
        .frame(minWidth: 460, minHeight: 320)
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
                await state.importLogin(profile: profiles[selection])
            }
            busy = false
            if state.loginError == nil { onDone() }
        }
    }
}
