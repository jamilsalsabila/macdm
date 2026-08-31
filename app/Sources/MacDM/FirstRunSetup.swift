import Foundation

/// One-time-ish setup the bundled app does for itself on every launch:
///
///  1. Register the native-messaging host manifest with every browser found, so
///     the MacDM extension can reach `macdm-nmhost` inside this bundle. Done on
///     each launch because the `path` must track the app if the user moves it.
///  2. Strip the quarantine flag from our own bundle. The app itself only runs
///     after the user did right-click → Open, but the nested helpers
///     (`macdmd`, `macdm-nmhost`, ffmpeg, yt-dlp) keep the flag, and Chrome
///     refuses to launch a quarantined native-messaging host.
enum FirstRunSetup {
    static func run() {
        clearOwnQuarantine()
        registerNativeMessagingHosts()
    }

    // Pinned by the "key" in extension/manifest.json.
    private static let extensionID = "bpdoaihjlkkbkkmeiccefmbalbhcppho"
    private static let hostName = "com.macdm.nmhost"

    private static var hostBinary: String {
        Bundle.main.bundlePath + "/Contents/MacOS/macdm-nmhost"
    }

    private static func registerNativeMessagingHosts() {
        let support = (NSHomeDirectory() as NSString)
            .appendingPathComponent("Library/Application Support")
        let mozilla = support + "/Mozilla"

        let chromium = [
            "Google/Chrome", "Google/Chrome Beta", "Google/Chrome Canary",
            "Chromium", "Microsoft Edge", "BraveSoftware/Brave-Browser",
            "Vivaldi", "Arc/User Data",
        ].map { support + "/" + $0 }
        let firefox = [mozilla, support + "/zen", support + "/librewolf"]

        let host = hostBinary
        guard FileManager.default.isExecutableFile(atPath: host) else { return }

        for base in chromium where dirExists(base) {
            writeManifest(dir: base, json: chromiumManifest(host: host))
        }
        for base in firefox where dirExists(base) {
            writeManifest(dir: base, json: firefoxManifest(host: host))
        }
    }

    // Built with JSONSerialization, not string interpolation: `host` is the
    // app's own bundle path, and macOS allows " and \ in a folder name. A user
    // who put MacDM under, say, My "Apps" would otherwise get a malformed
    // manifest and a silently dead native-messaging channel.
    private static func chromiumManifest(host: String) -> Data? {
        manifestData(host: host, extra: [
            "allowed_origins": ["chrome-extension://\(extensionID)/"],
        ])
    }

    private static func firefoxManifest(host: String) -> Data? {
        manifestData(host: host, extra: [
            "allowed_extensions": ["macdm@example.invalid"],
        ])
    }

    private static func manifestData(host: String, extra: [String: Any]) -> Data? {
        var obj: [String: Any] = [
            "name": hostName,
            "description": "MacDM native messaging host",
            "path": host,
            "type": "stdio",
        ]
        for (k, v) in extra { obj[k] = v }
        return try? JSONSerialization.data(withJSONObject: obj, options: [.prettyPrinted])
    }

    private static func writeManifest(dir: String, json: Data?) {
        guard let json else { return }
        let hostsDir = dir + "/NativeMessagingHosts"
        try? FileManager.default.createDirectory(
            atPath: hostsDir, withIntermediateDirectories: true)
        let path = hostsDir + "/\(hostName).json"
        try? json.write(to: URL(fileURLWithPath: path), options: .atomic)
    }

    private static func dirExists(_ p: String) -> Bool {
        var isDir: ObjCBool = false
        return FileManager.default.fileExists(atPath: p, isDirectory: &isDir) && isDir.boolValue
    }

    private static func clearOwnQuarantine() {
        let p = Process()
        p.executableURL = URL(fileURLWithPath: "/usr/bin/xattr")
        p.arguments = ["-dr", "com.apple.quarantine", Bundle.main.bundlePath]
        try? p.run()
        p.waitUntilExit()
    }
}
