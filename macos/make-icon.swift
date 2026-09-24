// Draws the app mark into an .iconset: the popover header's glyph (a rounded rectangle
// with a small right-pointing arrow inside) in white on the teal accent.
//
//   swift make-icon.swift <out.iconset>
//
// build-app.sh runs this and hands the result to iconutil.
import AppKit

let out = URL(fileURLWithPath: CommandLine.arguments.count > 1 ? CommandLine.arguments[1] : "AppIcon.iconset")
try? FileManager.default.createDirectory(at: out, withIntermediateDirectories: true)

func draw(px: Int) -> Data {
    let rep = NSBitmapImageRep(bitmapDataPlanes: nil, pixelsWide: px, pixelsHigh: px,
                               bitsPerSample: 8, samplesPerPixel: 4, hasAlpha: true, isPlanar: false,
                               colorSpaceName: .deviceRGB, bytesPerRow: 0, bitsPerPixel: 0)!
    NSGraphicsContext.saveGraphicsState()
    NSGraphicsContext.current = NSGraphicsContext(bitmapImageRep: rep)
    let s = CGFloat(px) / 1024

    // The macOS icon grid: an 824pt squircle-ish body centred in 1024.
    let body = NSRect(x: 100 * s, y: 100 * s, width: 824 * s, height: 824 * s)
    let bg = NSBezierPath(roundedRect: body, xRadius: 185 * s, yRadius: 185 * s)
    NSGradient(starting: NSColor(srgbRed: 0x14 / 255, green: 0x8A / 255, blue: 0x86 / 255, alpha: 1),
               ending: NSColor(srgbRed: 0x0B / 255, green: 0x6E / 255, blue: 0x6B / 255, alpha: 1))!
        .draw(in: bg, angle: -90)

    // The 24-unit glyph from the mock, scaled so it spans ~62% of the body.
    let u = 24 * s, ox = 512 * s - 12 * u, oy = 512 * s - 12 * u
    func p(_ x: CGFloat, _ y: CGFloat) -> NSPoint { NSPoint(x: ox + x * u, y: oy + (24 - y) * u) }
    NSColor.white.setStroke()
    let frame = NSBezierPath(roundedRect: NSRect(x: ox + 3 * u, y: oy + 5 * u, width: 18 * u, height: 14 * u),
                             xRadius: 3 * u, yRadius: 3 * u)
    let glyph = NSBezierPath()
    glyph.move(to: p(8, 12)); glyph.line(to: p(15, 12))
    glyph.move(to: p(12, 9)); glyph.line(to: p(15, 12)); glyph.line(to: p(12, 15))
    for path in [frame, glyph] {
        path.lineWidth = 1.9 * u
        path.lineCapStyle = .round
        path.lineJoinStyle = .round
        path.stroke()
    }
    NSGraphicsContext.restoreGraphicsState()
    return rep.representation(using: .png, properties: [:])!
}

for base in [16, 32, 128, 256, 512] {
    for scale in [1, 2] {
        let name = scale == 1 ? "icon_\(base)x\(base).png" : "icon_\(base)x\(base)@2x.png"
        try draw(px: base * scale).write(to: out.appendingPathComponent(name))
    }
}
