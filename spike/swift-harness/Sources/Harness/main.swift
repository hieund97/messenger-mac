import Foundation
import CSpike
import Darwin

guard CommandLine.arguments.count > 1 else {
    fputs("usage: Harness cookies.json\n", stderr)
    exit(1)
}
let raw: String
do { raw = try String(contentsOfFile: CommandLine.arguments[1], encoding: .utf8) }
catch { print("error: cannot read cookies \(CommandLine.arguments[1]): \(error)"); exit(1) }
let callback: @convention(c) (UnsafePointer<CChar>?) -> Void = { event in
    if let event { print(String(cString: event)); fflush(stdout) }
}
let result = raw.withCString { json in
    "./spike-data".withCString { dir in spike_start(UnsafeMutablePointer(mutating: json), UnsafeMutablePointer(mutating: dir), callback) }
}
if result != 0 { exit(1) }
signal(SIGINT, SIG_IGN)
let source = DispatchSource.makeSignalSource(signal: SIGINT, queue: .main)
source.setEventHandler { spike_stop(); exit(0) }
source.resume()
RunLoop.main.run()
