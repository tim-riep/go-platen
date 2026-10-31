// Draws the app icon into an .iconset directory: swift make-icon.swift <dir>
import AppKit

let out = URL(fileURLWithPath: CommandLine.arguments[1])
try FileManager.default.createDirectory(at: out, withIntermediateDirectories: true)

func draw(_ size: CGFloat) -> Data {
    let rep = NSBitmapImageRep(bitmapDataPlanes: nil, pixelsWide: Int(size), pixelsHigh: Int(size), bitsPerSample: 8,
                               samplesPerPixel: 4, hasAlpha: true, isPlanar: false, colorSpaceName: .deviceRGB,
                               bytesPerRow: 0, bitsPerPixel: 0)!
    NSGraphicsContext.saveGraphicsState()
    NSGraphicsContext.current = NSGraphicsContext(bitmapImageRep: rep)
    let s = size / 1024

    // Background: rounded square with the usual macOS icon margin.
    let bg = NSBezierPath(roundedRect: NSRect(x: 100 * s, y: 100 * s, width: 824 * s, height: 824 * s),
                          xRadius: 185 * s, yRadius: 185 * s)
    NSGradient(starting: NSColor(red: 0.20, green: 0.45, blue: 0.95, alpha: 1),
               ending: NSColor(red: 0.08, green: 0.22, blue: 0.62, alpha: 1))!.draw(in: bg, angle: -90)

    // Paper sheet coming out of the platen.
    let sheet = NSBezierPath(roundedRect: NSRect(x: 300 * s, y: 250 * s, width: 424 * s, height: 470 * s),
                             xRadius: 24 * s, yRadius: 24 * s)
    NSColor.white.setFill()
    sheet.fill()
    NSColor(white: 0.78, alpha: 1).setFill()
    for (i, w) in [300.0, 340.0, 250.0, 320.0].enumerated() {
        NSBezierPath(roundedRect: NSRect(x: 352 * s, y: (560 - Double(i) * 70) * s, width: w * s, height: 28 * s),
                     xRadius: 14 * s, yRadius: 14 * s).fill()
    }

    // The platen: a roller across the top of the sheet.
    let roller = NSBezierPath(roundedRect: NSRect(x: 220 * s, y: 690 * s, width: 584 * s, height: 110 * s),
                              xRadius: 55 * s, yRadius: 55 * s)
    NSGradient(starting: NSColor(white: 0.22, alpha: 1), ending: NSColor(white: 0.08, alpha: 1))!.draw(in: roller, angle: -90)
    NSColor(white: 1, alpha: 0.18).setFill()
    NSBezierPath(roundedRect: NSRect(x: 260 * s, y: 760 * s, width: 504 * s, height: 18 * s),
                 xRadius: 9 * s, yRadius: 9 * s).fill()

    NSGraphicsContext.restoreGraphicsState()
    return rep.representation(using: .png, properties: [:])!
}

for base in [16, 32, 128, 256, 512] {
    for scale in [1, 2] {
        let name = scale == 1 ? "icon_\(base)x\(base).png" : "icon_\(base)x\(base)@2x.png"
        try draw(CGFloat(base * scale)).write(to: out.appendingPathComponent(name))
    }
}
