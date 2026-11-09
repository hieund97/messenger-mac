// swift-tools-version: 6.0
import PackageDescription

let package = Package(
    name: "MessengerKit",
    platforms: [.macOS(.v14)],
    products: [.library(name: "CoreBridge", targets: ["CoreBridge"])],
    targets: [
        .binaryTarget(name: "MessengerCore", path: "../core/build/MessengerCore.xcframework"),
        .target(name: "CoreBridge", dependencies: ["MessengerCore"],
                linkerSettings: [.linkedLibrary("resolv"), .linkedFramework("CoreFoundation"), .linkedFramework("Security")]),
        .testTarget(name: "CoreBridgeTests", dependencies: ["CoreBridge"]),
    ]
)
