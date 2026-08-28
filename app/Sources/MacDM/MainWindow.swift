import AppKit

/// The main MacDM window: a toolbar and a table of all downloads. Double-click a
/// row to open its detail window (the IDM-style per-download progress view).
final class MainWindowController: NSWindowController, NSTableViewDataSource, NSTableViewDelegate {
    private let table = NSTableView()
    private var jobs: [Job] = []
    private var details: [String: DownloadDetailWindowController] = [:]

    convenience init() {
        let win = NSWindow(
            contentRect: NSRect(x: 0, y: 0, width: 720, height: 380),
            styleMask: [.titled, .closable, .miniaturizable, .resizable],
            backing: .buffered, defer: false)
        win.title = "MacDM"
        win.center()
        win.setFrameAutosaveName("MacDMMain")
        self.init(window: win)
        buildUI()

        DaemonClient.shared.onJobs { [weak self] jobs in self?.apply(jobs) }
    }

    private func buildUI() {
        guard let win = window else { return }

        let toolbar = NSToolbar(identifier: "MacDMToolbar")
        toolbar.delegate = self
        toolbar.displayMode = .iconAndLabel
        win.toolbar = toolbar

        // table
        table.dataSource = self
        table.delegate = self
        table.usesAlternatingRowBackgroundColors = true
        table.rowHeight = 40
        table.target = self
        table.doubleAction = #selector(openDetail)
        table.columnAutoresizingStyle = .uniformColumnAutoresizingStyle

        for (id, title, width) in [
            ("file", "File", CGFloat(260)), ("size", "Size", 110), ("status", "Status", 90),
            ("speed", "Speed", 90), ("eta", "Time left", 90), ("pct", "%", 70),
        ] {
            let col = NSTableColumn(identifier: .init(id))
            col.title = title
            col.width = width
            table.addTableColumn(col)
        }

        let scroll = NSScrollView()
        scroll.documentView = table
        scroll.hasVerticalScroller = true
        scroll.autohidesScrollers = true
        scroll.translatesAutoresizingMaskIntoConstraints = false

        let content = NSView()
        content.addSubview(scroll)
        NSLayoutConstraint.activate([
            scroll.topAnchor.constraint(equalTo: content.topAnchor),
            scroll.leadingAnchor.constraint(equalTo: content.leadingAnchor),
            scroll.trailingAnchor.constraint(equalTo: content.trailingAnchor),
            scroll.bottomAnchor.constraint(equalTo: content.bottomAnchor),
        ])
        win.contentView = content
    }

    func show() {
        showWindow(nil)
        window?.makeKeyAndOrderFront(nil)
        NSApp.activate(ignoringOtherApps: true)
    }

    private func apply(_ jobs: [Job]) {
        self.jobs = jobs
        table.reloadData()
        for (id, ctl) in details {
            if let j = jobs.first(where: { $0.id == id }) { ctl.update(j) }
        }
    }

    private var selectedJob: Job? {
        table.selectedRow >= 0 && table.selectedRow < jobs.count ? jobs[table.selectedRow] : nil
    }

    // MARK: actions

    @objc func addURL() {
        let alert = NSAlert()
        alert.messageText = "Add download"
        alert.informativeText = "Paste a file or page URL:"
        let field = NSTextField(frame: NSRect(x: 0, y: 0, width: 380, height: 24))
        field.placeholderString = "https://…"
        alert.accessoryView = field
        alert.addButton(withTitle: "Add")
        alert.addButton(withTitle: "Cancel")
        if alert.runModal() == .alertFirstButtonReturn {
            let url = field.stringValue.trimmingCharacters(in: .whitespacesAndNewlines)
            guard !url.isEmpty else { return }
            // Route through a proposal so the quality/connection dialog appears.
            DaemonClient.shared.probe(url) { probe in
                let p = Proposal(id: "", url: url, kind: probe?.kind ?? "http",
                                 category: nil, title: probe?.title,
                                 filename: probe?.filename ?? "download",
                                 size: probe?.size ?? 0, resumable: probe?.resumable ?? false,
                                 drm: probe?.drm ?? false, formats: probe?.formats)
                NewDownloadDialog.presentForManualAdd(p)
            }
        }
    }

