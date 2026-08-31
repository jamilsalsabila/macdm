import AppKit
import UserNotifications

/// Local "download finished" notifications. Clicking one reveals the file in
/// Finder. No-ops when the process isn't a proper .app bundle (a bare
/// `./build.sh` binary has no bundle identifier and UNUserNotificationCenter
/// would trap).
enum Notifier {
    private static let delegate = NotificationDelegate()
    private static var available: Bool { Bundle.main.bundleIdentifier != nil }

    static func configure() {
        guard available else { return }
        let center = UNUserNotificationCenter.current()
        center.delegate = delegate
        center.requestAuthorization(options: [.alert, .sound]) { _, _ in }
    }

    static func downloadFinished(_ job: Job) {
        guard available else { return }
        let content = UNMutableNotificationContent()
        content.title = "Download complete"
        content.body = job.filename
        content.sound = .default
        if let dest = job.dest, !dest.isEmpty {
            content.userInfo = ["path": dest]
        }
        let req = UNNotificationRequest(identifier: "done-\(job.id)", content: content, trigger: nil)
        UNUserNotificationCenter.current().add(req)
    }
}

final class NotificationDelegate: NSObject, UNUserNotificationCenterDelegate {
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
        if let path = response.notification.request.content.userInfo["path"] as? String,
           FileManager.default.fileExists(atPath: path) {
            NSWorkspace.shared.activateFileViewerSelecting([URL(fileURLWithPath: path)])
        }
        completionHandler()
    }
}
