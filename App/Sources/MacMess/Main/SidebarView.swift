import AppCore
import CoreBridge
import SwiftUI

extension String {
    /// Bỏ dấu, không phân biệt hoa thường (kể cả đ/Đ) để lọc tên.
    var searchKey: String {
        folding(options: [.diacriticInsensitive, .caseInsensitive], locale: nil)
            .replacingOccurrences(of: "đ", with: "d")
    }
}

struct SidebarView: View {
    @Environment(AppState.self) private var state
    @Environment(UIFlow.self) private var flow
    @State private var query = ""

    private var filtered: [Conversation] {
        let key = query.trimmingCharacters(in: .whitespaces).searchKey
        guard !key.isEmpty else { return state.conversations }
        return state.conversations.filter { $0.name.searchKey.contains(key) }
    }

    var body: some View {
        VStack(spacing: 0) {
            ConnectionBar()
            Text("Đoạn chat").font(.system(size: 22, weight: .bold))
                .frame(maxWidth: .infinity, alignment: .leading)
                .padding(.horizontal, 16).padding(.top, 8).padding(.bottom, 8)
            HStack(spacing: 6) {
                Image(systemName: "magnifyingglass").foregroundStyle(.secondary)
                TextField("Tìm kiếm", text: $query).textFieldStyle(.plain)
                if !query.isEmpty {
                    Button { query = "" } label: { Image(systemName: "xmark.circle.fill").foregroundStyle(.secondary) }
                        .buttonStyle(.plain)
                }
            }
            .padding(.horizontal, 10).padding(.vertical, 7)
            .background(Capsule().fill(Theme.gray))
            .padding(.horizontal, 12).padding(.bottom, 8)
            ScrollView {
                LazyVStack(spacing: 2) {
                    ForEach(filtered, id: \.id) { c in
                        Button { flow.selectedConversationID = c.id } label: {
                            ConversationRow(conversation: c, selected: flow.selectedConversationID == c.id)
                        }
                        .buttonStyle(.plain)
                    }
                    if state.isLoadingConversations { ProgressView().controlSize(.small).padding(8) }
                    if state.conversationsError != nil {
                        Button("Không tải được — Thử lại") { state.loadMoreConversations(retry: true) }
                            .padding(8)
                    } else if state.hasMoreConversations {
                        Color.clear.frame(height: 1)
                            .task(id: "\(state.conversations.count)-\(state.isLoadingConversations)-\(state.connection.state)") {
                                if !state.isLoadingConversations, state.connection.state == "connected" {
                                    state.loadMoreConversations()
                                }
                            }
                    }
                }
                .padding(.horizontal, 6)
            }
            .overlay {
                if filtered.isEmpty, state.conversationsError == nil {
                    ContentUnavailableView(query.isEmpty ? "Chưa có cuộc trò chuyện" : "Không tìm thấy",
                                           systemImage: query.isEmpty ? "tray" : "magnifyingglass")
                }
            }
        }
    }
}

struct ConnectionBar: View {
    @Environment(AppState.self) private var state

    var body: some View {
        let s = state.connection
        if s.state != "connected" {
            HStack(spacing: 6) {
                if s.state == "connecting" { ProgressView().controlSize(.mini) }
                else { Image(systemName: "wifi.slash").font(.caption) }
                Text(label(s)).font(.caption)
                Spacer()
            }
            .padding(.horizontal, 12).padding(.vertical, 5)
            .foregroundStyle(.secondary)
            .background(.bar)
        }
    }

    private func label(_ s: ConnectionState) -> String {
        switch s.state {
        case "connecting": return "Đang kết nối…"
        case "auth_required": return "Cần đăng nhập lại"
        default:
            if let r = s.reason, r == "network" || r == "rate_limited" { return friendlyCoreReason(r) }
            return "Mất kết nối"
        }
    }
}