    @objc private func openDetail() {
        guard let j = selectedJob ?? (table.clickedRow >= 0 ? jobs[table.clickedRow] : nil) else { return }
        if let existing = details[j.id] { existing.show(); return }
        let ctl = DownloadDetailWindowController(job: j)
        ctl.onClose = { [weak self] in self?.details[j.id] = nil }
        details[j.id] = ctl
        ctl.show()
    }

    @objc private func pauseSel() { if let j = selectedJob { DaemonClient.shared.command(j.id, "pause") } }
    @objc private func resumeSel() { if let j = selectedJob { DaemonClient.shared.command(j.id, "resume") } }
    @objc private func removeSel() { if let j = selectedJob { DaemonClient.shared.remove(j.id) } }
    @objc private func openSettings() { SettingsWindowController.shared.show() }

    // MARK: table

    func numberOfRows(in tableView: NSTableView) -> Int { jobs.count }

    func tableView(_ tv: NSTableView, viewFor column: NSTableColumn?, row: Int) -> NSView? {
        let j = jobs[row]
        let id = column?.identifier.rawValue ?? ""
        let cell = (tv.makeView(withIdentifier: .init(id), owner: self) as? NSTableCellView) ?? {
            let c = NSTableCellView()
            let tf = NSTextField(labelWithString: "")
            tf.translatesAutoresizingMaskIntoConstraints = false
            tf.lineBreakMode = .byTruncatingMiddle
            c.addSubview(tf)
            c.textField = tf
            NSLayoutConstraint.activate([
                tf.leadingAnchor.constraint(equalTo: c.leadingAnchor, constant: 4),
                tf.trailingAnchor.constraint(equalTo: c.trailingAnchor, constant: -4),
                tf.centerYAnchor.constraint(equalTo: c.centerYAnchor),
            ])
            c.identifier = .init(id)
            return c
        }()

        switch id {
        case "file":
            cell.textField?.stringValue = j.filename.isEmpty ? j.url : j.filename
            if let q = j.quality, !q.isEmpty { cell.textField?.stringValue += "  (\(q))" }
        case "size":
            cell.textField?.stringValue = j.sizeText
        case "status":
            cell.textField?.stringValue = j.status
        case "speed":
            cell.textField?.stringValue = j.status == "downloading" ? Fmt.speed(j.speed_bps) : "—"
        case "eta":
            cell.textField?.stringValue = j.etaText
        case "pct":
            cell.textField?.stringValue = String(format: "%.0f%%", j.percent)
        default: break
        }
        return cell
    }
}

extension MainWindowController: NSToolbarDelegate {
    func toolbarDefaultItemIdentifiers(_ t: NSToolbar) -> [NSToolbarItem.Identifier] {
        [.init("add"), .init("resume"), .init("pause"), .init("remove"), .flexibleSpace, .init("settings")]
    }
    func toolbarAllowedItemIdentifiers(_ t: NSToolbar) -> [NSToolbarItem.Identifier] {
        toolbarDefaultItemIdentifiers(t)
    }
    func toolbar(_ t: NSToolbar, itemForItemIdentifier id: NSToolbarItem.Identifier,
                willBeInsertedIntoToolbar flag: Bool) -> NSToolbarItem? {
        let item = NSToolbarItem(itemIdentifier: id)
        item.target = self
        switch id.rawValue {
        case "add": item.label = "Add URL"; item.image = sym("plus"); item.action = #selector(addURL)
        case "resume": item.label = "Resume"; item.image = sym("play.fill"); item.action = #selector(resumeSel)
        case "pause": item.label = "Pause"; item.image = sym("pause.fill"); item.action = #selector(pauseSel)
        case "remove": item.label = "Remove"; item.image = sym("trash"); item.action = #selector(removeSel)
        case "settings": item.label = "Settings"; item.image = sym("gearshape"); item.action = #selector(openSettings)
        default: return nil
        }
        return item
    }
    private func sym(_ n: String) -> NSImage? {
        NSImage(systemSymbolName: n, accessibilityDescription: nil)
    }
}
