import AppCore
import CoreBridge
import SwiftUI

struct AvatarView: View {
    let name: String
    let url: String?
    var size: CGFloat = 40

    var body: some View {
        ZStack {
            Circle().fill(Theme.gray)
            Text(String(name.trimmingCharacters(in: .whitespaces).first.map { String($0).uppercased() } ?? "?"))
                .font(.system(size: size * 0.42, weight: .medium)).foregroundStyle(.secondary)
            if let url {
                AsyncThumb(path: nil, url: url, maxPixel: Int(size * 2)) { Color.clear }
            }
        }
        .frame(width: size, height: size)
        .clipShape(Circle())
    }
}

/// "Vừa xong", "5 phút", "2 giờ", "T2" (trong 7 ngày), "d/M".
func shortTime(_ tsMillis: Int64, now: Date = .now) -> String {
    guard tsMillis > 0 else { return "" }
    let date = Date(timeIntervalSince1970: TimeInterval(tsMillis) / 1000)
    let secs = now.timeIntervalSince(date)
    if secs < 60 { return "Vừa xong" }
    if secs < 3600 { return "\(Int(secs / 60)) phút" }
    if secs < 86400 { return "\(Int(secs / 3600)) giờ" }
    let cal = Calendar.current
    if secs < 7 * 86400 {
        let wd = cal.component(.weekday, from: date)  // 1 = Chủ nhật
        return wd == 1 ? "CN" : "T\(wd)"
    }
    return "\(cal.component(.day, from: date))/\(cal.component(.month, from: date))"
}

struct ConversationRow: View {
    @Environment(AppState.self) private var state
    let conversation: Conversation
    let selected: Bool

    /// Tin cuối của mình mới hơn thông tin hội thoại thì dùng nội dung tin đó và thêm "Bạn: ".
    private var preview: String {
        if let last = state.messages(for: conversation.id).last, last.isOutgoing, last.ts >= conversation.lastActivityTS {
            return "Bạn: " + (last.text ?? "Đã gửi tệp đính kèm")
        }
        return conversation.lastMessagePreview ?? ""
    }

    var body: some View {
        let unread = conversation.unreadCount > 0
        HStack(spacing: 10) {
            AvatarView(name: conversation.name, url: conversation.avatarURL, size: 48)
            VStack(alignment: .leading, spacing: 2) {
                Text(conversation.name).fontWeight(unread ? .bold : .regular).lineLimit(1)
                HStack(spacing: 4) {
                    Text(preview).lineLimit(1)
                    Text("· " + shortTime(max(conversation.lastActivityTS, state.messages(for: conversation.id).last?.ts ?? 0)))
                        .layoutPriority(1)
                }
                .font(.callout)
                .fontWeight(unread ? .bold : .regular)
                .foregroundStyle(unread ? Color.primary : Color.secondary)
            }
            Spacer(minLength: 4)
            if unread { Circle().fill(Theme.blue).frame(width: 12, height: 12) }
        }
        .padding(.horizontal, 8).padding(.vertical, 6)
        .background(RoundedRectangle(cornerRadius: 10).fill(selected ? Theme.gray : Color.clear))
        .contentShape(Rectangle())
    }
}
