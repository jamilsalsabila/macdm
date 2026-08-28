import AppKit

/// Per-download window modelled on IDM's progress dialog: file info block,
/// segmented progress bar, a "Show details" connection table, Pause/Cancel.
final class DownloadDetailWindowController: NSWindowController, NSTableViewDataSource, NSTableViewDelegate {
    private var job: Job
    var onClose: (() -> Void)?

    private let urlField = detailValueField()
    private let statusField = detailValueField()
    private let sizeField = detailValueField()
    private let doneField = detailValueField()
    private let rateField = detailValueField()
    private let etaField = detailValueField()
    private let resumeField = detailValueField()
    private let connStepper = NSStepper()
    private let connValue = NSTextField(labelWithString: "—")
    private var connEditing = false
    private let bar = SegmentedBar()
    private let connTable = NSTableView()
    private let detailBox = NSBox()
    private let pauseBtn = NSButton()
    private let cancelBtn = NSButton()
    private var detailShown = false

    init(job: Job) {
        self.job = job
        let win = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 520, height: 260),
                           styleMask: [.titled, .closable, .miniaturizable],
                           backing: .buffered, defer: false)
        super.init(window: win)
        win.delegate = self
        win.center()
        build()
        update(job)
    }
    required init?(coder: NSCoder) { fatalError() }

    func show() {
        showWindow(nil)
        window?.makeKeyAndOrderFront(nil)
        NSApp.activate(ignoringOtherApps: true)
    }

    private func build() {
        guard let win = window else { return }
        let root = NSView()

        func row(_ l: String, _ v: NSView) -> NSStackView {
            let s = NSStackView(views: [detailLabelField(l), v])
            s.orientation = .horizontal
            s.spacing = 8
            (s.views.first as? NSTextField)?.widthAnchor.constraint(equalToConstant: 110).isActive = true
            return s
        }

        connStepper.minValue = 1
        connStepper.maxValue = 32
        connStepper.target = self
        connStepper.action = #selector(connChanged)
        let connRow = NSStackView(views: [connStepper, connValue,
            NSTextField(labelWithString: "(changes apply live)")])
        connRow.spacing = 6
        (connRow.views.last as? NSTextField)?.textColor = .tertiaryLabelColor
        (connRow.views.last as? NSTextField)?.font = .systemFont(ofSize: 10)

        let info = NSStackView(views: [
            row("Address:", urlField),
            row("Status:", statusField),
            row("File size:", sizeField),
            row("Downloaded:", doneField),
            row("Transfer rate:", rateField),
            row("Time left:", etaField),
            row("Resume capability:", resumeField),
            row("Connections:", connRow),
        ])
        info.orientation = .vertical
        info.alignment = .leading
        info.spacing = 4
        info.translatesAutoresizingMaskIntoConstraints = false

        bar.translatesAutoresizingMaskIntoConstraints = false
        bar.heightAnchor.constraint(equalToConstant: 14).isActive = true

        let disclosure = NSButton(title: "Show details", target: self, action: #selector(toggleDetails))
        disclosure.bezelStyle = .disclosure
        disclosure.setButtonType(.pushOnPushOff)
        disclosure.translatesAutoresizingMaskIntoConstraints = false

        // connection table
        connTable.dataSource = self
        connTable.delegate = self
        connTable.rowHeight = 20
        for (id, title, w) in [("n", "#", CGFloat(30)), ("dl", "Downloaded", 140), ("info", "Info", 260)] {
            let col = NSTableColumn(identifier: .init(id)); col.title = title; col.width = w
            connTable.addTableColumn(col)
        }
        let connScroll = NSScrollView()
        connScroll.documentView = connTable
        connScroll.hasVerticalScroller = true
        detailBox.titlePosition = .noTitle
        detailBox.contentView = connScroll
        detailBox.translatesAutoresizingMaskIntoConstraints = false
        detailBox.isHidden = true
        detailBox.heightAnchor.constraint(equalToConstant: 120).isActive = true

        pauseBtn.title = "Pause"
        pauseBtn.bezelStyle = .rounded
        pauseBtn.target = self
        pauseBtn.action = #selector(togglePause)
        cancelBtn.bezelStyle = .rounded
        cancelBtn.target = self
        cancelBtn.action = #selector(cancel)
        let openBtn = NSButton(title: "Open folder", target: self, action: #selector(openFolder))
        openBtn.bezelStyle = .rounded
        let buttons = NSStackView(views: [openBtn, NSView(), pauseBtn, cancelBtn])
        buttons.orientation = .horizontal
        buttons.translatesAutoresizingMaskIntoConstraints = false

        let stack = NSStackView(views: [info, bar, disclosure, detailBox, buttons])
        stack.orientation = .vertical
        stack.alignment = .leading
        stack.spacing = 10
        stack.translatesAutoresizingMaskIntoConstraints = false
        root.addSubview(stack)
        NSLayoutConstraint.activate([
            stack.topAnchor.constraint(equalTo: root.topAnchor, constant: 16),
            stack.leadingAnchor.constraint(equalTo: root.leadingAnchor, constant: 16),
            stack.trailingAnchor.constraint(equalTo: root.trailingAnchor, constant: -16),
            stack.bottomAnchor.constraint(equalTo: root.bottomAnchor, constant: -16),
            bar.widthAnchor.constraint(equalTo: stack.widthAnchor),
            detailBox.widthAnchor.constraint(equalTo: stack.widthAnchor),
            buttons.widthAnchor.constraint(equalTo: stack.widthAnchor),
        ])
        win.contentView = root
    }

    func update(_ j: Job) {
        job = j
        window?.title = "\(Int(j.percent))%  MacDM — \(Fmt.short(j.filename, 50))"
        urlField.stringValue = Fmt.short(j.url)
        urlField.toolTip = j.url
        statusField.stringValue = statusText(j)

        sizeField.stringValue = j.streamingSegments ? "adaptive stream (\(j.total_bytes) segments)" : j.sizeText
        doneField.stringValue = j.doneText
        rateField.stringValue = j.status == "downloading" ? Fmt.speed(j.speed_bps) : "—"
        etaField.stringValue = j.etaText
        resumeField.stringValue = (j.resumable ?? false) ? "Yes" : "No"

        connStepper.isEnabled = (j.resumable ?? false) && j.kind == "http"
        if !connEditing {
            connStepper.integerValue = max(1, j.connections)
            connValue.stringValue = connStepper.isEnabled ? "\(max(1, j.connections))"
                : "\(max(1, j.connections)) (fixed)"
        }

        bar.set(total: j.total_bytes, conns: j.conns ?? [], percent: j.percent)
        connTable.reloadData()

        let running = j.isRunning
        let terminal = j.status == "completed" || j.status == "drm_protected" || j.status == "error"
        pauseBtn.title = running ? "Pause" : "Resume"
        pauseBtn.isHidden = terminal
        cancelBtn.title = terminal ? "Close" : "Cancel"
    }

    private func statusText(_ j: Job) -> String {
        switch j.status {
        case "downloading": return "Receiving data…"
        case "probing": return "Resolving…"
        case "queued": return "Queued"
        case "paused": return "Paused"
        case "completed": return "Complete"
        case "drm_protected": return "Cannot download — DRM protected"
        case "error": return "Error — \(j.error ?? "")"
        default: return j.status
        }
    }

    @objc private func toggleDetails(_ sender: NSButton) {
        detailShown.toggle()
        detailBox.isHidden = !detailShown
        sender.title = detailShown ? "Hide details" : "Show details"
        if let w = window {
            var f = w.frame
            f.size.height += detailShown ? 130 : -130
            f.origin.y -= detailShown ? 130 : -130
            w.setFrame(f, display: true, animate: true)
        }
    }

    @objc private func togglePause() {
        DaemonClient.shared.command(job.id, job.isRunning ? "pause" : "resume")
    }

    @objc private func connChanged() {
        connEditing = true
        connValue.stringValue = "\(connStepper.integerValue)"
        NSObject.cancelPreviousPerformRequests(withTarget: self, selector: #selector(commitConns), object: nil)
        perform(#selector(commitConns), with: nil, afterDelay: 0.4) // debounce
    }
    @objc private func commitConns() {
        DaemonClient.shared.setConns(job.id, connStepper.integerValue)
        connEditing = false
    }
    @objc private func cancel() {
        // "Cancel" on a running job pauses it; "Close" on a finished job just
        // dismisses the window (the file is already saved).
        if job.isRunning { DaemonClient.shared.command(job.id, "pause") }
        window?.close()
    }
    @objc private func openFolder() {
        let path = job.dest ?? ""
        if !path.isEmpty {
            NSWorkspace.shared.activateFileViewerSelecting([URL(fileURLWithPath: path)])
        }
    }

    // conn table
    func numberOfRows(in tableView: NSTableView) -> Int { job.conns?.count ?? 0 }
    func tableView(_ tv: NSTableView, objectValueFor column: NSTableColumn?, row: Int) -> Any? {
        guard let c = job.conns?[row] else { return nil }
        switch column?.identifier.rawValue {
        case "n": return c.index + 1
        case "dl": return c.total > 0 ? "\(Fmt.bytes(c.downloaded)) / \(Fmt.bytes(c.total))" : Fmt.bytes(c.downloaded)
        default: return c.info ?? c.status
        }
    }
}

extension DownloadDetailWindowController: NSWindowDelegate {
    func windowWillClose(_ notification: Notification) { onClose?() }
}

func detailValueField() -> NSTextField {
    let t = NSTextField(labelWithString: "—")
    t.lineBreakMode = .byTruncatingMiddle
    t.font = .systemFont(ofSize: 11)
    return t
}

func detailLabelField(_ s: String) -> NSTextField {
    let t = NSTextField(labelWithString: s)
    t.font = .systemFont(ofSize: 11)
    t.textColor = .secondaryLabelColor
    t.alignment = .right
    return t
}
