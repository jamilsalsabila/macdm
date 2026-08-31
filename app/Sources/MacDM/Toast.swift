import AppKit

/// An in-app notification panel, drawn by MacDM itself in the top-right corner.
///
/// macOS refuses `UNUserNotificationCenter` to an ad-hoc signed app, and the
/// legacy `NSUserNotification` fallback proved unreliable too, so system
/// notifications cannot be depended on here at all. This panel needs no
/// permission and no signing identity: it is just a window we own.
///
/// Click it to reveal the finished file in Finder. Panels stack downward and
/// fade out on their own.
final class Toast: NSWindowController {
    private static var live: [Toast] = []
    private static let width: CGFloat = 340
    private static let gap: CGFloat = 10

    private var onClick: (() -> Void)?
    private var dismissTimer: Timer?

    /// Shows a panel. Safe to call from any thread.
    static func show(title: String, body: String, onClick: (() -> Void)? = nil) {
        if Thread.isMainThread {
            present(title: title, body: body, onClick: onClick)
        } else {
            DispatchQueue.main.async { present(title: title, body: body, onClick: onClick) }
        }
    }

    private static func present(title: String, body: String, onClick: (() -> Void)?) {
        let t = Toast(title: title, body: body, onClick: onClick)
        live.append(t)
        if live.count > 4 { live.first?.close() } // don't wallpaper the screen
        layout()
        t.showWindow(nil)
        t.fade(to: 1)
        t.dismissTimer = Timer.scheduledTimer(withTimeInterval: 6, repeats: false) { [weak t] _ in
            t?.close()
        }
    }

    /// Re-stacks the visible panels under the menu bar, newest at the top.
    private static func layout() {
        guard let screen = NSScreen.main else { return }
        let vf = screen.visibleFrame
        var y = vf.maxY - gap
        for t in live {
            guard let w = t.window else { continue }
            let h = w.frame.height
            w.setFrameOrigin(NSPoint(x: vf.maxX - width - gap, y: y - h))
            y -= h + gap
        }
    }

    private init(title: String, body: String, onClick: (() -> Void)?) {
        self.onClick = onClick

        let titleLabel = NSTextField(labelWithString: title)
        titleLabel.font = .systemFont(ofSize: 13, weight: .semibold)
        let bodyLabel = NSTextField(labelWithString: body)
        bodyLabel.font = .systemFont(ofSize: 12)
        bodyLabel.textColor = .secondaryLabelColor
        bodyLabel.lineBreakMode = .byTruncatingMiddle
        bodyLabel.maximumNumberOfLines = 2
        bodyLabel.cell?.wraps = true
        bodyLabel.toolTip = body

        let icon = NSImageView(image: NSApp.applicationIconImage)
        icon.imageScaling = .scaleProportionallyUpOrDown
        icon.translatesAutoresizingMaskIntoConstraints = false
        icon.widthAnchor.constraint(equalToConstant: 36).isActive = true
        icon.heightAnchor.constraint(equalToConstant: 36).isActive = true

        let text = NSStackView(views: [titleLabel, bodyLabel])
        text.orientation = .vertical
        text.alignment = .leading
        text.spacing = 2

        let row = NSStackView(views: [icon, text])
        row.orientation = .horizontal
        row.alignment = .centerY
        row.spacing = 10
        row.edgeInsets = NSEdgeInsets(top: 12, left: 12, bottom: 12, right: 12)
        row.translatesAutoresizingMaskIntoConstraints = false

        let bg = NSVisualEffectView()
        bg.material = .popover
        bg.blendingMode = .behindWindow
        bg.state = .active
        bg.wantsLayer = true
        bg.layer?.cornerRadius = 12
        bg.addSubview(row)
        NSLayoutConstraint.activate([
            row.leadingAnchor.constraint(equalTo: bg.leadingAnchor),
            row.trailingAnchor.constraint(equalTo: bg.trailingAnchor),
            row.topAnchor.constraint(equalTo: bg.topAnchor),
            row.bottomAnchor.constraint(equalTo: bg.bottomAnchor),
            bg.widthAnchor.constraint(equalToConstant: Toast.width),
        ])

        let fitting = bg.fittingSize
        let win = NSPanel(contentRect: NSRect(x: 0, y: 0, width: Toast.width,
                                              height: max(fitting.height, 60)),
                          styleMask: [.borderless, .nonactivatingPanel],
                          backing: .buffered, defer: false)
        win.isOpaque = false
        win.backgroundColor = .clear
        win.hasShadow = true
        win.level = .floating
        win.alphaValue = 0
        // Visible on every Space, and never steals focus from what you're doing.
        win.collectionBehavior = [.canJoinAllSpaces, .fullScreenAuxiliary, .stationary]
        win.ignoresMouseEvents = false
        win.contentView = bg

        super.init(window: win)

        let click = NSClickGestureRecognizer(target: self, action: #selector(clicked))
        bg.addGestureRecognizer(click)
    }

    required init?(coder: NSCoder) { fatalError() }

    @objc private func clicked() {
        onClick?()
        close()
    }

    private func fade(to alpha: CGFloat, then: (() -> Void)? = nil) {
        NSAnimationContext.runAnimationGroup({ ctx in
            ctx.duration = 0.18
            window?.animator().alphaValue = alpha
        }, completionHandler: then)
    }

    override func close() {
        dismissTimer?.invalidate()
        dismissTimer = nil
        guard Toast.live.contains(where: { $0 === self }) else { return }
        Toast.live.removeAll { $0 === self }
        // Capture self strongly for the duration of the fade. `live` held the
        // only strong reference, so a weak capture deallocates before the
        // completion runs, orderOut never happens, and an invisible window is
        // left ordered in — one leaked per download.
        fade(to: 0) {
            self.window?.orderOut(nil)
            Toast.layout()
        }
    }
}
