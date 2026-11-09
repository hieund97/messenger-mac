import CommonCrypto
import CryptoKit
import Foundation

public enum DecryptError: Error, Equatable, Sendable {
    case badPrefix
    case keyDerivationFailed
    case decryptFailed
    case notUTF8
    /// metaVersion >= 24: 32 byte đầu không khớp SHA256(host_key), thường do sai key.
    case hostHashMismatch
}

public struct ChromiumDecryptor: Sendable {
    private let key: Data

    public init(password: String) throws {
        var derived = Data(count: 16)
        let salt = Array("saltysalt".utf8)
        let pass = Array(password.utf8)
        let status = derived.withUnsafeMutableBytes { out in
            CCKeyDerivationPBKDF(
                CCPBKDFAlgorithm(kCCPBKDF2),
                pass.map { Int8(bitPattern: $0) }, pass.count,
                salt, salt.count,
                CCPseudoRandomAlgorithm(kCCPRFHmacAlgSHA1), 1003,
                out.bindMemory(to: UInt8.self).baseAddress, 16)
        }
        guard status == kCCSuccess else { throw DecryptError.keyDerivationFailed }
        key = derived
    }

    public func decrypt(_ blob: Data, metaVersion: Int, hostKey: String) throws -> String {
        guard blob.count > 3, blob.prefix(3) == Data("v10".utf8) else { throw DecryptError.badPrefix }
        let cipher = blob.dropFirst(3)
        let iv = [UInt8](repeating: 0x20, count: 16)
        var out = Data(count: cipher.count + kCCBlockSizeAES128)
        var written = 0
        let outCapacity = out.count
        let status = out.withUnsafeMutableBytes { outPtr in
            cipher.withUnsafeBytes { inPtr in
                key.withUnsafeBytes { keyPtr in
                    CCCrypt(CCOperation(kCCDecrypt), CCAlgorithm(kCCAlgorithmAES128),
                            CCOptions(kCCOptionPKCS7Padding),
                            keyPtr.baseAddress, key.count, iv,
                            inPtr.baseAddress, cipher.count,
                            outPtr.baseAddress, outCapacity, &written)
                }
            }
        }
        guard status == kCCSuccess else { throw DecryptError.decryptFailed }
        var plain = out.prefix(written)
        if metaVersion >= 24 {
            guard plain.count >= 32 else { throw DecryptError.decryptFailed }
            guard plain.prefix(32) == Data(SHA256.hash(data: Data(hostKey.utf8))) else { throw DecryptError.hostHashMismatch }
            plain = plain.dropFirst(32)
        }
        guard let text = String(data: plain, encoding: .utf8) else { throw DecryptError.notUTF8 }
        return text
    }
}
