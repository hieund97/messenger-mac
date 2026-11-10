import AppCore
import SwiftUI

struct PINEntryView: View {
    @Environment(AppState.self) private var state
    var onSkip: () -> Void
    @Environment(\.dismiss) private var dismiss
    @State private var pin = ""
    @State private var hasSubmitted = false
    @FocusState private var focused: Bool

    private var inProgress: Bool { state.historyRestore.state == "in_progress" }

    var body: some View {
        VStack(spacing: 16) {
            Text("Khôi phục lịch sử tin nhắn mã hóa").font(.title3.bold())
            Text("Nhập mã PIN 6 số bạn đã đặt cho tin nhắn mã hóa đầu cuối.")
                .font(.callout).foregroundStyle(.secondary)
            pinBoxes
            if hasSubmitted, state.historyRestore.state == "failed" {
                Text(state.historyRestore.failureMessage).foregroundStyle(.red).font(.callout)
            }
            if state.historyRestore.state == "in_progress" {
                ProgressView().controlSize(.small)
            }
            HStack {
                Button("Để sau") { state.skipHistoryRestore(); onSkip() }
                    .disabled(state.historyRestore.state == "in_progress")
                Button("Khôi phục") { submit() }
                    .keyboardShortcut(.defaultAction)
                    .disabled(pin.count != 6 || state.historyRestore.state == "in_progress")
            }
        }
        .padding(28)
        .frame(minWidth: 400)
        .interactiveDismissDisabled()
        .onAppear { focused = true }
        .onDisappear { pin = "" }
        .onChange(of: state.historyRestore.state) { _, new in
            if new == "done" { dismiss() }
            if new == "failed" {
                pin = ""
                focused = true
            }
        }
    }

    // Một SecureField ẩn nhận toàn bộ phím gõ/dán; 6 ô chỉ để hiển thị nên không có đổi focus giữa các field.
    private var pinBoxes: some View {
        ZStack {
            SecureField("", text: Binding(get: { pin }, set: { handle($0) }))
                .focused($focused)
                .textFieldStyle(.plain)
                .focusEffectDisabled()
                .opacity(0)
                .allowsHitTesting(false)
                .disabled(inProgress)
            HStack(spacing: 8) {
                ForEach(0..<6, id: \.self) { i in
                    let active = focused && i == min(pin.count, 5)
                    RoundedRectangle(cornerRadius: 6)
                        .fill(Color(nsColor: .textBackgroundColor))
                        .overlay(
                            RoundedRectangle(cornerRadius: 6)
                                .stroke(active ? Color.accentColor : Color.secondary.opacity(0.4),
                                        lineWidth: active ? 2 : 1)
                        )
                        .overlay { if i < pin.count { Circle().fill(.primary).frame(width: 10, height: 10) } }
                        .frame(width: 36, height: 44)
                }
            }
            .contentShape(Rectangle())
            .onTapGesture { if !inProgress { focused = true } }
        }
    }

    private func handle(_ new: String) {
        var filtered = String(new.filter { $0 >= "0" && $0 <= "9" })
        // Đủ 6 số mà gõ thêm 1 số: thay số cuối bằng số mới.
        if pin.count == 6, filtered.count == 7, filtered.hasPrefix(pin) {
            filtered = String(pin.prefix(5)) + String(filtered.suffix(1))
        }
        // Dán nguyên 6 số khi ô đang có số: lấy đúng chuỗi vừa dán.
        if filtered.count > 6, filtered.count - pin.count >= 6 { filtered = String(filtered.suffix(6)) }
        pin = String(filtered.prefix(6))
    }

    private func submit() {
        guard pin.count == 6 else { return }
        hasSubmitted = true
        state.restoreHistory(pin: pin)
        pin = ""
        focused = true
    }
}
