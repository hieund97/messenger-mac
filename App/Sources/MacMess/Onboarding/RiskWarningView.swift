import SwiftUI

struct RiskWarningView: View {
    @Environment(UIFlow.self) private var flow

    var body: some View {
        RiskAcceptView(onAccept: { flow.acceptRisk() }, onCancel: nil)
    }
}

/// Nội dung cảnh báo rủi ro; dùng ở onboarding và khi chọn lại trình duyệt lúc chưa chấp nhận.
struct RiskAcceptView: View {
    let onAccept: () -> Void
    let onCancel: (() -> Void)?

    var body: some View {
        VStack(spacing: 20) {
            Image(systemName: "exclamationmark.triangle.fill")
                .font(.system(size: 44)).foregroundStyle(.orange)
            Text("Trước khi bắt đầu").font(.title.bold())
            VStack(alignment: .leading, spacing: 10) {
                Label("Đây là ứng dụng không chính thức, không liên kết với Meta.", systemImage: "info.circle")
                Label("Meta có thể hạn chế hoặc khóa tài khoản dùng ứng dụng bên thứ ba.", systemImage: "lock.trianglebadge.exclamationmark")
                Label("Hãy dùng tài khoản phụ thay vì tài khoản chính.", systemImage: "person.crop.circle.badge.questionmark")
            }
            .frame(maxWidth: 440, alignment: .leading)
            HStack {
                if let onCancel { Button("Hủy", action: onCancel) }
                Button("Tôi hiểu, tiếp tục", action: onAccept)
                    .keyboardShortcut(.defaultAction)
                    .controlSize(.large)
            }
        }
        .padding(40)
        .frame(maxWidth: .infinity, maxHeight: .infinity)
    }
}
