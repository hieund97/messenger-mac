// swift-tools-version: 6.0
import PackageDescription

let package = Package(
    name: "MessengerKit",
    platforms: [.macOS(.v14)],
    products: [
        .library(name: "CoreBridge", targets: ["CoreBridge"]),
        .library(name: "CookieImport", targets: ["CookieImport"]),
    ],
    dependencies: [
        .package(url: "https://github.com/groue/GRDB.swift", from: "7.0.0"),
    ],
    targets: [
        .binaryTarget(name: "MessengerCore", path: "../core/build/MessengerCore.xcframework"),
        .target(name: "CoreBridge", dependencies: ["MessengerCore"],
                linkerSettings: [.linkedLibrary("resolv"), .linkedFramework("CoreFoundation"), .linkedFramework("Security")]),
        .testTarget(name: "CoreBridgeTests", dependencies: ["CoreBridge"]),
        .target(name: "CookieImport", dependencies: [.product(name: "GRDB", package: "GRDB.swift")],
                linkerSettings: [.linkedFramework("Security")]),
        .testTarget(name: "CookieImportTests", dependencies: ["CookieImport", .product(name: "GRDB", package: "GRDB.swift")]),
    ]
)
