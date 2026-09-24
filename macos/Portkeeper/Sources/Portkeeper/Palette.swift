import AppKit
import SwiftUI

/// The mock's palette: system colors for everything, plus one accent and one warning.
enum Palette {
    /// The menu-bar symbol. The mock draws a rounded rectangle with an arrow inside; this
    /// is the closest SF Symbol that reads as "a port with a line out of it".
    static let symbol = "rectangle.connected.to.line.below"

    /// Teal is the only accent: healthy dots and the primary button.
    static let accent = dynamic(light: 0x0B6E6B, dark: 0x3FBDB3)
    /// Amber marks a host that is reconnecting.
    static let amberDot = dynamic(light: 0xE39A1D, dark: 0xE0A33A)
    static let amberText = dynamic(light: 0x8A5700, dark: 0xE0A33A)

    private static func dynamic(light: UInt32, dark: UInt32) -> Color {
        Color(nsColor: NSColor(name: nil) { appearance in
            let isDark = appearance.bestMatch(from: [.darkAqua, .aqua]) == .darkAqua
            return rgb(isDark ? dark : light)
        })
    }

    private static func rgb(_ hex: UInt32) -> NSColor {
        NSColor(srgbRed: CGFloat((hex >> 16) & 0xFF) / 255,
                green: CGFloat((hex >> 8) & 0xFF) / 255,
                blue: CGFloat(hex & 0xFF) / 255, alpha: 1)
    }
}
