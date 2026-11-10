import Foundation
import GRDB

public enum ChromiumCookieReader {
    public struct Failure: Equatable, Sendable {
        public var domain: String
        public var name: String
    }

    public struct Result: Sendable {
        /// Đã bỏ cookie hết hạn và dedupe theo (site, name), giữ bản mới nhất.
        public var cookies: [Cookie]
        /// Cookie (bản mới nhất) không giải mã được; bị bỏ qua.
        public var failures: [Failure]
    }

    static let sites = ["messenger.com", "facebook.com"]
    private static let maxAttempts = 3
    private static let tempPrefix = "cookie-import-"

    /// Đọc cookie của messenger.com và facebook.com. `domain` giữ nguyên `host_key`.
    /// Copy `Cookies` cùng `-wal`/`-journal`/`-shm` ra thư mục tạm (0700), không mở file gốc.
    public static func read(_ profile: BrowserProfile, password: String, now: Date = Date()) throws -> Result {
        let decryptor = try ChromiumDecryptor(password: password)
        let fm = FileManager.default
        cleanStaleTempDirs(fm)
        try checkReadable(profile.cookiesPath)

        for _ in 0..<maxAttempts {
            let tmp = fm.temporaryDirectory.appendingPathComponent("\(tempPrefix)\(UUID().uuidString)", isDirectory: true)
            try fm.createDirectory(at: tmp, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
            defer { try? fm.removeItem(at: tmp) }

            let source = profile.cookiesPath
            let before = signature(of: source)
            let copy = tmp.appendingPathComponent("Cookies")
            do {
                try fm.copyItem(at: source, to: copy)
            } catch {
                if isPermissionError(error) { throw CookieImportError.permissionDenied }
                throw error
            }
            for suffix in ["-wal", "-journal", "-shm"] {
                let side = URL(fileURLWithPath: source.path + suffix)
                if fm.fileExists(atPath: side.path) {
                    try? fm.copyItem(at: side, to: URL(fileURLWithPath: copy.path + suffix))
                }
            }
            guard signature(of: source) == before else { continue }

            guard let queue = try? DatabaseQueue(path: copy.path) else { continue }
            defer { try? queue.close() }
            do {
                let healthy = try queue.read { try String.fetchOne($0, sql: "PRAGMA quick_check") == "ok" }
                guard healthy else { continue }
                return try queue.read { try query($0, decryptor: decryptor, now: now) }
            } catch {
                continue
            }
        }
        throw CookieImportError.databaseUnreadable
    }

    /// Ném `.permissionDenied` nếu file tồn tại nhưng mở đọc bị từ chối (EPERM/EACCES). Lỗi khác bỏ qua.
    static func checkReadable(_ url: URL) throws {
        do {
            let handle = try FileHandle(forReadingFrom: url)
            try? handle.close()
        } catch {
            if isPermissionError(error) { throw CookieImportError.permissionDenied }
        }
    }

    /// EPERM/EACCES, hoặc NSFileReadNoPermissionError, kể cả khi nằm trong underlying error.
    static func isPermissionError(_ error: any Error) -> Bool {
        let ns = error as NSError
        if ns.domain == NSPOSIXErrorDomain { return ns.code == Int(EPERM) || ns.code == Int(EACCES) }
        if ns.domain == NSCocoaErrorDomain {
            if ns.code == NSFileReadNoPermissionError { return true }
            if let under = ns.userInfo[NSUnderlyingErrorKey] as? NSError, under.domain == NSPOSIXErrorDomain {
                return under.code == Int(EPERM) || under.code == Int(EACCES)
            }
        }
        return false
    }

    /// Host chấp nhận: `.<domain>`, `<domain>`, `www.<domain>`.
    static func site(of host: String) -> String? {
        sites.first { host == "." + $0 || host == $0 || host == "www." + $0 }
    }

    private struct FileState: Equatable {
        var size: UInt64
        var mtime: Date
    }

    private static func signature(of source: URL) -> [FileState?] {
        ["", "-wal", "-journal"].map { suffix in
            let attrs = try? FileManager.default.attributesOfItem(atPath: source.path + suffix)
            guard let size = attrs?[.size] as? UInt64, let mtime = attrs?[.modificationDate] as? Date else { return nil }
            return FileState(size: size, mtime: mtime)
        }
    }

    private static func cleanStaleTempDirs(_ fm: FileManager) {
        let base = fm.temporaryDirectory
        guard let items = try? fm.contentsOfDirectory(atPath: base.path) else { return }
        for item in items where item.hasPrefix(tempPrefix) {
            try? fm.removeItem(at: base.appendingPathComponent(item))
        }
    }

    private struct Row2 {
        var host: String
        var stamp: Int64
        var row: Row
    }

    private static func query(_ db: Database, decryptor: ChromiumDecryptor, now: Date) throws -> Result {
        let version = try String.fetchOne(db, sql: "SELECT value FROM meta WHERE key = 'version'").flatMap(Int.init) ?? 0
        let columns = try Row.fetchAll(db, sql: "PRAGMA table_info(cookies)").compactMap { $0["name"] as String? }
        let stampColumn = columns.contains("last_update_utc") ? "last_update_utc" : "creation_utc"
        let hosts = sites.flatMap { [".\($0)", $0, "www.\($0)"] }
        let marks = hosts.map { _ in "?" }.joined(separator: ",")
        let rows = try Row.fetchAll(db, sql: """
            SELECT host_key, name, value, encrypted_value, expires_utc, \(stampColumn) AS stamp FROM cookies
            WHERE host_key IN (\(marks)) ORDER BY \(stampColumn) DESC
            """, arguments: StatementArguments(hosts))

        let nowChrome = Int64((now.timeIntervalSince1970 + 11_644_473_600) * 1_000_000)
        var candidates: [Row2] = []
        for row in rows {
            let expiresUTC: Int64 = row["expires_utc"] ?? 0
            if expiresUTC != 0 && expiresUTC <= nowChrome { continue }
            candidates.append(Row2(host: row["host_key"], stamp: row["stamp"] ?? 0, row: row))
        }
        // Cùng thời gian thì host bắt đầu bằng dấu chấm đứng trước (sort ổn định thủ công).
        let ordered = candidates.enumerated().sorted { a, b in
            if a.element.stamp != b.element.stamp { return a.element.stamp > b.element.stamp }
            let ad = a.element.host.hasPrefix("."), bd = b.element.host.hasPrefix(".")
            if ad != bd { return ad }
            return a.offset < b.offset
        }.map(\.element)

        var seen = Set<String>()
        var cookies: [Cookie] = []
        var failures: [Failure] = []
        for item in ordered {
            guard let site = site(of: item.host) else { continue }
            let name: String = item.row["name"]
            guard seen.insert("\(site)|\(name)").inserted else { continue }
            let plain: String = item.row["value"] ?? ""
            let value: String
            if !plain.isEmpty {
                value = plain
            } else if let blob: Data = item.row["encrypted_value"], !blob.isEmpty {
                do {
                    value = try decryptor.decrypt(blob, metaVersion: version, hostKey: item.host)
                } catch {
                    failures.append(Failure(domain: item.host, name: name))
                    continue
                }
            } else {
                continue
            }
            let expiresUTC: Int64 = item.row["expires_utc"] ?? 0
            let expires = expiresUTC > 0 ? Date(timeIntervalSince1970: Double(expiresUTC) / 1_000_000 - 11_644_473_600) : nil
            cookies.append(Cookie(name: name, value: value, domain: item.host, expires: expires))
        }
        return Result(cookies: cookies, failures: failures)
    }
}
