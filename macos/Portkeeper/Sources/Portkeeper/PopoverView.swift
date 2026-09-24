import AppKit
import PortkeeperKit
import SwiftUI

/// The popover, after Popover.dc.html: header, one section per host, footer.
struct PopoverView: View {
    @ObservedObject var client: DaemonClient
    @ObservedObject var route: ConsoleRoute
    @Environment(\.openWindow) private var openWindow
    @Environment(\.openSettings) private var openSettings

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            header
            if let err = client.lastError {
                Text(err)
                    .font(.system(size: 12))
                    .foregroundStyle(Palette.amberText)
                    .padding(.horizontal, 12)
                    .padding(.vertical, 8)
            }
            let sections = client.sections
            if sections.isEmpty && client.status != nil {
                Text("No hosts yet. Add a mapping to get started.")
                    .font(.system(size: 12))
                    .foregroundStyle(.secondary)
                    .padding(.horizontal, 12)
                    .padding(.vertical, 8)
            }
            ForEach(Array(sections.enumerated()), id: \.element.id) { index, section in
                if index > 0 { divider }
                HostSectionView(section: section)
            }
            divider
            footer
        }
        .padding(6)
        .frame(width: 360)
    }

    private var divider: some View {
        Rectangle()
            .fill(Color.primary.opacity(0.10))
            .frame(height: 1)
            .padding(.horizontal, 12)
            .padding(.vertical, 4)
    }

    private var header: some View {
        HStack(spacing: 8) {
            Image(systemName: Palette.symbol).font(.system(size: 13))
            Text("Portkeeper").font(.system(size: 13, weight: .semibold))
            Spacer()
            Button {
                NSApp.activate(ignoringOtherApps: true)
                openSettings()
            } label: {
                Image(systemName: "gearshape")
                    .font(.system(size: 13))
                    .foregroundStyle(.secondary)
                    .frame(width: 24, height: 24)
                    .contentShape(Rectangle())
            }
            .buttonStyle(.plain)
            .help("Settings")
            .accessibilityLabel("Settings")
        }
        .padding(.horizontal, 12)
        .padding(.top, 8)
        .padding(.bottom, 6)
    }

    private var footer: some View {
        HStack(spacing: 8) {
            Button {
                showConsole(DaemonClient.addURL)
            } label: {
                Label("Add mapping", systemImage: "plus")
                    .font(.system(size: 13, weight: .semibold))
                    .foregroundStyle(.white)
                    .padding(.horizontal, 14)
                    .padding(.vertical, 7)
                    .background(Palette.accent, in: RoundedRectangle(cornerRadius: 6))
            }
            .buttonStyle(.plain)

            Button("Open console") { showConsole(DaemonClient.consoleURL) }
                .buttonStyle(.plain)
                .font(.system(size: 13, weight: .medium))
                .foregroundStyle(Palette.accent)
                .padding(.horizontal, 10)
                .padding(.vertical, 7)

            Spacer()

            Button("Quit") { NSApp.terminate(nil) }
                .buttonStyle(.plain)
                .font(.system(size: 13, weight: .medium))
                .foregroundStyle(.secondary)
                .padding(.horizontal, 10)
                .padding(.vertical, 7)
        }
        .padding(.horizontal, 8)
        .padding(.top, 8)
        .padding(.bottom, 6)
    }

    /// Adding a mapping is the console's add sheet, not a native form: the console already
    /// validates ranges, target hosts and pins, and a second form would drift from it.
    private func showConsole(_ url: URL) {
        route.show(url)
        // An LSUIElement app is never active on its own; without this the window opens
        // behind whatever the operator was using.
        NSApp.activate(ignoringOtherApps: true)
        openWindow(id: "console")
    }
}

struct HostSectionView: View {
    let section: HostSection

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            HStack(spacing: 8) {
                Circle()
                    .fill(section.healthy ? Palette.accent : Palette.amberDot)
                    .frame(width: 8, height: 8)
                Text(section.alias)
                    .font(.system(size: 13, weight: .semibold, design: .monospaced))
                Text(section.healthy ? "connected" : "reconnecting")
                    .font(.system(size: 12))
                    .foregroundStyle(section.healthy ? AnyShapeStyle(.secondary) : AnyShapeStyle(Palette.amberText))
                Spacer(minLength: 0)
                if let h = section.health, !h.healthy {
                    Text(retryText(h))
                        .font(.system(size: 11))
                        .monospacedDigit()
                        .foregroundStyle(.secondary)
                        .help(h.lastError)
                }
            }
            .padding(.horizontal, 12)
            .padding(.top, 6)
            .padding(.bottom, section.forwards.isEmpty ? 6 : 0)

