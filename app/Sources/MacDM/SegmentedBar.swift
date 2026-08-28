import AppKit

/// The IDM-style progress bar: the file is a horizontal track, each connection
/// draws a filled block over its byte range showing how much of that range is
/// done. Colour keys the connection's state.
final class SegmentedBar: NSView {
    var total: Int64 = 0
    var conns: [ConnStat] = []
    var fallbackPercent: Double = 0  // used when there are no per-conn stats

    override var isFlipped: Bool { true }

    func set(total: Int64, conns: [ConnStat], percent: Double) {
        self.total = total
        self.conns = conns
        self.fallbackPercent = percent
        needsDisplay = true
    }

    override func draw(_ dirty: NSRect) {
        let r = bounds
        NSColor(white: 0.5, alpha: 0.25).setFill()
        NSBezierPath(roundedRect: r, xRadius: 3, yRadius: 3).fill()

        // Use the per-connection rendering only when the connections are a real
        // byte-range split of the whole file (they cover ~all of it). yt-dlp /
        // segment jobs report a single summary row that doesn't — fall back to a
        // plain fill for those.
        let covered = conns.reduce(Int64(0)) { $0 + max(0, $1.total) }
        let realSplit = total > 0 && !conns.isEmpty && covered >= total * 95 / 100

        guard realSplit else {
            drawFraction(0, fallbackPercent / 100, color: .controlAccentColor, in: r)
            return
        }

        for c in conns {
            let x0 = Double(c.start) / Double(total)
            let span = (c.total > 0 ? Double(c.total) : 0) / Double(total)
            let filled = c.total > 0 ? span * Double(c.downloaded) / Double(c.total) : 0
            let color: NSColor
            switch c.status {
            case "done": color = .systemGreen
            case "connecting": color = .systemOrange
            case "error": color = .systemRed
            case "idle": color = NSColor(white: 0.5, alpha: 0.4)
            default: color = .controlAccentColor
            }
            // faint block for the whole assigned range …
            drawFraction(x0, x0 + span, color: color.withAlphaComponent(0.18), in: r)
            // … solid for the downloaded part
            drawFraction(x0, x0 + filled, color: color, in: r)
        }
    }

    private func drawFraction(_ from: Double, _ to: Double, color: NSColor, in r: NSRect) {
        let a = max(0, min(1, from)), b = max(0, min(1, to))
        guard b > a else { return }
        color.setFill()
        NSBezierPath(rect: NSRect(x: r.minX + a * r.width, y: r.minY,
                                  width: (b - a) * r.width, height: r.height)).fill()
    }
}
