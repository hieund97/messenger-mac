import AppCore
import SwiftUI

struct AuthBanner: View {
    @Environment(AppState.self) private var state
    @Environment(UIFlow.self) private var flow

    var body: some View {
        HStack(spacing: 10) {
            Image(systemName: "exclamationmark.triangle.fill").foregroundStyle(.orange)
            VStack(alignment: .leading, spacing: 2) {
                Text("Phiên đăng nhập đã hết hạn. Mở messenger.com trên trình duyệt rồi bấm Đăng nhập lại.")
                    .font(.callout)
                if let reason = state.connection.authRequiredReason, !reason.isEmpty {
                    Text(friendlyCoreReason(reason)).font(.caption).foregroundStyle(.secondary)
                }
            }
            Spacer()
            Button("Đăng nhập lại") { flow.showPicker = true }
        }
        .padding(10)
        .background(Color.orange.opacity(0.18))
    }
}
