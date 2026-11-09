import CoreServices
import Foundation

private let maxNameBytes = 200

/// Cắt chuỗi theo số byte UTF-8, đúng ranh giới ký tự.
func truncateUTF8(_ s: String, maxBytes: Int) -> String {
    var out = ""
    var used = 0
    for ch in s {
        let n = String(ch).utf8.count
        if used + n > maxBytes { break }
        out.append(ch)
        used += n
    }
    return out
}

/// Tên file an toàn từ tên do người gửi cung cấp (không tin tên này): chỉ lấy tên cuối, thay `/` `:`,
/// ký tự điều khiển và Zl/Zp/Cf (vd U+202E đảo chiều) bằng `_`, bỏ dấu chấm đầu, rỗng/`.`/`..` → UUID,
/// tối đa 200 byte UTF-8 (giữ phần mở rộng ngắn).
public func safeDownloadName(_ filename: String?) -> String {
    var name = ((filename ?? "") as NSString).lastPathComponent
    name = String(String.UnicodeScalarView(name.unicodeScalars.map { s -> Unicode.Scalar in
        switch s.properties.generalCategory {
        case .control, .lineSeparator, .paragraphSeparator, .format: return "_"
        default: return (s == "/" || s == ":") ? "_" : s
        }
    }))
    while name.hasPrefix(".") { name.removeFirst() }
    if name.isEmpty || name == "." || name == ".." { return UUID().uuidString }
    if name.utf8.count > maxNameBytes {
        let ext = (name as NSString).pathExtension
        if !ext.isEmpty, ext.utf8.count <= 20 {
            let base = (name as NSString).deletingPathExtension
            name = truncateUTF8(base, maxBytes: maxNameBytes - ext.utf8.count - 1) + "." + ext
        } else {
            name = truncateUTF8(name, maxBytes: maxNameBytes)
        }
    }
    return name
}

/// "a.txt" → "a (2).txt"; index 0 giữ nguyên tên. Phần gốc được cắt để tên vẫn ≤ 200 byte.
func numberedName(_ name: String, index: Int) -> String {
    guard index > 0 else { return name }
    let ext = (name as NSString).pathExtension
    let base = (name as NSString).deletingPathExtension
    let suffix = " (\(index))" + (ext.isEmpty ? "" : "." + ext)
    return truncateUTF8(base, maxBytes: maxNameBytes - suffix.utf8.count) + suffix
}

/// Chuyển `source` vào `directory` với tên an toàn. Không kiểm tồn tại trước (tránh race): `moveItem` lỗi vì trùng
/// thì thử " (1)", " (2)"… (tối đa 100 lần).
public func moveWithUniqueName(from source: URL, toDirectory directory: URL, filename: String?) throws -> URL {
    let name = safeDownloadName(filename)
    var lastError: Error = CocoaError(.fileWriteFileExists)
    for i in 0...100 {
        let dest = directory.appendingPathComponent(numberedName(name, index: i))
        do {
            try FileManager.default.moveItem(at: source, to: dest)
            return dest
        } catch let e as CocoaError where e.code == .fileWriteFileExists {
            lastError = e
        }
    }
    throw lastError
}

/// Đặt cờ quarantine để Gatekeeper kiểm khi người dùng mở file tải về.
public func markQuarantined(_ url: URL) {
    var u = url
    var values = URLResourceValues()
    values.quarantineProperties = [
        kLSQuarantineTypeKey as String: kLSQuarantineTypeOtherDownload as String,
        kLSQuarantineAgentNameKey as String: "MacMess",
    ]
    try? u.setResourceValues(values)
}
