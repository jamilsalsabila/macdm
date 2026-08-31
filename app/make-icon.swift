// make-icon.swift — generates AppIcon.iconset (all sizes) + AppIcon.icns.
// Run from app/:  swift make-icon.swift
// Draws a macOS-grid rounded-rect icon with a download glyph (arrow into tray).
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

    // --- rounded-rect body on the macOS icon grid ---
    // The body is ~824/1024 of the canvas, centred, rest transparent. Filling
    // edge-to-edge makes the Dock icon look oversized next to every other app.
    let margin = s * 0.0977
    let rect = CGRect(x: margin, y: margin, width: s - 2*margin, height: s - 2*margin)
    let radius = rect.width * 0.225
    let body = CGPath(roundedRect: rect, cornerWidth: radius, cornerHeight: radius, transform: nil)

    ctx.saveGState()
    ctx.addPath(body); ctx.clip()
    let cs = CGColorSpaceCreateDeviceRGB()
    let grad = CGGradient(colorsSpace: cs, colors: [
        CGColor(red: 0.29, green: 0.44, blue: 0.96, alpha: 1),   // top  #4A70F5
        CGColor(red: 0.42, green: 0.24, blue: 0.86, alpha: 1),   // bot  #6B3DDB
    ] as CFArray, locations: [0, 1])!
    ctx.drawLinearGradient(grad, start: CGPoint(x: rect.minX, y: rect.maxY),
                           end: CGPoint(x: rect.maxX, y: rect.minY), options: [])
    let sheen = CGGradient(colorsSpace: cs, colors: [
        CGColor(red: 1, green: 1, blue: 1, alpha: 0.16),
        CGColor(red: 1, green: 1, blue: 1, alpha: 0.0),
    ] as CFArray, locations: [0, 1])!
    ctx.drawLinearGradient(sheen, start: CGPoint(x: rect.minX, y: rect.maxY),
                           end: CGPoint(x: rect.minX, y: rect.minY), options: [])
    ctx.restoreGState()

    // --- download glyph: arrow (shaft + head) pointing into a tray line ---
    // Measured in fractions of the body so the inset holds as the body shrinks.
    let u = rect.width
    let cx = rect.midX
    ctx.setFillColor(CGColor(red: 1, green: 1, blue: 1, alpha: 1))

    let trayW = u * 0.44
    let trayH = max(u * 0.055, 2)
    let trayY = rect.minY + u * 0.24
    let tray = CGPath(roundedRect: CGRect(x: cx - trayW/2, y: trayY, width: trayW, height: trayH),
        cornerWidth: trayH/2, cornerHeight: trayH/2, transform: nil)
    ctx.addPath(tray); ctx.fillPath()

    let tipY = trayY + trayH + u * 0.055     // arrow tip, just above the tray
    let headHalf = u * 0.155
    let headBaseY = tipY + u * 0.16
    let shaftW = u * 0.12
    let shaftTopY = rect.minY + u * 0.74

    let shaft = CGPath(roundedRect: CGRect(x: cx - shaftW/2, y: headBaseY - u*0.02,
        width: shaftW, height: shaftTopY - (headBaseY - u*0.02)),
        cornerWidth: shaftW*0.4, cornerHeight: shaftW*0.4, transform: nil)
    ctx.addPath(shaft); ctx.fillPath()

    ctx.beginPath()
    ctx.move(to: CGPoint(x: cx - headHalf, y: headBaseY))
    ctx.addLine(to: CGPoint(x: cx + headHalf, y: headBaseY))
    ctx.addLine(to: CGPoint(x: cx, y: tipY))
    ctx.closePath()
    ctx.fillPath()

    NSGraphicsContext.restoreGraphicsState()
    return rep.representation(using: .png, properties: [:])!
}

let fm = FileManager.default
let iconset = "AppIcon.iconset"
try? fm.removeItem(atPath: iconset)
try! fm.createDirectory(atPath: iconset, withIntermediateDirectories: true)
for (name, px) in sizes {
    try! draw(px).write(to: URL(fileURLWithPath: "\(iconset)/\(name).png"))
}

let p = Process()
p.executableURL = URL(fileURLWithPath: "/usr/bin/iconutil")
p.arguments = ["-c", "icns", iconset, "-o", "AppIcon.icns"]
try! p.run(); p.waitUntilExit()
try? fm.removeItem(atPath: iconset)
print(p.terminationStatus == 0 ? "wrote AppIcon.icns" : "iconutil failed \(p.terminationStatus)")
exit(p.terminationStatus)
