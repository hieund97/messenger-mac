import Security
import Testing
@testable import CookieImport

struct CookieImportTests {
    @Test func testDecryptKnownVectorV10() throws {
        let decryptor = try ChromiumDecryptor(password: password)
        let blob = plainBlob("hello-cookie")
        #expect(try decryptor.decrypt(blob, metaVersion: 23, hostKey: ".messenger.com") == "hello-cookie")

        // Hằng sinh độc lập bằng openssl (không dùng code của repo):
        //   KEY=$(python3 -c "import hashlib;print(hashlib.pbkdf2_hmac('sha1',b'peanuts',b'saltysalt',1003,16).hex())")
        //   printf 'openssl-vector' | openssl enc -aes-128-cbc -K $KEY -iv 20202020202020202020202020202020 | xxd -p
        // rồi thêm tiền tố "v10" (763130).
        let fixed = hexData("763130" + "b78996af6d2d57d00613c325cb011896")
        #expect(try decryptor.decrypt(fixed, metaVersion: 23, hostKey: ".messenger.com") == "openssl-vector")
    }

    @Test func testStripsDomainHashWhenMetaVersion24() throws {
        let blob = hashedBlob("real-value", host: ".messenger.com")
        let decryptor = try ChromiumDecryptor(password: password)
        #expect(try decryptor.decrypt(blob, metaVersion: 24, hostKey: ".messenger.com") == "real-value")
        #expect(throws: DecryptError.hostHashMismatch) {
            try decryptor.decrypt(blob, metaVersion: 24, hostKey: ".facebook.com")
        }
    }

    @Test func testPrefersMessengerDomain() throws {
        let (path, _) = try makeTempDB(rows: [
            ("www.messenger.com", "c_user", "m1"), (".messenger.com", "xs", "m2"), (".messenger.com", "datr", "m3"),
            (".facebook.com", "c_user", "f1"), (".facebook.com", "xs", "f2"), (".facebook.com", "datr", "f3"),
        ])
        let cookies = try importer(password: password).importSession(profile(path))
        #expect(cookies.count == 3)
        #expect(cookies.allSatisfy { $0.domain.hasSuffix("messenger.com") })
        #expect(cookies.first { $0.name == "xs" }?.value == "m2")
    }

    @Test func testMissingXsThrowsNotLoggedIn() throws {
        let (path, _) = try makeTempDB(rows: [
            (".messenger.com", "c_user", "1"), (".messenger.com", "datr", "3"),
            (".facebook.com", "c_user", "1"), (".facebook.com", "datr", "3"),
        ])
        #expect(throws: CookieImportError.notLoggedIn) {
            try importer(password: password).importSession(profile(path))
        }
    }

    @Test func testLockedDatabaseIsCopiedNotOpenedInPlace() throws {
        // Writer giữ khóa EXCLUSIVE trong suốt test; mở tại chỗ sẽ bị SQLITE_BUSY.
        let (path, writer) = try makeTempDB(rows: [
            (".messenger.com", "c_user", "1"), (".messenger.com", "xs", "2"), (".messenger.com", "datr", "3"),
        ], exclusive: true)
        let before = mtime(path)
        let cookies = try importer(password: password).importSession(profile(path))
        #expect(cookies.count == 3)
        #expect(mtime(path) == before)
        withExtendedLifetime(writer) {}
    }

    @Test func testExpiredCookieIgnoredFallsBackToFacebook() throws {
        let (path, _) = try makeTempDB(rows: [
            (".messenger.com", "c_user", "m1"), (".messenger.com", "xs", "m2"), (".messenger.com", "datr", "m3"),
            (".facebook.com", "c_user", "f1"), (".facebook.com", "xs", "f2"), (".facebook.com", "datr", "f3"),
        ], expires: [".messenger.com|xs": 1_000_000])  // 1970: đã hết hạn
        let cookies = try importer(password: password).importSession(profile(path))
        #expect(cookies.count == 3)
        #expect(cookies.allSatisfy { $0.domain.hasSuffix("facebook.com") })
    }

    @Test func testUndecryptableUnrelatedCookieDoesNotFailImport() throws {
        let (path, _) = try makeTempDB(rows: [
            (".messenger.com", "c_user", "1"), (".messenger.com", "xs", "2"), (".messenger.com", "datr", "3"),
            (".messenger.com", "other", "x"),
        ], garbage: [".messenger.com|other"])
        let cookies = try importer(password: password).importSession(profile(path))
        #expect(cookies.map(\.name).sorted() == ["c_user", "datr", "xs"])
    }

    @Test func testKeychainDeniedMapsToError() throws {
        for status in [errSecAuthFailed, errSecUserCanceled] {
            #expect(throws: CookieImportError.keychainDenied) {
                try importer(failing: status).importSession(nonexistentProfile())
            }
        }
    }
}
