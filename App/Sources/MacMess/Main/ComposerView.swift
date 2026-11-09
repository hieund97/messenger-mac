import AppKit
import SwiftUI
import UniformTypeIdentifiers

@MainActor
final class ComposerController {
    weak var coordinator: ComposerTextView.Coordinator?
    func submit() { coordinator?.submit() }
}

struct ComposerView: View {
    let onSend: (String) -> Void
    let onFiles: ([URL]) -> Void
    @State private var text = ""
    @State private var height: CGFloat = 36
    @State private var controller = ComposerController()

    private var hasText: Bool { !text.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty }

    var body: some View {
        HStack(alignment: .bottom, spacing: 8) {
            Menu {
                Button("Ảnh…") { pickFiles(images: true) }
                Button("File…") { pickFiles(images: false) }
            } label: {
                Image(systemName: "plus.circle.fill").font(.system(size: 24)).foregroundStyle(Theme.blue)
            }
            .menuStyle(.borderlessButton).menuIndicator(.hidden).fixedSize()
            .help("Đính kèm ảnh hoặc file")
            .frame(height: 36)

            ComposerTextView(text: $text, height: $height, placeholder: "Aa", controller: controller, onSubmit: onSend)
                .frame(height: height)
                .background(RoundedRectangle(cornerRadius: 18).fill(Theme.gray))

            Group {
                if hasText {
                    Button { controller.submit() } label: {
                        Image(systemName: "paperplane.fill").font(.system(size: 20)).foregroundStyle(Theme.blue)
                    }
                    .help("Gửi")
                } else {
                    Button { onSend("👍") } label: {
                        Text("👍").font(.system(size: 22))
                    }
                    .help("Gửi biểu tượng thích")
                }
            }
            .buttonStyle(.plain).frame(width: 28, height: 36)
        }
        .padding(.horizontal, 12).padding(.vertical, 8)
    }

    private func pickFiles(images: Bool) {
        let panel = NSOpenPanel()
        panel.allowsMultipleSelection = true
        panel.canChooseDirectories = false
        if images { panel.allowedContentTypes = [.image] }
        if panel.runModal() == .OK { onFiles(panel.urls) }
    }
}

/// NSTextView có placeholder tự vẽ. Enter gửi; Shift/Option+Enter xuống dòng; đang gõ dở bằng IME
/// (marked text) thì Enter chỉ chốt chữ, không gửi, không xuống dòng.
final class PlaceholderTextView: NSTextView {
    var placeholder = ""

    override func draw(_ dirtyRect: NSRect) {
        super.draw(dirtyRect)
        guard string.isEmpty, !hasMarkedText() else { return }
        let attrs: [NSAttributedString.Key: Any] = [.font: font ?? .systemFont(ofSize: 13), .foregroundColor: NSColor.placeholderTextColor]
        placeholder.draw(at: NSPoint(x: textContainerInset.width + (textContainer?.lineFragmentPadding ?? 0), y: textContainerInset.height), withAttributes: attrs)
    }
}

struct ComposerTextView: NSViewRepresentable {
    @Binding var text: String
    @Binding var height: CGFloat
    let placeholder: String
    let controller: ComposerController
    let onSubmit: (String) -> Void

    static let maxLines = 6

    func makeCoordinator() -> Coordinator { Coordinator(self) }

    func makeNSView(context: Context) -> NSScrollView {
        let scroll = NSScrollView()
        scroll.drawsBackground = false
        scroll.hasVerticalScroller = false
        scroll.borderType = .noBorder
        let tv = PlaceholderTextView()
        tv.placeholder = placeholder
        tv.delegate = context.coordinator
        tv.isRichText = false
        tv.allowsUndo = true
        tv.drawsBackground = false
        tv.font = .systemFont(ofSize: NSFont.systemFontSize)
        tv.textColor = .textColor
        tv.textContainerInset = NSSize(width: 8, height: 9)
        tv.isVerticallyResizable = true
        tv.isHorizontallyResizable = false
        tv.autoresizingMask = [.width]
        tv.textContainer?.widthTracksTextView = true
        tv.postsFrameChangedNotifications = true
        NotificationCenter.default.addObserver(context.coordinator, selector: #selector(Coordinator.frameChanged),
                                               name: NSView.frameDidChangeNotification, object: tv)
        scroll.documentView = tv
        context.coordinator.textView = tv
        controller.coordinator = context.coordinator
        Task { @MainActor in context.coordinator.updateHeight() }
        return scroll
    }

    func updateNSView(_ scroll: NSScrollView, context: Context) {
        context.coordinator.parent = self
        guard let tv = context.coordinator.textView else { return }
        if tv.string != text, !tv.hasMarkedText() {
            tv.string = text
            tv.needsDisplay = true
            Task { @MainActor in context.coordinator.updateHeight() }  // không đổi binding giữa lúc cập nhật view
        }
    }

    @MainActor final class Coordinator: NSObject, NSTextViewDelegate {
        var parent: ComposerTextView
        weak var textView: NSTextView?
        init(_ parent: ComposerTextView) { self.parent = parent }

        func textDidChange(_ notification: Notification) {
            guard let tv = textView else { return }
            parent.text = tv.string
            tv.needsDisplay = true
            updateHeight()
        }

        @objc func frameChanged() { updateHeight() }

        /// Chốt chữ IME, đọc text, xóa ô nhập rồi gửi.
        func submit() {
            guard let tv = textView else { return }
            tv.unmarkText()
            tv.inputContext?.discardMarkedText()
            let t = tv.string.trimmingCharacters(in: .whitespacesAndNewlines)
            guard !t.isEmpty else { return }
            tv.string = ""
            parent.text = ""
            tv.needsDisplay = true
            updateHeight()
            parent.onSubmit(t)
        }

        func textView(_ textView: NSTextView, doCommandBy selector: Selector) -> Bool {
            guard selector == #selector(NSResponder.insertNewline(_:)) else { return false }
            if textView.hasMarkedText() {
                textView.unmarkText()  // chốt chữ, không gửi, không xuống dòng
                return true
            }
            let mods = NSApp.currentEvent?.modifierFlags ?? []
            if mods.contains(.shift) || mods.contains(.option) {
                textView.insertNewlineIgnoringFieldEditor(nil)
                return true
            }
            submit()
            return true
        }

        func updateHeight() {
            guard let tv = textView, let lm = tv.layoutManager, let tc = tv.textContainer else { return }
            lm.ensureLayout(for: tc)
            let inset = tv.textContainerInset.height * 2
            let line = lm.defaultLineHeight(for: tv.font ?? .systemFont(ofSize: 13))
            let content = lm.usedRect(for: tc).height + inset
            let h = min(max(content, line + inset), line * CGFloat(ComposerTextView.maxLines) + inset)
            if abs(parent.height - h) > 0.5 { parent.height = h }
            tv.enclosingScrollView?.hasVerticalScroller = content > h + 0.5
        }
    }
}
