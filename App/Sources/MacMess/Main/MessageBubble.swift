import AppCore
import AppKit
import CoreBridge
import QuickLook
import SwiftUI

struct MessageBubble: View {
    @Environment(AppState.self) private var state
    let message: Message
    let position: ClusterPosition
    let isLastInChat: Bool
    let conversation: Conversation
    let viewportHeight: CGFloat
    @State private var quickLookURL: URL?
    @State private var busy = false
    @State private var downloadError: String?

    private var out: Bool { message.isOutgoing }
    private let big: CGFloat = 18, small: CGFloat = 4

    /// Góc phía người gửi ở giữa cụm bo 4pt, đầu/cuối cụm bo đầy đủ.
    private var shape: UnevenRoundedRectangle {
        let top = position.hasPrevious ? small : big
        let bottom = position.hasNext ? small : big
        return out
            ? UnevenRoundedRectangle(topLeadingRadius: big, bottomLeadingRadius: big, bottomTrailingRadius: bottom, topTrailingRadius: top)
            : UnevenRoundedRectangle(topLeadingRadius: top, bottomLeadingRadius: bottom, bottomTrailingRadius: big, topTrailingRadius: big)
    }

    private var imageOnly: Bool {
        (message.text ?? "").isEmpty && !message.attachments.isEmpty && message.attachments.allSatisfy { $0.kind == "image" }
    }

    var body: some View {
        HStack(alignment: .bottom, spacing: 6) {
            if out {
                Spacer(minLength: 60)
            } else {
                // Avatar 28pt chỉ ở tin cuối cụm; các tin khác chừa chỗ cho thẳng hàng.
                if position.hasNext { Color.clear.frame(width: 28, height: 28) }
                else { AvatarView(name: conversation.name, url: conversation.avatarURL, size: 28) }
            }
            VStack(alignment: out ? .trailing : .leading, spacing: 3) {
                bubble
                statusView
            }
            if !out { Spacer(minLength: 60) }
        }
        .quickLookPreview($quickLookURL)
    }

    @ViewBuilder private var bubble: some View {
        VStack(alignment: .leading, spacing: 6) {
            ForEach(Array(message.attachments.enumerated()), id: \.offset) { _, a in attachmentView(a) }
            if let t = message.text, !t.isEmpty { Text(t).textSelection(.enabled) }
        }
        .padding(.horizontal, imageOnly ? 0 : 12).padding(.vertical, imageOnly ? 0 : 8)
        .foregroundStyle(out ? Color.white : Color.primary)
        .background {
            if !imageOnly { bubbleFill }
        }
        .clipShape(shape)
    }

    /// Bubble của mình: gradient xanh → tím cố định theo khung nhìn (như Messenger), không theo từng bubble.
    @ViewBuilder private var bubbleFill: some View {
        if out {
            GeometryReader { g in
                let y = g.frame(in: .named("chatViewport")).minY
                Theme.outgoing
                    .frame(width: g.size.width, height: max(viewportHeight, 1))
                    .offset(y: -y)
            }
        } else {
            Theme.gray
        }
    }

    // MARK: Trạng thái

    @ViewBuilder private var statusView: some View {
        if out {
            switch message.status {
            case .sending:
                if isLastInChat { Text("Đang gửi").font(.caption2).foregroundStyle(.secondary) }
            case .sent:
                if isLastInChat { Text("Đã gửi").font(.caption2).foregroundStyle(.secondary) }
            case .unknown:
                failureLine("Không chắc đã gửi", reason: nil)
            case .failed:
                failureLine("Gửi lỗi", reason: failureReason)
            }
        }
        if let downloadError {
            Text(downloadError).font(.caption2).foregroundStyle(.red)
        }
    }

    private func failureLine(_ title: String, reason: String?) -> some View {
        HStack(spacing: 6) {
            Text([title, reason].compactMap { $0 }.joined(separator: " – ")).font(.caption2).foregroundStyle(.red)
            Button("Gửi lại") { if let id = message.clientID { state.retry(clientID: id) } }
                .buttonStyle(.link).font(.caption2)
        }
    }

    private var failureReason: String? {
        guard let a = message.attachments.first,
              let p = AppState.fileProblem(URL(fileURLWithPath: a.ref)) else { return nil }
        return p.code == "file_too_large" ? "file quá 25 MB" : "không đọc được file"
    }

    // MARK: Attachment

    private func localPath(_ a: Attachment) -> String? {
        a.ref.hasPrefix("/") ? a.ref : nil
    }

    @ViewBuilder private func attachmentView(_ a: Attachment) -> some View {
        if a.kind == "image" {
            Button { openAttachment(a, quickLook: true) } label: {
                AsyncThumb(path: localPath(a), url: a.previewURL, maxPixel: 600) {
                    ZStack { Rectangle().fill(.quaternary); ProgressView().controlSize(.small) }
                }
                .frame(width: thumbSize(a).width, height: thumbSize(a).height)
                .clipShape(RoundedRectangle(cornerRadius: 12))
            }
            .buttonStyle(.plain)
        } else {
            Button { openAttachment(a, quickLook: false) } label: {
                HStack {
                    if busy { ProgressView().controlSize(.small) } else { Image(systemName: "doc.fill") }
                    VStack(alignment: .leading, spacing: 0) {
                        Text(a.filename ?? "Tệp đính kèm").lineLimit(1)
                        if let s = a.size { Text(ByteCountFormatter.string(fromByteCount: s, countStyle: .file)).font(.caption).opacity(0.7) }
                    }
                }
            }
            .buttonStyle(.plain)
        }
    }

    /// Khung cố định theo tỉ lệ ảnh (nếu biết) để layout không nhảy khi ảnh nạp xong.
    private func thumbSize(_ a: Attachment) -> CGSize {
        guard let w = a.width, let h = a.height, w > 0, h > 0 else { return CGSize(width: 220, height: 160) }
        let scale = min(240 / CGFloat(w), 240 / CGFloat(h), 1)
        return CGSize(width: max(CGFloat(w) * scale, 60), height: max(CGFloat(h) * scale, 60))
    }

    private func openAttachment(_ a: Attachment, quickLook: Bool) {
        guard !busy else { return }
        downloadError = nil
        // File local của tin mình gửi: không cần tải.
        if let p = localPath(a), FileManager.default.fileExists(atPath: p) {
            reveal(URL(fileURLWithPath: p), quickLook: quickLook)
            return
        }
        busy = true
        Task {
            defer { busy = false }
            let dir = quickLook ? AppState.previewDirectory
                                : (FileManager.default.urls(for: .downloadsDirectory, in: .userDomainMask).first ?? FileManager.default.temporaryDirectory)
            switch await state.downloadMedia(a, into: dir) {
            case .success(let url): reveal(url, quickLook: quickLook)
            case .failure(.inProgress): break
            case .failure(.failed): downloadError = "Tải xuống lỗi"
            }
        }
    }

    /// Ảnh: Quick Look. File khác: chỉ hiện trong Finder (không tự mở vì có thể là .app/.command/script).
    private func reveal(_ url: URL, quickLook: Bool) {
        if quickLook { quickLookURL = url } else { NSWorkspace.shared.activateFileViewerSelecting([url]) }
    }
}
