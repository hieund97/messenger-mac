import AppCore
import SwiftUI

struct PINEntryView: View {
    @Environment(AppState.self) private var state
    var onSkip: () -> Void
    @Environment(\.dismiss) private var dismiss
    @State private var digits = Array(repeating: "", count: 6)
    @FocusState private var focus: Int?

    private var pin: String { digits.joined() }

    var body: some View {
        VStack(spacing: 16) {
            Text("Khôi phục lịch sử tin nhắn mã hóa").font(.title3.bold())
            Text("Nhập mã PIN 6 số bạn đã đặt cho tin nhắn mã hóa đầu cuối.")
                .font(.callout).foregroundStyle(.secondary)
            HStack(spacing: 8) {
                ForEach(0..<6, id: \.self) { i in
                    TextField("", text: $digits[i])
                        .multilineTextAlignment(.center)
                        .frame(width: 36, height: 44)
                        .textFieldStyle(.roundedBorder)
                        .font(.title2.monospacedDigit())
                        .focused($focus, equals: i)
                        .onChange(of: digits[i]) { _, new in handle(i, new) }
                }
            }
            if state.historyRestore.state == "failed" {
                Text("PIN không đúng, hãy nhập lại.").foregroundStyle(.red).font(.callout)
            }
            if state.historyRestore.state == "in_progress" {
                ProgressView().controlSize(.small)
            }
            HStack {
                Button("Bỏ qua") { state.skipHistoryRestore(); onSkip() }
                Button("Khôi phục") { submit() }
                    .keyboardShortcut(.defaultAction)
                    .disabled(pin.count != 6 || state.historyRestore.state == "in_progress")
            }
        }
        .padding(28)
        .frame(minWidth: 400)
        .onAppear { focus = 0 }
        .onChange(of: state.historyRestore.state) { _, new in if new == "done" { dismiss() } }
    }

    private func handle(_ i: Int, _ new: String) {
        let filtered = new.filter(\.isNumber)
        if filtered.count > 1 {
            // Dán nhiều số: rải vào các ô.
            for (k, ch) in filtered.prefix(6 - i).enumerated() { digits[i + k] = String(ch) }
            focus = min(i + filtered.count, 5)
            return
        }
        if filtered != new { digits[i] = filtered }
        if filtered.count == 1, i < 5 { focus = i + 1 }
    }

    private func submit() {
        guard pin.count == 6 else { return }
        state.submitPIN(pin)
        digits = Array(repeating: "", count: 6)
        focus = 0
    }
}
