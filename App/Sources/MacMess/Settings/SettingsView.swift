import AppCore
import SwiftUI

struct SettingsView: View {
    @Environment(AppState.self) private var state
    @Environment(UIFlow.self) private var flow
    @AppStorage("launchAtLogin") private var launchAtLogin = false
    @AppStorage("riskAccepted") private var riskAccepted = false
    @State private var showPIN = false
    @State private var showPicker = false
    @State private var showRisk = false
    @State private var confirmLogout = false

    var body: some View {
        Form {
            Section("Tin nhắn mã hóa") {
                Button("Nhập PIN khôi phục lịch sử…") { showPIN = true }
                    .disabled(!state.isLoggedIn)
            }
            Section("Tài khoản") {
                Button("Chọn lại trình duyệt…") { if riskAccepted { showPicker = true } else { showRisk = true } }
                Button("Đăng xuất", role: .destructive) { confirmLogout = true }
                    .disabled(!state.isLoggedIn)
            }
            Section("Hệ thống") {
                Toggle("Mở khi đăng nhập macOS", isOn: $launchAtLogin)
            }
        }
        .formStyle(.grouped)
        .frame(width: 440, height: 340)
        .sheet(isPresented: $showPIN) {
            PINEntryView(onSkip: { showPIN = false })
        }
        .sheet(isPresented: $showRisk) {
            VStack {
                RiskAcceptView(onAccept: { riskAccepted = true; showRisk = false; showPicker = true },
                               onCancel: { showRisk = false })
            }
            .frame(width: 520, height: 380)
        }
        .sheet(isPresented: $showPicker) {
            BrowserPickerView(onDone: { showPicker = false; flow.step = .main }, onCancel: { showPicker = false })
        }
        .confirmationDialog("Đăng xuất khỏi MacMess?", isPresented: $confirmLogout) {
            Button("Đăng xuất", role: .destructive) {
                Task {
                    await state.logout()
                    flow.resetSession()
                    flow.step = .picker
                }
            }
            Button("Hủy", role: .cancel) {}
        } message: {
            Text("Cookie phiên đăng nhập sẽ bị xóa khỏi Keychain.")
        }
    }
}
