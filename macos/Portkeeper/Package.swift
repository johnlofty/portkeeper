// swift-tools-version: 5.9
//
// The menu-bar app. A thin client: everything that manages ssh lives in the Go daemon,
// and this package only reads its loopback JSON API and draws it.
//
// PortkeeperKit holds the models and the HTTP client, with no SwiftUI in it, so the
// test target can import it without dragging in an app with an @main.
import PackageDescription

let package = Package(
    name: "Portkeeper",
    platforms: [.macOS(.v14)],
    targets: [
        .target(name: "PortkeeperKit"),
        .executableTarget(name: "Portkeeper", dependencies: ["PortkeeperKit"]),
        .testTarget(name: "PortkeeperKitTests", dependencies: ["PortkeeperKit"]),
    ],
    swiftLanguageVersions: [.v5]
)
