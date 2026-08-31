import AppKit
import UserNotifications

/// Local "download finished" notifications. Clicking one reveals the file in
/// Finder.
///
/// MacDM is ad-hoc signed (no Apple Developer account), and macOS refuses
/// `UNUserNotificationCenter` for an app with no Team ID — `requestAuthorization`
/// comes back with "Notifications are not allowed for this application". So the
/// modern API is tried first and, when it is refused, we fall back to the
/// deprecated-but-functional `NSUserNotification`, which has no such
/// requirement. Once MacDM is ever Developer ID signed the modern path takes
/// over on its own with no code change.
enum Notifier {
    private static let unDelegate = UNDelegate()
    private static let legacyDelegate = LegacyDelegate()
    private static var available: Bool { Bundle.main.bundleIdentifier != nil }

    /// nil until `configure()`'s authorization callback lands.
    private static var useModern: Bool?
    private static var pending: [Job] = []
    private static let lock = NSLock()

    /// Last known state, also written to
    /// ~/Library/Application Support/MacDM/notify-status.txt — NSLog from a
    /// LaunchServices-started app does not reliably reach `log show`, and a
    /// silent refusal here looks exactly like "downloads never finish".
    private(set) static var status = "unknown"

    private static func record(_ s: String) {
        status = s
        NSLog("MacDM: notifications — %@", s)
        let dir = (NSHomeDirectory() as NSString)
            .appendingPathComponent("Library/Application Support/MacDM")
        try? FileManager.default.createDirectory(atPath: dir, withIntermediateDirectories: true)
        try? s.write(toFile: dir + "/notify-status.txt", atomically: true, encoding: .utf8)
    }

    static func configure() {
        guard available else {
            record("unavailable (no bundle identifier)")
            settle(modern: false)
            return
        }
        NSUserNotificationCenter.default.delegate = legacyDelegate
        let center = UNUserNotificationCenter.current()
        center.delegate = unDelegate
        center.requestAuthorization(options: [.alert, .sound]) { granted, err in
            if let err = err {
                record("UNUserNotificationCenter refused (\(err.localizedDescription)) — using NSUserNotification")
                settle(modern: false)
            } else if granted {
                record("authorized")
                settle(modern: true)
            } else {
                // An explicit user "Don't Allow" must be honoured, not routed
                // around via the legacy API.
                record("denied by the user")
                settle(modern: nil)
            }
        }
    }

    /// Records which backend to use and flushes anything that finished while the
    /// authorization callback was still in flight.
    private static func settle(modern: Bool?) {
        lock.lock()
        useModern = modern ?? false
        let queued = pending
        pending = []
        let disabled = (modern == nil)
        lock.unlock()
        guard !disabled else { return }
        DispatchQueue.main.async { queued.forEach { post($0) } }
    }

    static func downloadFinished(_ job: Job) {
        guard available else { return }
        lock.lock()
        if useModern == nil { // authorization still pending — do not drop it
            pending.append(job)
            lock.unlock()
            return
        }
        lock.unlock()
        post(job)
    }

    private static func post(_ job: Job) {
        let path = (job.dest?.isEmpty == false) ? job.dest! : nil
        if useModern == true {
            let content = UNMutableNotificationContent()
            content.title = "Download complete"
            content.body = job.filename
            content.sound = .default
            if let path { content.userInfo = ["path": path] }
            let req = UNNotificationRequest(identifier: "done-\(job.id)", content: content, trigger: nil)
            UNUserNotificationCenter.current().add(req) { err in
                if let err = err { record("post failed: \(err.localizedDescription)") }
            }
        } else {
            postLegacy(title: "Download complete", body: job.filename, path: path)
        }
    }

    // NSUserNotification is deprecated on macOS 11+, but it is the only local
    // notification API available to an ad-hoc signed app, and it still works.
    @available(macOS, deprecated: 11.0)
    private static func postLegacy(title: String, body: String, path: String?) {
        let n = NSUserNotification()
        n.title = title
        n.informativeText = body
        n.soundName = NSUserNotificationDefaultSoundName
        if let path { n.userInfo = ["path": path] }
        NSUserNotificationCenter.default.deliver(n)
    }

    fileprivate static func reveal(_ path: String?) {
        guard let path, FileManager.default.fileExists(atPath: path) else { return }
        NSWorkspace.shared.activateFileViewerSelecting([URL(fileURLWithPath: path)])
    }
}

final class UNDelegate: NSObject, UNUserNotificationCenterDelegate {
    // Show the banner even when MacDM is the frontmost app.
    func userNotificationCenter(_ center: UNUserNotificationCenter,
                                willPresent notification: UNNotification,
                                withCompletionHandler completionHandler: @escaping (UNNotificationPresentationOptions) -> Void) {
        completionHandler([.banner, .sound])
    }

    // Click → reveal the finished file in Finder.
    func userNotificationCenter(_ center: UNUserNotificationCenter,
                                didReceive response: UNNotificationResponse,
                                withCompletionHandler completionHandler: @escaping () -> Void) {
        Notifier.reveal(response.notification.request.content.userInfo["path"] as? String)
        completionHandler()
    }
}

@available(macOS, deprecated: 11.0)
final class LegacyDelegate: NSObject, NSUserNotificationCenterDelegate {
    // Same "show it even when we're frontmost" behaviour as the modern path.
    func userNotificationCenter(_ center: NSUserNotificationCenter,
                                shouldPresent notification: NSUserNotification) -> Bool { true }

    func userNotificationCenter(_ center: NSUserNotificationCenter,
                                didActivate notification: NSUserNotification) {
        Notifier.reveal(notification.userInfo?["path"] as? String)
    }
}
