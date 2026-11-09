import Foundation

func tempDBPath() -> String {
    FileManager.default.temporaryDirectory.appendingPathComponent("macchat-\(UUID().uuidString)").appendingPathComponent("app.db").path
}
