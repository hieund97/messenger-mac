import AppCore
import AppKit
import CoreBridge
import SwiftUI

/// Vị trí tin trong cụm (cùng người gửi, cách nhau ≤ 5 phút).
struct ClusterPosition {
    var hasPrevious: Bool
    var hasNext: Bool
}

private enum ChatRow: Identifiable {
    case time(id: String, label: String)
    case message(Message, ClusterPosition, topSpacing: CGFloat, isLast: Bool)

    var id: String {
        switch self {
        case .time(let id, _): id
        case .message(let m, _, _, _): "m-" + (m.clientID ?? m.id)
        }
    }
}

private struct ContentFrameKey: PreferenceKey {
    static let defaultValue: CGRect = .zero
    static func reduce(value: inout CGRect, nextValue: () -> CGRect) { value = nextValue() }
}

private func timeLabel(_ ms: Int64) -> String {
    let date = Date(timeIntervalSince1970: TimeInterval(ms) / 1000)
    let f = DateFormatter()
    f.locale = Locale(identifier: "vi_VN")
    f.dateFormat = Calendar.current.isDateInToday(date) ? "HH:mm" : "HH:mm, d/M/yyyy"
    return f.string(from: date)
}

private func buildRows(_ messages: [Message]) -> [ChatRow] {
    let cluster: Int64 = 5 * 60_000, separator: Int64 = 15 * 60_000
    func sameCluster(_ a: Message, _ b: Message) -> Bool {
        a.isOutgoing == b.isOutgoing && a.senderID == b.senderID && b.ts - a.ts <= cluster
    }
    var rows: [ChatRow] = []
    for (i, m) in messages.enumerated() {
        let prev = i > 0 ? messages[i - 1] : nil
        let next = i + 1 < messages.count ? messages[i + 1] : nil
        if prev == nil || m.ts - prev!.ts > separator {
            rows.append(.time(id: "t-" + (m.clientID ?? m.id), label: timeLabel(m.ts)))
        }
        let hasPrev = prev.map { sameCluster($0, m) && m.ts - $0.ts <= separator } ?? false
        let hasNext = next.map { sameCluster(m, $0) } ?? false
        rows.append(.message(m, ClusterPosition(hasPrevious: hasPrev, hasNext: hasNext),
                             topSpacing: prev == nil ? 0 : (hasPrev ? 2 : 10), isLast: next == nil))
    }
    return rows
}

struct ChatView: View {
    @Environment(AppState.self) private var state
    @Environment(UIFlow.self) private var flow
    @Environment(\.controlActiveState) private var activeState
    let conversation: Conversation
    @State private var opened = false
    @State private var dropTargeted = false
    @State private var viewportHeight: CGFloat = 600
    @State private var nearBottom = true
    @State private var atTop = false
    @State private var scrolledAway = false
    @State private var lastMarkedTS: Int64 = 0

    private var messages: [Message] { state.messages(for: conversation.id) }
    private var lastKey: String? { messages.last.map { $0.clientID ?? $0.id } }

    var body: some View {
        VStack(spacing: 0) {
            header
            Divider()
            if !conversation.historyRestored, ["needed", "failed"].contains(state.historyRestore.state) {
                HStack {
                    Image(systemName: "lock.fill")
                    Text("Chưa khôi phục lịch sử")
                    Spacer()
                    Button("Nhập PIN") { flow.pinDismissed = false; flow.showPIN = true }
                }
                .font(.callout).padding(8).background(Color.yellow.opacity(0.18))
            }
            messageList
            ComposerView(onSend: { state.send(text: $0, to: conversation.id) }, onFiles: sendFiles)
        }
        .overlay { if dropTargeted { RoundedRectangle(cornerRadius: 8).stroke(Theme.blue, lineWidth: 3).padding(4) } }
        .dropDestination(for: URL.self) { urls, _ in
            sendFiles(urls)
            return !urls.isEmpty
        } isTargeted: { dropTargeted = $0 }
        .navigationTitle(conversation.name)
        .task {
            await state.open(conversationID: conversation.id)
            opened = true
            markReadIfNeeded()
        }
        .onChange(of: activeState) { _, _ in markReadIfNeeded() }
        .onChange(of: messages.last?.ts) { _, _ in markReadIfNeeded() }
        .onReceive(NotificationCenter.default.publisher(for: NSApplication.didBecomeActiveNotification)) { _ in markReadIfNeeded() }
    }

    private var header: some View {
        HStack(spacing: 10) {
            AvatarView(name: conversation.name, url: conversation.avatarURL, size: 36)
            Text(conversation.name).font(.headline)
            Spacer()
        }
        .padding(.horizontal, 14).padding(.vertical, 8)
    }

    private var messageList: some View {
        let rows = buildRows(messages)
        return GeometryReader { outer in
            ScrollViewReader { proxy in
                ScrollView {
                    VStack(spacing: 0) {
                        ForEach(rows) { row in
                            switch row {
                            case .time(_, let label):
                                Text(label).font(.caption).foregroundStyle(.secondary)
                                    .frame(maxWidth: .infinity).padding(.vertical, 10)
                            case .message(let m, let pos, let top, let isLast):
                                MessageBubble(message: m, position: pos, isLastInChat: isLast, conversation: conversation,
                                              viewportHeight: outer.size.height)
                                    .padding(.top, top)
                            }
                        }
                    }
                    .padding(12)
                    .background(GeometryReader { g in
                        Color.clear.preference(key: ContentFrameKey.self, value: g.frame(in: .named("chatViewport")))
                    })
                }
                .coordinateSpace(name: "chatViewport")
                .defaultScrollAnchor(.bottom)
                .onPreferenceChange(ContentFrameKey.self) { frame in
                    handleScroll(frame, viewport: outer.size.height)
                }
                .onChange(of: lastKey) { old, new in
                    // Chỉ cuộn khi đang ở gần đáy hoặc tin của mình; id optimistic → id thật giữ nguyên key nên không cuộn.
                    guard let new, old != nil, let last = messages.last else { return }
                    if last.isOutgoing || nearBottom { withAnimation { proxy.scrollTo("m-" + new, anchor: .bottom) } }
                }
                .task(id: opened) {
                    if opened, let k = lastKey { proxy.scrollTo("m-" + k, anchor: .bottom) }
                }
            }
        }
    }

    private func handleScroll(_ frame: CGRect, viewport: CGFloat) {
        let near = frame.maxY - viewport < 120
        if near != nearBottom { nearBottom = near }
        if frame.minY < -60, !scrolledAway { scrolledAway = true }
        let top = frame.minY > -30
        if top != atTop {
            atTop = top
            // Chỉ nạp thêm khi người dùng thật sự cuộn lên tới đầu (nội dung ngắn thì không cuộn được nên không gọi).
            if top, scrolledAway, opened { state.loadOlder(conversationID: conversation.id) }
        }
    }

    private func sendFiles(_ urls: [URL]) {
        for u in urls where u.isFileURL { state.send(file: u, to: conversation.id) }
    }

    /// Đánh dấu đã đọc khi tin đến mới nhất mới hơn mốc đã mark, app đang active và cửa sổ là key.
    private func markReadIfNeeded() {
        guard opened, NSApp.isActive, activeState == .key, conversation.unreadCount > 0,
              let latest = messages.last(where: { !$0.isOutgoing }), latest.ts > lastMarkedTS else { return }
        lastMarkedTS = latest.ts
        state.markRead(conversationID: conversation.id)
    }
}
