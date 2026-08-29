import AppKit

// MacDM entry point. A regular app: Dock icon + menu bar, and it also keeps a
// status-bar item for quick access while downloads run in the background.
let app = NSApplication.shared
app.setActivationPolicy(.regular)

let delegate = AppDelegate()
app.delegate = delegate
app.run()
