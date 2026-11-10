import Foundation
import Security

public enum CookieImportError: Error, Equatable, Sendable {
    case notLoggedIn
    case keychainDenied
    case keychainUnavailable(Int32)
    case databaseUnreadable
    /// macOS (TCC) chặn đọc dữ liệu trình duyệt: app thiếu Full Disk Access.
    case permissionDenied
    /// Thiếu cookie bắt buộc và có cookie bắt buộc không giải mã được (thường do sai key).
    case decryptFailed
}

public enum KeychainError: Error, Equatable, Sendable {
    case status(Int32)
}

public protocol KeychainReading: Sendable {
    func password(service: String, account: String?) throws -> String
}

public struct SystemKeychain: KeychainReading {
    public init() {}

    public func password(service: String, account: String?) throws -> String {
        var query: [String: Any] = [
            kSecClass as String: kSecClassGenericPassword,
            kSecAttrService as String: service,
            kSecReturnData as String: true,
            kSecMatchLimit as String: kSecMatchLimitOne,
        ]
        if let account { query[kSecAttrAccount as String] = account }
        var item: CFTypeRef?
        let status = SecItemCopyMatching(query as CFDictionary, &item)
        guard status == errSecSuccess, let data = item as? Data, let text = String(data: data, encoding: .utf8) else {
            throw KeychainError.status(status == errSecSuccess ? errSecDecode : status)
        }
        return text
    }
}

public struct CookieImporter: Sendable {
    public static let required = ["c_user", "xs", "datr"]
    private let keychain: any KeychainReading

    public init(keychain: any KeychainReading = SystemKeychain()) {
        self.keychain = keychain
    }

    /// Chặn luồng gọi khi hộp thoại Keychain đang mở (chờ người dùng cho phép).
    /// Không gọi trên main actor.
    public func importSession(_ profile: BrowserProfile) throws -> [Cookie] {
        let password: String
        do {
            password = try keychain.password(service: profile.browser.keychainService, account: profile.browser.keychainAccount)
        } catch KeychainError.status(let status) {
            switch status {
            case errSecItemNotFound:
                throw CookieImportError.notLoggedIn
            case errSecAuthFailed, errSecUserCanceled, errSecInteractionNotAllowed:
                throw CookieImportError.keychainDenied
            default:
                throw CookieImportError.keychainUnavailable(status)
            }
        }

        let result: ChromiumCookieReader.Result
        do {
            result = try ChromiumCookieReader.read(profile, password: password)
        } catch CookieImportError.permissionDenied {
            throw CookieImportError.permissionDenied
        } catch {
            throw CookieImportError.databaseUnreadable
        }

        var requiredUndecryptable = false
        for site in ChromiumCookieReader.sites {
            let group = result.cookies.filter { ChromiumCookieReader.site(of: $0.domain) == site }
            let names = Set(group.map(\.name))
            if Self.required.allSatisfy(names.contains) { return group }
            if result.failures.contains(where: {
                ChromiumCookieReader.site(of: $0.domain) == site && Self.required.contains($0.name)
            }) { requiredUndecryptable = true }
        }
        throw requiredUndecryptable ? CookieImportError.decryptFailed : CookieImportError.notLoggedIn
    }
}
