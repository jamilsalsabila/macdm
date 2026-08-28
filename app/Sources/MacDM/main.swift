import AppKit

// MacDM menu-bar app entry point. Runs as an accessory (no Dock icon, no menu
// bar name) so it lives purely in the status bar like a typical utility.
let app = NSApplication.shared
app.setActivationPolicy(.accessory)

let delegate = AppDelegate()
app.delegate = delegate
app.run()
