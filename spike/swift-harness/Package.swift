// swift-tools-version: 6.0
import PackageDescription
let package = Package(name: "Harness", platforms: [.macOS(.v14)], targets: [
    .systemLibrary(name: "CSpike"),
    .executableTarget(name: "Harness", dependencies: ["CSpike"], linkerSettings: [
        .unsafeFlags(["-L", "../bin", "-lspike", "-lresolv"]),
        .linkedFramework("CoreFoundation"), .linkedFramework("Security")
    ])
])