            ForEach(section.forwards) { f in
                MappingRow(forward: f)
            }
        }
    }

    private func retryText(_ h: HostHealth) -> String {
        h.nextRetryIn > 0
            ? "attempt \(h.attempts), next try in \(h.nextRetryIn)s"
            : "attempt \(h.attempts)"
    }
}

/// One mapping: the remote chip, the cable, the Mac chip, the label.
struct MappingRow: View {
    let forward: Forward

    var body: some View {
        HStack(spacing: 8) {
            PortChip(port: forward.remotePort, caption: remoteCaption, alignment: .leading)
            // The arrowhead points at the side where the port APPEARS: a local-forward
            // makes a remote port show up on the Mac, a remote-forward the reverse.
            Cable(pointsRight: forward.isLocalForward)
                .stroke(.secondary, style: StrokeStyle(lineWidth: 1.5, lineCap: .round, lineJoin: .round))
                .overlay(CableDot(atLeft: forward.isLocalForward).fill(.secondary))
                .frame(width: 56, height: 24)
            PortChip(port: forward.localPort, caption: "Mac", alignment: .trailing)
            HStack(spacing: 6) {
                Text(forward.label.isEmpty ? " " : forward.label)
                    .font(.system(size: 12, weight: .medium))
                    .lineLimit(1)
                    .truncationMode(.tail)
                if forward.pinned {
                    Image(systemName: "pin")
                        .font(.system(size: 10))
                        .foregroundStyle(.secondary)
                        .help("Kept across restarts")
                }
            }
            .padding(.leading, 4)
            .frame(maxWidth: .infinity, alignment: .leading)
            .opacity(forward.isAlive ? 1 : 0.6)

            if forward.isLocalForward, let url = URL(string: forward.url), !forward.url.isEmpty {
                Button {
                    NSWorkspace.shared.open(url)
                } label: {
                    Image(systemName: "arrow.up.forward.square")
                        .font(.system(size: 12))
                        .foregroundStyle(.secondary)
                }
                .buttonStyle(.plain)
                .help("Open \(forward.url)")
                .accessibilityLabel("Open :\(forward.localPort)")
            } else {
                Color.clear.frame(width: 13, height: 1)
            }
        }
        .padding(.horizontal, 12)
        .padding(.vertical, 8)
        .help(help)
    }

    /// "code", or "code → db" when the mapping targets a machine past the remote.
    private var remoteCaption: String {
        forward.remoteHost.isEmpty ? forward.host : "\(forward.host) → \(forward.remoteHost)"
    }

    private var help: String {
        var s = "\(forward.id), \(forward.state)"
        if forward.expiresIn > 0 { s += ", expires in \(forward.expiresIn)s" }
        return s
    }
}

struct PortChip: View {
    let port: Int
    let caption: String
    let alignment: HorizontalAlignment

    var body: some View {
        VStack(alignment: alignment, spacing: 3) {
            Text(verbatim: ":\(port)")
                .font(.system(size: 14, weight: .semibold, design: .monospaced))
                .monospacedDigit()
                .lineLimit(1)
                .minimumScaleFactor(0.8)
            Text(caption)
                .font(.system(size: 10))
                .foregroundStyle(.secondary)
                .lineLimit(1)
                .truncationMode(.middle)
        }
        .frame(width: 72, alignment: alignment == .leading ? .leading : .trailing)
        .padding(.horizontal, 8)
        .padding(.vertical, 6)
        .background(Color(nsColor: .controlBackgroundColor), in: RoundedRectangle(cornerRadius: 6))
        .overlay(RoundedRectangle(cornerRadius: 6).strokeBorder(Color(nsColor: .separatorColor)))
    }
}

/// The line and arrowhead of the mock's 56×24 cable, mirrored for a remote-forward.
struct Cable: Shape {
    let pointsRight: Bool

    func path(in rect: CGRect) -> Path {
        let sx = rect.width / 56, sy = rect.height / 24
        func p(_ x: CGFloat, _ y: CGFloat) -> CGPoint {
            CGPoint(x: rect.minX + (pointsRight ? x : 56 - x) * sx, y: rect.minY + y * sy)
        }
        var path = Path()
        path.move(to: p(7, 12))
        path.addLine(to: p(42, 12))
        path.move(to: p(36, 6))
        path.addLine(to: p(43, 12))
        path.addLine(to: p(36, 18))
        return path
    }
}

/// The dot at the cable's origin: where the traffic is accepted.
struct CableDot: Shape {
    let atLeft: Bool

    func path(in rect: CGRect) -> Path {
        let sx = rect.width / 56, sy = rect.height / 24
        let cx = rect.minX + (atLeft ? 4 : 52) * sx
        let cy = rect.minY + 12 * sy
        return Path(ellipseIn: CGRect(x: cx - 3, y: cy - 3, width: 6, height: 6))
    }
}
