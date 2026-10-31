// swift-tools-version:5.9
import PackageDescription

let package = Package(
    name: "Platen",
    platforms: [.macOS(.v14)],
    targets: [
        // Everything except the user interface, so it can be checked without one.
        .target(name: "PlatenCore"),
        .executableTarget(name: "Platen", dependencies: ["PlatenCore"]),
        // XCTest is not available with the Command Line Tools alone, so the
        // checks are a plain executable: swift run PlatenChecks.
        .executableTarget(name: "PlatenChecks", dependencies: ["PlatenCore"]),
    ]
)
