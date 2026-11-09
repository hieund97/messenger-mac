// swift-tools-version: 6.0
import PackageDescription

let package = Package(
    name: "MacChat",
    platforms: [.macOS(.v14)],
    dependencies: [.package(path: "../MessengerKit")],
    targets: [
        .executableTarget(name: "MacChat",
                          dependencies: [.product(name: "CoreBridge", package: "MessengerKit")]),
    ]
)
