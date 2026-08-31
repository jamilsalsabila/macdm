import AppKit

/// The IDM-style "New Download" dialog. Shown when the extension catches a
/// download (SSE `proposal` event) or when the user pastes a URL.
///
/// A caught proposal is emitted twice — once immediately, once after the
/// background probe resolves quality/size. `present` opens the dialog on the
/// first and just refreshes the quality menu on the second.
final class NewDownloadDialog: NSWindowController, NSWindowDelegate {
    private var latest: Proposal
    private let isManual: Bool

    private let nameField = NSTextField()
    private let folderLabel = NSTextField(labelWithString: "")
    private let urlField = NSTextField(labelWithString: "")
    private let categoryField = NSTextField(labelWithString: "")
    private let qualityMenu = NSPopUpButton()
    private let audioMenu = NSPopUpButton()
    private let subsMenu = NSPopUpButton()
    /// Language rows are hidden unless the video actually offers a choice, so a
    /// normal download is not cluttered with two useless menus.
    private var audioRow: NSStackView?
    private var subsRow: NSStackView?
    private var audioCodes: [String] = []
    private var subCodes: [String] = []
    private let connStepper = NSStepper()
    private let connLabel = NSTextField(labelWithString: "")
    private let dlButton = NSButton()
    private let laterButton = NSButton()

    private var folder: String {
        didSet { folderLabel.stringValue = (folder as NSString).abbreviatingWithTildeInPath }
    }

    private static let lastFolderKey = "lastSaveFolder"
    private static let connKey = "defaultConns"
    private static var open: [String: NewDownloadDialog] = [:]
    private var selfRef: NewDownloadDialog?

    // MARK: entry points

    static func present(_ p: Proposal) {
        if let existing = open[p.id] { existing.refresh(p); return }
        NSApp.activate(ignoringOtherApps: true)
        let dlg = NewDownloadDialog(proposal: p, manual: false)
        open[p.id] = dlg
        dlg.selfRef = dlg
        dlg.showWindow(nil)
        dlg.window?.makeKeyAndOrderFront(nil)
    }

    static func presentForManualAdd(_ p: Proposal) {
        NSApp.activate(ignoringOtherApps: true)
        let dlg = NewDownloadDialog(proposal: p, manual: true)
        dlg.selfRef = dlg
        dlg.showWindow(nil)
        dlg.window?.makeKeyAndOrderFront(nil)
    }

    // MARK: init / layout

