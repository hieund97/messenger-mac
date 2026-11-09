// swift-tools-version: 6.0
import PackageDescription

let package = Package(
    name: "MacMess",
    platforms: [.macOS(.v14)],
    dependencies: [.package(path: "../MessengerKit")],
    targets: [
        .executableTarget(name: "MacMess",
                          dependencies: [
                              .product(name: "CoreBridge", package: "MessengerKit"),
                              .product(name: "AppCore", package: "MessengerKit"),
                              .product(name: "CookieImport", package: "MessengerKit"),
                          ]),
    ]
)
