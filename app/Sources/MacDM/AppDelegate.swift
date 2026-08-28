import AppKit

final class AppDelegate: NSObject, NSApplicationDelegate {
    private var statusItem: NSStatusItem!
    private let daemon = DaemonProcess()
    private lazy var mainWindow = MainWindowController()
    private var connected = false
    private var recentJobs: [Job] = []

    private let addr = UserDefaults.standard.string(forKey: "addr") ?? "127.0.0.1:7345"

    func applicationDidFinishLaunching(_ note: Notification) {
        FirstRunSetup.run()
        DaemonClient.shared.configure(addr: addr)

        statusItem = NSStatusBar.system.statusItem(withLength: NSStatusItem.variableLength)
        if let b = statusItem.button {
            b.image = NSImage(systemSymbolName: "arrow.down.circle", accessibilityDescription: "MacDM")
            b.image?.isTemplate = true
        }
        statusItem.menu = buildMenu()

        DaemonClient.shared.onConnection = { [weak self] up in
            self?.connected = up
            self?.refreshMenu()
        }
        DaemonClient.shared.onJobs { [weak self] jobs in
            self?.recentJobs = jobs
            self?.refreshMenu()
        }
        DaemonClient.shared.onProposal = { proposal in
            NewDownloadDialog.present(proposal)
        }

        daemon.ensureRunning(addr: addr) { _ in
            DaemonClient.shared.startStream()
        }
    }

    func applicationWillTerminate(_ note: Notification) {
        DaemonClient.shared.stopStream()
        // leave the daemon running so downloads survive the UI quitting
    }

    // MARK: menu

    private func buildMenu() -> NSMenu {
        let m = NSMenu()
        m.autoenablesItems = false
        m.delegate = self
        return m
    }

    private func refreshMenu() {
        guard let m = statusItem.menu else { return }
        m.removeAllItems()

        let status = NSMenuItem(title: connected ? "Daemon connected" : "Daemon offline", action: nil, keyEquivalent: "")
        status.isEnabled = false
        m.addItem(status)
        m.addItem(.separator())

        m.addItem(withTitle: "Open MacDM", action: #selector(openMain), keyEquivalent: "o").target = self
        m.addItem(withTitle: "Add URL…", action: #selector(addURL), keyEquivalent: "n").target = self
        m.addItem(withTitle: "Settings…", action: #selector(openSettings), keyEquivalent: ",").target = self

        let active = recentJobs.filter { $0.isRunning }
        if !active.isEmpty {
            m.addItem(.separator())
            let hdr = NSMenuItem(title: "Downloading", action: nil, keyEquivalent: "")
            hdr.isEnabled = false
            m.addItem(hdr)
            for j in active.prefix(6) {
                let name = j.filename.count > 34
                    ? j.filename.prefix(31) + "…" : Substring(j.filename)
                let it = NSMenuItem(
                    title: "   \(name) — \(Int(j.percent))%  \(Fmt.speed(j.speed_bps))",
                    action: #selector(openJobDetail(_:)), keyEquivalent: "")
                it.toolTip = j.filename
                it.target = self
                it.representedObject = j.id
                m.addItem(it)
            }
        }

        m.addItem(.separator())
        m.addItem(withTitle: "Quit MacDM", action: #selector(NSApplication.terminate(_:)), keyEquivalent: "q")
    }

    @objc private func openMain() { mainWindow.show() }
    @objc private func openJobDetail(_ sender: NSMenuItem) {
        guard let id = sender.representedObject as? String else { return }
        mainWindow.showDetail(jobID: id)
    }
    @objc private func openSettings() { SettingsWindowController.shared.show() }
    @objc private func addURL() {
        mainWindow.show()
        mainWindow.perform(NSSelectorFromString("addURL"))
    }
}

extension AppDelegate: NSMenuDelegate {
    func menuNeedsUpdate(_ menu: NSMenu) { refreshMenu() }
}
