import AppKit

/// Preferences: connection count (IDM's "Max. connections number"), default
/// folder, and the auto-accept toggle. These are stored client-side in
/// UserDefaults and mirrored to the daemon's config where relevant.
final class SettingsWindowController: NSWindowController {
    static let shared = SettingsWindowController()

    private let connStepper = NSStepper()
    private let connLabel = NSTextField(labelWithString: "8")
    private let folderLabel = NSTextField(labelWithString: "")
    private let autoAccept = NSButton(checkboxWithTitle: "Skip the dialog — start caught downloads automatically", target: nil, action: nil)

    private var folder: String = (NSHomeDirectory() as NSString).appendingPathComponent("Downloads/MacDM")

    private convenience init() {
        let win = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 460, height: 220),
                           styleMask: [.titled, .closable], backing: .buffered, defer: false)
        win.title = "MacDM Settings"
        win.center()
        self.init(window: win)
        build()
        load()
    }

    func show() {
        showWindow(nil)
        window?.makeKeyAndOrderFront(nil)
        NSApp.activate(ignoringOtherApps: true)
    }

    private func build() {
        guard let win = window else { return }
        let root = NSView()

        connStepper.minValue = 1
        connStepper.maxValue = 32
        connStepper.integerValue = 8
        connStepper.target = self
        connStepper.action = #selector(connChanged)

        folderLabel.lineBreakMode = .byTruncatingMiddle
        let change = NSButton(title: "Change…", target: self, action: #selector(chooseFolder))
        change.bezelStyle = .rounded

        func labelled(_ s: String, _ v: NSView) -> NSStackView {
            let l = NSTextField(labelWithString: s)
            l.alignment = .right
            l.textColor = .secondaryLabelColor
            l.widthAnchor.constraint(equalToConstant: 150).isActive = true
            let st = NSStackView(views: [l, v])
            st.orientation = .horizontal
            st.spacing = 8
            return st
        }

        let connRow = NSStackView(views: [connStepper, connLabel,
            NSTextField(labelWithString: "(1–32, IDM default is 8)")])
        connRow.spacing = 6
        (connRow.views.last as? NSTextField)?.textColor = .secondaryLabelColor

        let save = NSButton(title: "Done", target: self, action: #selector(saveAndClose))
        save.keyEquivalent = "\r"
        save.bezelStyle = .rounded

        let stack = NSStackView(views: [
            labelled("Max. connections number:", connRow),
            labelled("Default download folder:", NSStackView(views: [folderLabel, change])),
            autoAccept,
            NSView(),
            NSStackView(views: [NSView(), save]),
        ])
        stack.orientation = .vertical
        stack.alignment = .leading
        stack.spacing = 14
        stack.translatesAutoresizingMaskIntoConstraints = false
        root.addSubview(stack)
        NSLayoutConstraint.activate([
            stack.topAnchor.constraint(equalTo: root.topAnchor, constant: 20),
            stack.leadingAnchor.constraint(equalTo: root.leadingAnchor, constant: 20),
            stack.trailingAnchor.constraint(equalTo: root.trailingAnchor, constant: -20),
            stack.bottomAnchor.constraint(equalTo: root.bottomAnchor, constant: -20),
        ])
        win.contentView = root
    }

    private func load() {
        let d = UserDefaults.standard
        let c = d.integer(forKey: "defaultConns")
        connStepper.integerValue = c == 0 ? 8 : c
        connLabel.stringValue = "\(connStepper.integerValue)"
        folder = d.string(forKey: "lastSaveFolder") ?? folder
        folderLabel.stringValue = (folder as NSString).abbreviatingWithTildeInPath
        autoAccept.state = d.bool(forKey: "autoAcceptHint") ? .on : .off
    }

    @objc private func connChanged() { connLabel.stringValue = "\(connStepper.integerValue)" }

    @objc private func chooseFolder() {
        let panel = NSOpenPanel()
        panel.canChooseDirectories = true
        panel.canChooseFiles = false
        panel.canCreateDirectories = true
        panel.directoryURL = URL(fileURLWithPath: folder)
        if panel.runModal() == .OK, let url = panel.url {
            folder = url.path
            folderLabel.stringValue = (folder as NSString).abbreviatingWithTildeInPath
        }
    }

    @objc private func saveAndClose() {
        let d = UserDefaults.standard
        d.set(connStepper.integerValue, forKey: "defaultConns")
        d.set(folder, forKey: "lastSaveFolder")
        d.set(autoAccept.state == .on, forKey: "autoAcceptHint")
        window?.close()
    }
}
