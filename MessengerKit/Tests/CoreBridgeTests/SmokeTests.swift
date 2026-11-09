import Testing
@testable import CoreBridge

// CLT-only: máy không có XCTest, dùng swift-testing. Đổi lại XCTest khi có Xcode.
struct SmokeTests {
    @Test func testInitReturnsZero() {
        #expect(core_init("{}", nil) == 0)
    }
}
