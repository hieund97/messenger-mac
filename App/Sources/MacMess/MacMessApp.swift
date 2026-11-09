import AppCore
import Combine
import CookieImport
import Sparkle
import SwiftUI

/// Trạng thái điều hướng của UI (không thuộc AppCore).
@MainActor @Observable
final class UIFlow {
    enum Step { case risk, resuming, picker, main }
    var step: Step
    var selectedConversationID: String?
    var showPicker = false
    var showPIN = false
    var pinDismissed = false
    /// Đã resume xong nhưng cần chọn trình duyệt (dùng để ResumingView ngắt chờ).
    func resetSession() { selectedConversationID = nil; pinDismissed = false; showPIN = false }

    init() {
        step = UserDefaults.standard.bool(forKey: "riskAccepted") ? .resuming : .risk
    }

    func acceptRisk() {
        UserDefaults.standard.set(true, forKey: "riskAccepted")
        step = .picker
    }
}

let isFakeMode = ProcessInfo.processInfo.environment["MACMESS_FAKE"] == "1"

@main
struct MacMessApp: App {
    @NSApplicationDelegateAdaptor(AppDelegate.self) private var appDelegate
    @State private var state: AppState
    @State private var flow: UIFlow
    /// Tự cập nhật qua Sparkle. Chỉ tạo khi chạy từ .app đã phát hành (có khóa công khai, không phải bản dev/fake).
    private let updaterController: SPUStandardUpdaterController? = Self.makeUpdater()

    private static func makeUpdater() -> SPUStandardUpdaterController? {
        let info = Bundle.main.infoDictionary
        let key = info?["SUPublicEDKey"] as? String ?? ""
        let build = info?["CFBundleVersion"] as? String ?? ""
        guard !isFakeMode,
              Bundle.main.bundleURL.pathExtension == "app",
              !key.isEmpty, !key.contains("REPLACE_WITH"),
              build != "1"
        else { return nil }
        return SPUStandardUpdaterController(startingUpdater: true, updaterDelegate: nil, userDriverDelegate: nil)
    }

    init() {
        let appState: AppState
        if isFakeMode {
            // Dữ liệu fake tách hẳn: thư mục tạm, Keychain service riêng, lastUserID riêng.
            appState = AppState(
                session: SessionStore(service: "dev.hieund.macmess.fake"),
                backend: "fake",
                defaults: UserDefaults(suiteName: "dev.hieund.macmess.fake") ?? .standard,
                dataDir: URL(fileURLWithPath: NSTemporaryDirectory()).appendingPathComponent("MacMess-fake"))
        } else {
            appState = AppState(backend: "meta")
        }
        let uiFlow = UIFlow()
        _state = State(initialValue: appState)
        _flow = State(initialValue: uiFlow)
        Notifier.shared.start(state: appState, flow: uiFlow)
        Lifecycle.shared.onMainWindowClosed = { [weak uiFlow] in uiFlow?.selectedConversationID = nil }
        Lifecycle.shared.start(state: appState)
    }

    var body: some Scene {
        Window("MacMess", id: Lifecycle.windowID) {
            RootView()
                .onAppear {
                    // Chạy binary trực tiếp thì cửa sổ nằm sau terminal: ép lên foreground.
                    NSApp.setActivationPolicy(.regular)
                    NSApp.activate()
                }
                .environment(state)
                .environment(flow)
                .frame(minWidth: 720, minHeight: 480)
        }
        Settings {
            SettingsView()
                .environment(state)
                .environment(flow)
        }
        .commands {
            CommandGroup(after: .appInfo) {
                CheckForUpdatesButton(updater: updaterController?.updater)
            }
        }
    }
}

/// Menu "Kiểm tra cập nhật…": disabled khi không có updater hoặc updater chưa sẵn sàng.
struct CheckForUpdatesButton: View {
    let updater: SPUUpdater?
    @State private var canCheck = false

    var body: some View {
        Button("Kiểm tra cập nhật…") { updater?.checkForUpdates() }
            .disabled(updater == nil || !canCheck)
            .onReceive(updater?.publisher(for: \.canCheckForUpdates).eraseToAnyPublisher()
                       ?? Empty<Bool, Never>().eraseToAnyPublisher()) { canCheck = $0 }
    }
}

struct RootView: View {
    @Environment(AppState.self) private var state
    @Environment(UIFlow.self) private var flow
    @Environment(\.openWindow) private var openWindow

    var body: some View {
        Group {
            switch flow.step {
            case .risk: RiskWarningView()
            case .resuming: ResumingView()
            case .picker: BrowserPickerView(onDone: { flow.step = .main })
            case .main: MainView()
            }
        }
        .background(WindowAccessor())
        .onAppear { Lifecycle.shared.register { openWindow(id: Lifecycle.windowID) } }
        .task(id: flow.step == .resuming) {
            guard flow.step == .resuming else { return }
            let outcome = await state.resumeSavedSession()
            // Có thể người dùng đã bấm "Chọn trình duyệt khác" trong lúc chờ.
            guard flow.step == .resuming else { return }
            flow.step = outcome == .resumed ? .main : .picker
        }
        .onChange(of: state.historyRestore.state) { _, new in
            if new == "needed", flow.step == .main, !flow.pinDismissed { flow.showPIN = true }
            if new == "done" { flow.showPIN = false }
        }
        // Mất phiên (đăng xuất, đăng nhập thất bại) → về chọn trình duyệt; auth_required thì giữ màn chính + AuthBanner.
        .onChange(of: state.isLoggedIn) { _, loggedIn in
            if !loggedIn { flow.pinDismissed = false; flow.showPIN = false }
            if !loggedIn, flow.step == .main, !state.isLoggingIn, !state.connection.isAuthRequired { flow.step = .picker }
        }
        .onChange(of: state.isLoggingIn) { _, busy in
            if !busy, flow.step == .main, !state.isLoggedIn, !state.connection.isAuthRequired { flow.step = .picker }
        }
    }
}

struct ResumingView: View {
    @Environment(UIFlow.self) private var flow
    @State private var slow = false

    var body: some View {
        VStack(spacing: 12) {
            ProgressView()
            Text("Đang kết nối…").foregroundStyle(.secondary)
            if slow {
                Button("Chọn trình duyệt khác") { flow.step = .picker }
            }
        }
        .task {
            try? await Task.sleep(for: .seconds(10))
            slow = true
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity)
    }
}

struct MainView: View {
    @Environment(AppState.self) private var state
    @Environment(UIFlow.self) private var flow

    var body: some View {
        @Bindable var flow = flow
        NavigationSplitView {
            SidebarView()
                .navigationSplitViewColumnWidth(min: 240, ideal: 300, max: 400)
        } detail: {
            VStack(spacing: 0) {
                if state.connection.isAuthRequired { AuthBanner() }
                if let id = flow.selectedConversationID, let c = state.conversations.first(where: { $0.id == id }) {
                    ChatView(conversation: c).id(id)
                } else {
                    ContentUnavailableView("Chọn một cuộc trò chuyện", systemImage: "bubble.left.and.bubble.right")
                }
            }
        }
        .sheet(isPresented: $flow.showPicker) {
            BrowserPickerView(onDone: { flow.showPicker = false }, onCancel: { flow.showPicker = false })
        }
        .sheet(isPresented: $flow.showPIN) {
            PINEntryView(onSkip: { flow.pinDismissed = true; flow.showPIN = false })
        }
        .onAppear {
            if state.historyRestore.state == "needed", !flow.pinDismissed { flow.showPIN = true }
        }
    }
}
