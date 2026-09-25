import AppKit
import PortkeeperKit
import SwiftUI

/// Where the console window should point, shared between the popover (which asks for
/// "/" or "/#add") and the window (which loads it). `token` changes on every request so
/// asking for the same URL twice still does something.
@MainActor
final class ConsoleRoute: ObservableObject {
    @Published private(set) var url = DaemonClient.consoleURL
    @Published private(set) var token = 0

    func show(_ url: URL) {
        self.url = url
        token += 1
    }
}

@main
struct PortkeeperApp: App {
    @StateObject private var client = DaemonClient()
    @StateObject private var route = ConsoleRoute()
    private let notifier = Notifier()
    @StateObject private var agent = DaemonAgent()

    var body: some Scene {
        // MenuBarExtra is declared first on purpose: SwiftUI opens the first window-like
        // scene at launch, and this app must launch to nothing but its menu-bar item.
        MenuBarExtra {
            PopoverView(client: client, route: route)
                .onAppear { start() }
        } label: {
            MenuBarLabel(client: client)
                .task { start() }
                .onChange(of: client.polls) { _, n in if n == 1 { checkDaemon() } }
        }
        .menuBarExtraStyle(.window)

        Window("Portkeeper Console", id: "console") {
            ConsoleView(route: route)
        }
        .defaultSize(width: 1280, height: 880)

        Settings {
            SettingsView(client: client, agent: agent)
        }
    }

    /// Idempotent: the label's task and the popover's onAppear both call it, and
    /// whichever runs first starts the one poll loop.
    private func start() {
        notifier.attach(to: client)
        client.start()
    }

    /// Runs once, after the first poll; see DaemonAgent.reconcile.
    private func checkDaemon() {
        agent.reconcile(client.status)
    }
}

/// The menu-bar item: the symbol, then the count of mappings that are alive, or "!" when
/// any host's connection is down or the daemon cannot be reached at all.
struct MenuBarLabel: View {
    @ObservedObject var client: DaemonClient

    var body: some View {
        HStack(spacing: 4) {
            Image(systemName: Palette.symbol)
            Text(text).monospacedDigit()
        }
    }

    private var text: String {
        if client.lastError != nil && client.status == nil { return "!" }
        if client.anyUnhealthy { return "!" }
        return "\(client.aliveCount)"
    }
}
