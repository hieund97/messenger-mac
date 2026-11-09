import SwiftUI
import CoreBridge

@main
struct MacChatApp: App {
    // Tạm gọi lõi Go để linker giữ archive và lỗi link lộ ra ngay. Task 8 thay bằng CoreBridge thật.
    init() { _ = core_init("{}", nil) }

    var body: some Scene {
        WindowGroup {
            Text("MacChat").frame(minWidth: 400, minHeight: 300)
        }
    }
}
