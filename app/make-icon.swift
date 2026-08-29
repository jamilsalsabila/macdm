// make-icon.swift — generates AppIcon.iconset (all sizes) + AppIcon.icns.
// Run from app/:  swift make-icon.swift
// Draws a macOS-style rounded-rect icon with a download glyph (arrow into tray).
import AppKit

let sizes: [(name: String, px: Int)] = [
    ("icon_16x16",      16),
    ("icon_16x16@2x",   32),
    ("icon_32x32",      32),
    ("icon_32x32@2x",   64),
    ("icon_128x128",   128),
    ("icon_128x128@2x",256),
    ("icon_256x256",   256),
    ("icon_256x256@2x",512),
    ("icon_512x512",   512),
    ("icon_512x512@2x",1024),
]

func draw(_ px: Int) -> Data {
    let s = CGFloat(px)
    let rep = NSBitmapImageRep(bitmapDataPlanes: nil, pixelsWide: px, pixelsHigh: px,
        bitsPerSample: 8, samplesPerPixel: 4, hasAlpha: true, isPlanar: false,
        colorSpaceName: .deviceRGB, bytesPerRow: 0, bitsPerPixel: 0)!
    rep.size = NSSize(width: px, height: px)
    NSGraphicsContext.saveGraphicsState()
    NSGraphicsContext.current = NSGraphicsContext(bitmapImageRep: rep)
    let ctx = NSGraphicsContext.current!.cgContext

    // --- rounded-rect (squircle-ish) background with a diagonal gradient ---
    let margin = s * 0.06
    let rect = CGRect(x: margin, y: margin, width: s - 2*margin, height: s - 2*margin)
    let radius = rect.width * 0.2237
    let path = CGPath(roundedRect: rect, cornerWidth: radius, cornerHeight: radius, transform: nil)
    ctx.saveGState()
    ctx.addPath(path); ctx.clip()
    let cs = CGColorSpaceCreateDeviceRGB()
    let grad = CGGradient(colorsSpace: cs, colors: [
        CGColor(red: 0.29, green: 0.44, blue: 0.96, alpha: 1),   // top  #4A70F5
        CGColor(red: 0.42, green: 0.24, blue: 0.86, alpha: 1),   // bot  #6B3DDB
    ] as CFArray, locations: [0, 1])!
    ctx.drawLinearGradient(grad, start: CGPoint(x: rect.minX, y: rect.maxY),
                           end: CGPoint(x: rect.maxX, y: rect.minY), options: [])
    // soft top-down sheen (no hard edge)
    let sheen = CGGradient(colorsSpace: cs, colors: [
        CGColor(red: 1, green: 1, blue: 1, alpha: 0.16),
        CGColor(red: 1, green: 1, blue: 1, alpha: 0.0),
    ] as CFArray, locations: [0, 1])!
    ctx.drawLinearGradient(sheen, start: CGPoint(x: rect.minX, y: rect.maxY),
                           end: CGPoint(x: rect.minX, y: rect.minY), options: [])
    ctx.restoreGState()

    // --- download glyph: arrow shaft + head, over a tray line ---
    let cx = s / 2
    let white = CGColor(red: 1, green: 1, blue: 1, alpha: 1)
    ctx.setFillColor(white)
    ctx.setStrokeColor(white)
    ctx.setLineCap(.round)
    ctx.setLineJoin(.round)

    let shaftW = s * 0.115
    let arrowTop = s * 0.72          // y grows upward in this context
    let arrowMidY = s * 0.44         // where the head starts
    let headHalf = s * 0.185
    let headTipY = s * 0.275

    // shaft (rounded)
    let shaft = CGPath(roundedRect: CGRect(x: cx - shaftW/2, y: arrowMidY,
        width: shaftW, height: arrowTop - arrowMidY),
        cornerWidth: shaftW*0.35, cornerHeight: shaftW*0.35, transform: nil)
    ctx.addPath(shaft); ctx.fillPath()
    // head (triangle pointing down)
    ctx.beginPath()
    ctx.move(to: CGPoint(x: cx - headHalf, y: arrowMidY + s*0.03))
    ctx.addLine(to: CGPoint(x: cx + headHalf, y: arrowMidY + s*0.03))
    ctx.addLine(to: CGPoint(x: cx, y: headTipY))
    ctx.closePath()
    ctx.fillPath()

    // tray / baseline (rounded)
    let trayW = s * 0.42
    let trayH = max(s * 0.05, 2)
    let trayY = s * 0.21
    let tray = CGPath(roundedRect: CGRect(x: cx - trayW/2, y: trayY, width: trayW, height: trayH),
        cornerWidth: trayH/2, cornerHeight: trayH/2, transform: nil)
    ctx.addPath(tray); ctx.fillPath()

    NSGraphicsContext.restoreGraphicsState()
    return rep.representation(using: .png, properties: [:])!
}

let fm = FileManager.default
let iconset = "AppIcon.iconset"
try? fm.removeItem(atPath: iconset)
try! fm.createDirectory(atPath: iconset, withIntermediateDirectories: true)
for (name, px) in sizes {
    let data = draw(px)
    try! data.write(to: URL(fileURLWithPath: "\(iconset)/\(name).png"))
}

let p = Process()
p.executableURL = URL(fileURLWithPath: "/usr/bin/iconutil")
p.arguments = ["-c", "icns", iconset, "-o", "AppIcon.icns"]
try! p.run(); p.waitUntilExit()
try? fm.removeItem(atPath: iconset)
print(p.terminationStatus == 0 ? "wrote AppIcon.icns" : "iconutil failed \(p.terminationStatus)")
exit(p.terminationStatus)
