import CommonCrypto
import CryptoKit
import Foundation
import GRDB
import Security
@testable import CookieImport

func encrypt(_ plain: Data, password: String) -> Data {
    var key = Data(count: 16)
    let salt = Array("saltysalt".utf8)
    let pass = Array(password.utf8)
    _ = key.withUnsafeMutableBytes { out in
        CCKeyDerivationPBKDF(CCPBKDFAlgorithm(kCCPBKDF2), pass.map { Int8(bitPattern: $0) }, pass.count,
                             salt, salt.count, CCPseudoRandomAlgorithm(kCCPRFHmacAlgSHA1), 1003,
                             out.bindMemory(to: UInt8.self).baseAddress, 16)
    }
    let iv = [UInt8](repeating: 0x20, count: 16)
    var out = Data(count: plain.count + 16)
    var written = 0
    let cap = out.count
    let status = out.withUnsafeMutableBytes { o in
        plain.withUnsafeBytes { i in
            key.withUnsafeBytes { k in
                CCCrypt(CCOperation(kCCEncrypt), CCAlgorithm(kCCAlgorithmAES128), CCOptions(kCCOptionPKCS7Padding),
                        k.baseAddress, 16, iv, i.baseAddress, plain.count, o.baseAddress, cap, &written)
            }
        }
    }
    precondition(status == kCCSuccess)
    return Data("v10".utf8) + out.prefix(written)
}

let password = "peanuts"

func enc(_ value: String, host: String, metaVersion: Int = 24) -> Data {
    let prefix = metaVersion >= 24 ? Data(SHA256.hash(data: Data(host.utf8))) : Data()
    return encrypt(prefix + Data(value.utf8), password: password)
}

struct FakeKeychain: KeychainReading {
    var result: Result<String, KeychainError>
    func password(service: String, account: String?) throws -> String { try result.get() }
}

func makeDB(in dir: URL, rows: [(host: String, name: String, value: String)], metaVersion: Int = 24,
                    wal: Bool = false, exclusive: Bool = false,
                    expires: [String: Int64] = [:], garbage: Set<String> = []) throws -> (URL, DatabaseQueue) {
    let path = dir.appendingPathComponent("Cookies")
    let queue = try DatabaseQueue(path: path.path)
    // Khóa EXCLUSIVE chỉ được giữ sau lần ghi đầu tiên, nên pragma phải đặt trước khi tạo bảng.
    if exclusive { try queue.writeWithoutTransaction { try $0.execute(sql: "PRAGMA locking_mode = EXCLUSIVE") } }
    if wal { try queue.writeWithoutTransaction { try $0.execute(sql: "PRAGMA journal_mode = WAL") } }
    try queue.write { db in
        try db.execute(sql: "CREATE TABLE meta(key TEXT, value TEXT)")
        try db.execute(sql: "INSERT INTO meta VALUES ('version', ?)", arguments: [String(metaVersion)])
        try db.execute(sql: """
            CREATE TABLE cookies(creation_utc INTEGER, host_key TEXT, name TEXT, value TEXT, encrypted_value BLOB,
            expires_utc INTEGER, path TEXT, is_secure INTEGER, is_httponly INTEGER, last_update_utc INTEGER)
            """)
        for (i, r) in rows.enumerated() {
            let key = "\(r.host)|\(r.name)"
            let blob = garbage.contains(key) ? Data("v10".utf8) + Data(repeating: 0x42, count: 16)
                                              : enc(r.value, host: r.host, metaVersion: metaVersion)
            try db.execute(sql: "INSERT INTO cookies VALUES (?, ?, ?, '', ?, ?, '/', 1, 1, ?)",
                           arguments: [i + 1, r.host, r.name, blob, expires[key] ?? 0, i + 1])
        }
    }
    return (path, queue)
}

func tempDir() throws -> URL {
    let dir = FileManager.default.temporaryDirectory.appendingPathComponent("cookie-test-\(UUID().uuidString)")
    try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
    return dir
}

func nonexistentProfile() -> BrowserProfile { profile(URL(fileURLWithPath: "/nonexistent/Cookies")) }

func profile(_ path: URL) -> BrowserProfile {
    BrowserProfile(browser: .chrome, name: "Test", cookiesPath: path)
}


func importer(password: String) -> CookieImporter { CookieImporter(keychain: FakeKeychain(result: .success(password))) }
func importer(failing status: Int32) -> CookieImporter { CookieImporter(keychain: FakeKeychain(result: .failure(.status(status)))) }

func walExists(_ path: URL) -> Bool { FileManager.default.fileExists(atPath: path.path + "-wal") }

/// Fixture nằm trong thư mục tạm của hệ thống; để hệ điều hành dọn.
func makeTempDB(rows: [(host: String, name: String, value: String)], wal: Bool = false, exclusive: Bool = false,
                expires: [String: Int64] = [:], garbage: Set<String> = []) throws -> (URL, DatabaseQueue) {
    try makeDB(in: try tempDir(), rows: rows, wal: wal, exclusive: exclusive, expires: expires, garbage: garbage)
}

func plainBlob(_ text: String) -> Data { encrypt(Data(text.utf8), password: password) }
func hashedBlob(_ text: String, host: String) -> Data { encrypt(Data(SHA256.hash(data: Data(host.utf8))) + Data(text.utf8), password: password) }

func mtime(_ url: URL) -> Date? { try? FileManager.default.attributesOfItem(atPath: url.path)[.modificationDate] as? Date }

func hexData(_ hex: String) -> Data {
    Data(stride(from: 0, to: hex.count, by: 2).map { i in
        let s = hex.index(hex.startIndex, offsetBy: i)
        return UInt8(hex[s..<hex.index(s, offsetBy: 2)], radix: 16)!
    })
}

func setPermissions(_ url: URL, _ mode: Int) throws {
    try FileManager.default.setAttributes([.posixPermissions: mode], ofItemAtPath: url.path)
}
