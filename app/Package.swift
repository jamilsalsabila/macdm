// swift-tools-version:5.7
import PackageDescription

let package = Package(
    name: "MacDM",
    platforms: [.macOS(.v12)],
    targets: [
        .executableTarget(
            name: "MacDM",
            path: "Sources/MacDM"
        )
    ]
)