    private init(proposal: Proposal, manual: Bool) {
        self.latest = proposal
        self.isManual = manual
        self.folder = UserDefaults.standard.string(forKey: Self.lastFolderKey)
            ?? (NSHomeDirectory() as NSString).appendingPathComponent("Downloads/MacDM")
        let win = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 470, height: 300),
                           styleMask: [.titled, .closable], backing: .buffered, defer: false)
        super.init(window: win)
        win.delegate = self
        win.title = proposal.drm ? "Cannot download — DRM protected" : "New Download"
        win.center()
        build()
        rebuildQualityMenu()
        rebuildLanguageMenus()
    }
    required init?(coder: NSCoder) { fatalError() }

    private func labelled(_ s: String, _ v: NSView) -> NSStackView {
        let l = NSTextField(labelWithString: s)
        l.alignment = .right
        l.textColor = .secondaryLabelColor
        l.widthAnchor.constraint(equalToConstant: 92).isActive = true
        let st = NSStackView(views: [l, v])
        st.orientation = .horizontal
        st.spacing = 8
        return st
    }

    private func build() {
        guard let win = window else { return }
        let root = NSView()

        nameField.stringValue = latest.filename
        nameField.target = self
        nameField.action = #selector(nameEdited)
        urlField.stringValue = Fmt.short(latest.url)
        urlField.toolTip = latest.url
        urlField.lineBreakMode = .byTruncatingMiddle
        urlField.textColor = .secondaryLabelColor
        urlField.font = .systemFont(ofSize: 10)
        categoryField.stringValue = category()
        categoryField.textColor = .secondaryLabelColor
        folderLabel.stringValue = (folder as NSString).abbreviatingWithTildeInPath

        let defConns = UserDefaults.standard.integer(forKey: Self.connKey)
        connStepper.minValue = 1
        connStepper.maxValue = 32
        connStepper.integerValue = latest.resumable ? (defConns == 0 ? 8 : defConns) : 1
        connStepper.isEnabled = latest.resumable
        connStepper.target = self
        connStepper.action = #selector(connChanged)
        connLabel.stringValue = "\(connStepper.integerValue)"

        let chooseBtn = NSButton(title: "Change…", target: self, action: #selector(chooseFolder))
        chooseBtn.bezelStyle = .rounded

        let connNote = NSTextField(labelWithString: latest.resumable ? "connections" : "(no resume — 1)")
        connNote.textColor = .secondaryLabelColor
        let connRow = NSStackView(views: [connStepper, connLabel, connNote])
        connRow.spacing = 6

        let folderRow = NSStackView(views: [folderLabel, chooseBtn])
        folderRow.spacing = 8

        dlButton.title = "Download"
        dlButton.keyEquivalent = "\r"
        dlButton.bezelStyle = .rounded
        dlButton.target = self
        dlButton.action = #selector(download)
        laterButton.title = "Download Later"
        laterButton.bezelStyle = .rounded
        laterButton.target = self
        laterButton.action = #selector(downloadLater)
        let cancelBtn = NSButton(title: "Cancel", target: self, action: #selector(cancelBtn))
        cancelBtn.bezelStyle = .rounded
        let btns = NSStackView(views: [NSView(), laterButton, cancelBtn, dlButton])
        btns.orientation = .horizontal

        var rows: [NSView] = [
            labelled("File Name:", nameField),
            labelled("Save to:", folderRow),
            labelled("Category:", categoryField),
            labelled("Quality:", qualityMenu),
            { let r = labelled("Audio language:", audioMenu); audioRow = r; return r }(),
            { let r = labelled("Subtitles:", subsMenu); subsRow = r; return r }(),
            labelled("Connections:", connRow),
            labelled("URL:", urlField),
        ]
        if latest.drm {
            let warn = NSTextField(wrappingLabelWithString:
                "This stream is DRM-protected. MacDM does not remove DRM, so it cannot be downloaded.")
            warn.textColor = .systemRed
            rows = [warn]
            dlButton.isEnabled = false
            laterButton.isEnabled = false
        }
        rows.append(btns)

        let stack = NSStackView(views: rows)
        stack.orientation = .vertical
        stack.alignment = .leading
        stack.spacing = 10
        stack.translatesAutoresizingMaskIntoConstraints = false
        root.addSubview(stack)
        NSLayoutConstraint.activate([
            stack.topAnchor.constraint(equalTo: root.topAnchor, constant: 18),
            stack.leadingAnchor.constraint(equalTo: root.leadingAnchor, constant: 18),
            stack.trailingAnchor.constraint(equalTo: root.trailingAnchor, constant: -18),
            stack.bottomAnchor.constraint(equalTo: root.bottomAnchor, constant: -18),
            nameField.widthAnchor.constraint(equalToConstant: 320),
            qualityMenu.widthAnchor.constraint(equalToConstant: 250),
            audioMenu.widthAnchor.constraint(equalToConstant: 250),
            subsMenu.widthAnchor.constraint(equalToConstant: 250),
            btns.widthAnchor.constraint(equalTo: stack.widthAnchor),
        ])
        win.contentView = root
    }

    // MARK: two-phase refresh

    private var userEditedName = false

    func refresh(_ p: Proposal) {
        guard !isManual else { return }
        latest = p
        if !userEditedName, p.filename != nameField.stringValue, looksGeneric(nameField.stringValue) {
            nameField.stringValue = p.filename
        }
        categoryField.stringValue = category()
        if p.resumable != connStepper.isEnabled {
            connStepper.isEnabled = p.resumable
            if !p.resumable { connStepper.integerValue = 1; connLabel.stringValue = "1" }
        }
        rebuildQualityMenu()
        rebuildLanguageMenus()
        if p.drm {
            window?.title = "Cannot download — DRM protected"
            dlButton.isEnabled = false
            laterButton.isEnabled = false
        }
    }

    /// Fills the language menus from what the probe found. Both rows stay
    /// hidden when the site offers no alternatives — most videos have one
    /// soundtrack and no subtitles, and empty pickers only confuse.
    private func rebuildLanguageMenus() {
        audioCodes = latest.audio_langs ?? []
        subCodes = latest.sub_langs ?? []

        let keepAudio = audioMenu.indexOfSelectedItem
        audioMenu.removeAllItems()
        audioMenu.addItem(withTitle: "Default")
        for c in audioCodes { audioMenu.addItem(withTitle: languageName(c)) }
        if keepAudio > 0 && keepAudio < audioMenu.numberOfItems {
            audioMenu.selectItem(at: keepAudio)
        }
        audioRow?.isHidden = audioCodes.isEmpty

        let keepSubs = subsMenu.indexOfSelectedItem
        subsMenu.removeAllItems()
        subsMenu.addItem(withTitle: "None")
        for c in subCodes { subsMenu.addItem(withTitle: languageName(c)) }
        if keepSubs > 0 && keepSubs < subsMenu.numberOfItems {
            subsMenu.selectItem(at: keepSubs)
        }
        subsRow?.isHidden = subCodes.isEmpty

        window?.setContentSize(window?.contentView?.fittingSize ?? .zero)
    }

    /// "id" -> "Indonesian (id)". Falls back to the raw tag when macOS has no
    /// name for it, so an unusual code is still selectable.
    private func languageName(_ code: String) -> String {
        let name = Locale.current.localizedString(forIdentifier: code)
            ?? Locale.current.localizedString(forLanguageCode: code)
        return name.map { "\($0) (\(code))" } ?? code
    }

    /// The chosen tags, or nil for "leave it to the Settings default".
    private var chosenAudioLang: String? {
        let i = audioMenu.indexOfSelectedItem
        return i > 0 && i - 1 < audioCodes.count ? audioCodes[i - 1] : nil
    }
    private var chosenSubLangs: String? {
        let i = subsMenu.indexOfSelectedItem
        return i > 0 && i - 1 < subCodes.count ? subCodes[i - 1] : nil
    }

    private func rebuildQualityMenu() {
        let formats = latest.formats ?? []
        let probing = latest.probing ?? false
        // Keep the user's pick when the menu is rebuilt (the real formats arrive
        // a few seconds after the static ladder).
        let wasChosen = qualityMenu.numberOfItems > 0 ? qualityMenu.titleOfSelectedItem : nil
        let chosenLabel = wasChosen.map { $0.components(separatedBy: "  (~").first ?? $0 }
        qualityMenu.removeAllItems()
        if formats.isEmpty {
            qualityMenu.addItem(withTitle: probing ? "Detecting… (Download works now)" : "Best available")
            qualityMenu.isEnabled = false
        } else {
            for f in formats {
                let size = (f.size_bytes ?? 0) > 0 ? "  (~\(Fmt.bytes(f.size_bytes!)))" : ""
                qualityMenu.addItem(withTitle: f.label + size)
            }
            qualityMenu.isEnabled = true
            if let want = chosenLabel,
               let idx = formats.firstIndex(where: { $0.label == want }) {
                qualityMenu.selectItem(at: idx)
            } else if let idx = formats.firstIndex(where: { ($0.height ?? 0) == 1080 }) {
                qualityMenu.selectItem(at: idx)
            }
        }
    }

    private func category() -> String {
        if let c = latest.category, !c.isEmpty { return c.capitalized }
        switch latest.kind {
        case "extract", "hls", "dash": return "Video"
        default: return "General"
        }
    }

    // MARK: actions

    @objc private func connChanged() { connLabel.stringValue = "\(connStepper.integerValue)" }
    @objc private func nameEdited() { userEditedName = true }

    private func looksGeneric(_ name: String) -> Bool {
        let base = (name as NSString).deletingPathExtension.lowercased().trimmingCharacters(in: .whitespaces)
        if ["", "watch", "download", "index", "video", "media", "playlist", "master", "manifest"].contains(base) { return true }
        return base.contains("watch?v=")
    }

    @objc private func chooseFolder() {
        let panel = NSOpenPanel()
        panel.canChooseDirectories = true
        panel.canChooseFiles = false
        panel.canCreateDirectories = true
        panel.directoryURL = URL(fileURLWithPath: folder)
        if panel.runModal() == .OK, let url = panel.url { folder = url.path }
    }

    private func chosenFormat() -> (id: String, label: String)? {
        guard let formats = latest.formats, !formats.isEmpty else { return nil }
        let i = qualityMenu.indexOfSelectedItem
        guard i >= 0 && i < formats.count else { return nil }
        return (formats[i].id, formats[i].label)
    }

    @objc private func download() { commit() }
    @objc private func downloadLater() { commit() } // job is created queued; user resumes from the list

    private func commit() {
        let d = UserDefaults.standard
        d.set(folder, forKey: Self.lastFolderKey)
        d.set(connStepper.integerValue, forKey: Self.connKey)

        let name = nameField.stringValue.trimmingCharacters(in: .whitespacesAndNewlines)
        let f = chosenFormat()
        // Pressing Download should land you on the IDM-style progress window.
        // Both paths used to discard the job the daemon returned, so nothing
        // opened and the download ran invisibly until you found it in the list.
        let showProgress: (Job?) -> Void = { job in
            guard let job else { return }
            MainWindowController.shared?.showDetail(job: job)
        }

        if isManual {
            // The File Name field is editable in this dialog too — it used to be
            // collected here and then dropped, so a rename silently did nothing.
            DaemonClient.shared.add(url: latest.url, dest: folder, conns: connStepper.integerValue,
                                    formatID: f?.id, quality: f?.label,
                                    filename: name.isEmpty ? nil : name,
                                    audioLang: chosenAudioLang, subLangs: chosenSubLangs) { result in
                showProgress(try? result.get())
            }
        } else {
            DaemonClient.shared.accept(latest.id, dest: folder,
                                       filename: name.isEmpty ? nil : name,
                                       conns: connStepper.integerValue,
                                       formatID: f?.id, quality: f?.label,
                                       audioLang: chosenAudioLang, subLangs: chosenSubLangs,
                                       completion: showProgress)
        }
        dismiss()
    }

    @objc private func cancelBtn() {
        if !isManual { DaemonClient.shared.reject(latest.id) }
        dismiss()
    }

    private func dismiss() {
        Self.open.removeValue(forKey: latest.id)
        window?.close()
        selfRef = nil
    }

    func windowWillClose(_ notification: Notification) {
        Self.open.removeValue(forKey: latest.id)
        selfRef = nil
    }
}
