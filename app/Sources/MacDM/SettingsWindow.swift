import AppKit

/// Preferences: connection count (IDM's "Max. connections number"), default
/// folder, the auto-accept toggle, and the bundled-tools panel (yt-dlp version
/// + auto-update). Client prefs live in UserDefaults; the tool settings are
/// mirrored to the daemon's config.json via POST /api/config.
final class SettingsWindowController: NSWindowController {
    static let shared = SettingsWindowController()

    private let connStepper = NSStepper()
    private let connLabel = NSTextField(labelWithString: "8")
    private let folderLabel = NSTextField(labelWithString: "")
    private let autoAccept = NSButton(checkboxWithTitle: "Skip the dialog — start caught downloads automatically", target: nil, action: nil)

    private let ytdlpLabel = NSTextField(labelWithString: "yt-dlp: …")
    private let ffmpegLabel = NSTextField(labelWithString: "ffmpeg: …")
    private let autoUpdate = NSButton(checkboxWithTitle: "Keep yt-dlp updated automatically", target: nil, action: nil)
    private let updateBtn = NSButton(title: "Update now", target: nil, action: nil)
    private let channelPop = NSPopUpButton(frame: .zero, pullsDown: false)

    private var folder: String = (NSHomeDirectory() as NSString).appendingPathComponent("Downloads/MacDM")

    private convenience init() {
        let win = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 480, height: 320),
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
        refreshTools()
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

        ytdlpLabel.textColor = .secondaryLabelColor
        ffmpegLabel.textColor = .secondaryLabelColor
        autoUpdate.target = self
        autoUpdate.action = #selector(autoUpdateToggled)
        updateBtn.target = self
        updateBtn.action = #selector(updateNow)
        updateBtn.bezelStyle = .rounded

        channelPop.addItems(withTitles: ["nightly (recommended)", "stable"])
        channelPop.target = self
        channelPop.action = #selector(channelChanged)
        let channelRow = NSStackView(views: [NSTextField(labelWithString: "Channel:"), channelPop])
        channelRow.spacing = 6

        let sep = NSBox()
        sep.boxType = .separator

        let toolsBox = NSStackView(views: [
            ytdlpLabel,
            NSStackView(views: [autoUpdate, updateBtn]),
            channelRow,
            ffmpegLabel,
        ])
        toolsBox.orientation = .vertical
        toolsBox.alignment = .leading
        toolsBox.spacing = 8

        let save = NSButton(title: "Done", target: self, action: #selector(saveAndClose))
        save.keyEquivalent = "\r"
        save.bezelStyle = .rounded

        let stack = NSStackView(views: [
            labelled("Max. connections number:", connRow),
            labelled("Default download folder:", NSStackView(views: [folderLabel, change])),
            autoAccept,
            sep,
            labelled("Bundled tools:", toolsBox),
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
            sep.widthAnchor.constraint(equalTo: stack.widthAnchor),
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

    private func refreshTools() {
        DaemonClient.shared.fetchTools { [weak self] info in
            guard let self = self, let info = info else { return }
            let yt = info.ytdlp
            var s = "yt-dlp: " + (yt.version.isEmpty ? "not installed" : yt.version)
            if !yt.latest.isEmpty && yt.update_available {
                s += "  (latest \(yt.latest))"
            } else if !yt.latest.isEmpty {
                s += "  (up to date)"
            }
            self.ytdlpLabel.stringValue = s
            self.ffmpegLabel.stringValue = "ffmpeg: " + (info.ffmpeg.version.isEmpty ? "not found" : info.ffmpeg.version)
            self.autoUpdate.state = info.auto_update ? .on : .off
            self.channelPop.selectItem(at: (yt.channel == "stable") ? 1 : 0)
        }
    }

    @objc private func connChanged() { connLabel.stringValue = "\(connStepper.integerValue)" }

    @objc private func autoUpdateToggled() {
        DaemonClient.shared.setConfig(["auto_update_ytdlp": autoUpdate.state == .on])
    }

    @objc private func channelChanged() {
        DaemonClient.shared.setConfig(["ytdlp_channel": channelPop.indexOfSelectedItem == 1 ? "stable" : "nightly"])
    }

    @objc private func updateNow() {
        updateBtn.isEnabled = false
        updateBtn.title = "Updating…"
        DaemonClient.shared.updateYtDlp { [weak self] _, error in
            guard let self = self else { return }
            self.updateBtn.title = "Update now"
            self.updateBtn.isEnabled = true
            if let msg = error {
                let a = NSAlert()
                a.messageText = "yt-dlp update failed"
                a.informativeText = msg
                a.runModal()
            } else {
                self.refreshTools()
            }
        }
    }

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
