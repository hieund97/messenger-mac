import Testing
@testable import CoreBridge

// CLT-only: máy không có XCTest, dùng swift-testing. Đổi lại XCTest khi có Xcode.
// C state là singleton toàn tiến trình: mọi test gọi core_* nằm trong CoreSerialTests.
@Suite(.serialized) struct CoreSerialTests {
    @Test func testInitReturnsZero() {
        #expect(core_init("{\"backend\":\"fake\"}", nil) == 0)
        core_shutdown()
    }
}
