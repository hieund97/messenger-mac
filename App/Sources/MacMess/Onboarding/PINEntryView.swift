import AppCore
import SwiftUI

struct PINEntryView: View {
    @Environment(AppState.self) private var state
    var onSkip: () -> Void
    @Environment(\.dismiss) private var dismiss
    @State private var digits = Array(repeating: "", count: 6)
    @State private var hasSubmitted = false
    @FocusState private var focus: Int?

    private var pin: String { digits.joined() }

    var body: some View {
        VStack(spacing: 16) {
            Text("Khôi phục lịch sử tin nhắn mã hóa").font(.title3.bold())
            Text("Nhập mã PIN 6 số bạn đã đặt cho tin nhắn mã hóa đầu cuối.")
                .font(.callout).foregroundStyle(.secondary)
            HStack(spacing: 8) {
                ForEach(0..<6, id: \.self) { i in
                    SecureField("", text: Binding(get: { digits[i] }, set: { handle(i, $0) }))
                        .multilineTextAlignment(.center)
                        .frame(width: 36, height: 44)
                        .textFieldStyle(.roundedBorder)
                        .font(.title2.monospacedDigit())
                        .focused($focus, equals: i)
                        .disabled(state.historyRestore.state == "in_progress")
                }
            }
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
        .onAppear { focus = 0 }
        .onDisappear { digits = Array(repeating: "", count: 6) }
        .onChange(of: state.historyRestore.state) { _, new in
            if new == "done" { dismiss() }
            if new == "failed" {
                digits = Array(repeating: "", count: 6)
                focus = 0
            }
        }
    }

    private func handle(_ i: Int, _ new: String) {
        var filtered = new.filter { $0 >= "0" && $0 <= "9" }
        if filtered.count == 2, !digits[i].isEmpty, filtered.hasPrefix(digits[i]) {
            filtered = String(filtered.suffix(1))
        }
        if filtered.count > 1 {
            // Dán đủ sáu số ở bất kỳ ô nào cũng điền toàn bộ PIN.
            let start = filtered.count >= 6 ? 0 : i
            for (k, ch) in filtered.prefix(6 - start).enumerated() { digits[start + k] = String(ch) }
            focus = min(start + filtered.count, 5)
        } else {
            digits[i] = filtered
            if !filtered.isEmpty, i < 5 { focus = i + 1 }
            if filtered.isEmpty, i > 0 { focus = i - 1 }
        }
    }

    private func submit() {
        guard pin.count == 6 else { return }
        hasSubmitted = true
        state.restoreHistory(pin: pin)
        digits = Array(repeating: "", count: 6)
        focus = 0
    }
}
