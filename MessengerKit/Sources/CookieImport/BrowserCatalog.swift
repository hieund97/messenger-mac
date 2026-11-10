import Foundation

public struct Cookie: Codable, Equatable, Sendable {
    public var name: String
    public var value: String
    public var domain: String
    public var expires: Date?

    public init(name: String, value: String, domain: String, expires: Date? = nil) {
        self.name = name
        self.value = value
        self.domain = domain
        self.expires = expires
    }

    private enum CodingKeys: String, CodingKey {
        case name, value, domain, expires
    }

    /// `expires` mã hóa thành số nguyên giây Unix để khớp `Expires int64` phía Go.
    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        name = try c.decode(String.self, forKey: .name)
        value = try c.decode(String.self, forKey: .value)
        domain = try c.decode(String.self, forKey: .domain)
        expires = try c.decodeIfPresent(Int64.self, forKey: .expires).map { Date(timeIntervalSince1970: TimeInterval($0)) }
    }

    public func encode(to encoder: any Encoder) throws {
        var c = encoder.container(keyedBy: CodingKeys.self)
        try c.encode(name, forKey: .name)
        try c.encode(value, forKey: .value)
        try c.encode(domain, forKey: .domain)
        try c.encodeIfPresent(expires.map { Int64($0.timeIntervalSince1970) }, forKey: .expires)
    }
}

public enum Browser: String, CaseIterable, Sendable {
    case chrome, arc, brave, edge

    /// Đường dẫn tương đối so với `~/Library/Application Support`.
    var dataDirComponents: [String] {
        switch self {
        case .chrome: ["Google", "Chrome"]
        case .arc: ["Arc", "User Data"]
        case .brave: ["BraveSoftware", "Brave-Browser"]
        case .edge: ["Microsoft Edge"]
        }
    }

    public var keychainService: String {
        switch self {
        case .chrome: "Chrome Safe Storage"
        case .arc: "Arc Safe Storage"
        case .brave: "Brave Safe Storage"
        case .edge: "Microsoft Edge Safe Storage"
        }
    }

    /// Giá trị `account` của mục Keychain "<Browser> Safe Storage".
    public var keychainAccount: String {
        switch self {
        case .chrome: "Chrome"
        case .arc: "Arc"
        case .brave: "Brave"
        case .edge: "Microsoft Edge"
        }
    }

    public var bundleIdentifier: String {
        switch self {
        case .chrome: "com.google.Chrome"
        case .arc: "company.thebrowser.Browser"
        case .brave: "com.brave.Browser"
        case .edge: "com.microsoft.edgemac"
        }
    }

    public var displayName: String {
        switch self {
        case .chrome: "Chrome"
        case .arc: "Arc"
        case .brave: "Brave"
        case .edge: "Edge"
        }
    }

    public func dataDirectory(home: URL = FileManager.default.homeDirectoryForCurrentUser) -> URL {
        dataDirComponents.reduce(home.appendingPathComponent("Library/Application Support", isDirectory: true)) {
            $0.appendingPathComponent($1, isDirectory: true)
        }
    }
}

public struct BrowserProfile: Equatable, Sendable {
    public var browser: Browser
    public var name: String
    public var cookiesPath: URL

    public init(browser: Browser, name: String, cookiesPath: URL) {
        self.browser = browser
        self.name = name
        self.cookiesPath = cookiesPath
    }
}

public enum BrowserCatalog {
    /// false khi có ít nhất một trình duyệt đã cài (thư mục dữ liệu tồn tại) mà `Local State` hoặc `Cookies`
    /// của profile `Default` không đọc được do bị chặn quyền (EPERM/EACCES, thường là thiếu Full Disk Access).
    public static func hasDataAccess(
        home: URL = FileManager.default.homeDirectoryForCurrentUser
    ) -> Bool {
        let fm = FileManager.default
        for browser in Browser.allCases {
            let dataDir = browser.dataDirectory(home: home)
            // Liệt kê thư mục cũng có thể bị TCC chặn; chỉ coi là "đã cài" khi thấy được thư mục.
            guard fm.fileExists(atPath: dataDir.path) else { continue }
            let targets = [
                dataDir.appendingPathComponent("Local State"),
                dataDir.appendingPathComponent("Default/Cookies"),
                dataDir.appendingPathComponent("Default/Network/Cookies"),
            ]
            for url in targets where (try? ChromiumCookieReader.checkReadable(url)) == nil {
                return false
            }
        }
        return true
    }

    /// Profile có file `Cookies`. macOS để ở `<Profile>/Cookies`; giữ fallback `<Profile>/Network/Cookies`.
    /// Không đọc được `Local State` thì vẫn trả profile `Default` nếu có file Cookies.
    public static func installedProfiles(
        home: URL = FileManager.default.homeDirectoryForCurrentUser
    ) -> [BrowserProfile] {
        var result: [BrowserProfile] = []
        let fm = FileManager.default
        for browser in Browser.allCases {
            let dataDir = browser.dataDirectory(home: home)
            var cache: [String: Any] = [:]
            if let data = try? Data(contentsOf: dataDir.appendingPathComponent("Local State")),
               let root = try? JSONSerialization.jsonObject(with: data) as? [String: Any],
               let profile = root["profile"] as? [String: Any],
               let infoCache = profile["info_cache"] as? [String: Any] {
                cache = infoCache
            }
            var dirNames = Array(cache.keys)
            if !dirNames.contains("Default") { dirNames.append("Default") }
            dirNames.sort { $0.localizedStandardCompare($1) == .orderedAscending }
            for dirName in dirNames {
                let dir = dataDir.appendingPathComponent(dirName, isDirectory: true)
                let candidates = [dir.appendingPathComponent("Cookies"), dir.appendingPathComponent("Network/Cookies")]
                guard let path = candidates.first(where: { fm.fileExists(atPath: $0.path) }) else { continue }
                let displayName = ((cache[dirName] as? [String: Any])?["name"] as? String) ?? dirName
                result.append(BrowserProfile(browser: browser, name: displayName, cookiesPath: path))
            }
        }
        return result
    }
}
