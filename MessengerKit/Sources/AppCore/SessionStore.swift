import CoreBridge
import Foundation
import Security

public enum SessionStoreError: Error, Equatable, Sendable {
    case keychain(Int32)
    case corrupted
}

/// Keychain ghi được, tách protocol để test inject bản giả.
public protocol KeychainStoring: Sendable {
    func set(_ data: Data, service: String, account: String) throws
    func get(service: String, account: String) throws -> Data?
    func delete(service: String, account: String) throws
}

public struct SystemKeychainStore: KeychainStoring {
    public init() {}

    private func query(_ service: String, _ account: String) -> [String: Any] {
        [kSecClass as String: kSecClassGenericPassword,
         kSecAttrService as String: service,
         kSecAttrAccount as String: account]
    }

    public func set(_ data: Data, service: String, account: String) throws {
        var status = SecItemUpdate(query(service, account) as CFDictionary, [kSecValueData as String: data] as CFDictionary)
        if status == errSecItemNotFound {
            var add = query(service, account)
            add[kSecValueData as String] = data
            status = SecItemAdd(add as CFDictionary, nil)
        }
        guard status == errSecSuccess else { throw SessionStoreError.keychain(status) }
    }

    public func get(service: String, account: String) throws -> Data? {
        var q = query(service, account)
        q[kSecReturnData as String] = true
        q[kSecMatchLimit as String] = kSecMatchLimitOne
        var item: CFTypeRef?
        let status = SecItemCopyMatching(q as CFDictionary, &item)
        if status == errSecItemNotFound { return nil }
        guard status == errSecSuccess, let data = item as? Data else { throw SessionStoreError.keychain(status) }
        return data
    }

    public func delete(service: String, account: String) throws {
        let status = SecItemDelete(query(service, account) as CFDictionary)
        guard status == errSecSuccess || status == errSecItemNotFound else { throw SessionStoreError.keychain(status) }
    }
}

/// Bí mật của phiên (cookie, `secret_store`) trong Keychain: service `dev.hieund.macmess`, account `<c_user>/<key>`.
public struct SessionStore: Sendable {
    public static let service = "dev.hieund.macmess"
    public static let historySecretKey = "secure_history_device"
    static let cookiesKey = "cookies"
    private let keychain: any KeychainStoring
    private let service: String

    /// `service` mặc định là `dev.hieund.macmess`; chế độ fake dùng service riêng để không đụng dữ liệu thật.
    public init(keychain: any KeychainStoring = SystemKeychainStore(), service: String = SessionStore.service) {
        self.keychain = keychain
        self.service = service
    }

    private func account(_ cUser: String, _ key: String) -> String { "\(cUser)/\(key)" }

    public func saveCookies(_ cookies: [CookieParam], cUser: String) throws {
        try keychain.set(try JSONEncoder().encode(cookies), service: service, account: account(cUser, Self.cookiesKey))
    }

    public func loadCookies(cUser: String) throws -> [CookieParam]? {
        guard let data = try keychain.get(service: service, account: account(cUser, Self.cookiesKey)) else { return nil }
        do { return try JSONDecoder().decode([CookieParam].self, from: data) } catch { throw SessionStoreError.corrupted }
    }

    public func deleteCookies(cUser: String) throws {
        try keychain.delete(service: service, account: account(cUser, Self.cookiesKey))
    }

    public func saveSecret(_ value: String, key: String, cUser: String) throws {
        guard !value.isEmpty else { try deleteSecret(key: key, cUser: cUser); return }
        try keychain.set(Data(value.utf8), service: service, account: account(cUser, key))
    }

    public func deleteSecret(key: String, cUser: String) throws {
        try keychain.delete(service: service, account: account(cUser, key))
    }

    public func loadSecret(key: String, cUser: String) throws -> String? {
        guard let data = try keychain.get(service: service, account: account(cUser, key)) else { return nil }
        return String(data: data, encoding: .utf8)
    }
}
