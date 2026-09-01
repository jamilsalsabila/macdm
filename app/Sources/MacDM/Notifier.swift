import AppKit

/// "Download finished" notifications.
///
/// These are drawn by MacDM itself (see `Toast`) rather than handed to
/// Notification Centre. macOS refuses `UNUserNotificationCenter` to an app with
/// no Team ID, and MacDM is ad-hoc signed (no Apple Developer account) — for
/// most of its life `requestAuthorization` answered "Notifications are not
/// allowed for this application" and downloads finished in silence. Even once
/// the system does grant it, posting to both places would show every completion
/// twice.
///
/// So: one panel, no permission needed, same behaviour on every machine.
/// Clicking it reveals the file in Finder.
enum Notifier {
    static func configure() {
        // Nothing to set up — kept as the single call site in AppDelegate so
        // switching back to system notifications later is a one-file change.
    }

    static func downloadFinished(_ job: Job) {
        let path = (job.dest?.isEmpty == false) ? job.dest : nil
        Toast.show(title: "Download complete", body: job.filename) {
            guard let path, FileManager.default.fileExists(atPath: path) else { return }
            Reveal.inFinder(path)
        }
    }
}

/// Reveals a file in Finder without blocking the caller.
///
/// NSWorkspace.activateFileViewerSelecting sends the request to Finder and
/// waits for an answer. When Finder is busy it does not answer, and the call
/// sits there for a full 30 seconds ("didn't return pasteboard data in time").
/// On the main thread that freezes the whole app — the button looks broken and
/// nothing repaints — so the wait happens off it.
enum Reveal {
    static func inFinder(_ path: String) {
        guard !path.isEmpty else { return }
        let url = URL(fileURLWithPath: path)
        DispatchQueue.global(qos: .userInitiated).async {
            NSWorkspace.shared.activateFileViewerSelecting([url])
        }
    }
}
