import AppKit
import PortkeeperKit
import ServiceManagement
import SwiftUI

/// Settings: the login item, the bundled daemon's launchd registration, and where the
/// daemon is.
///
/// Both registrations point at wherever this bundle is when they are made, so they are
/// only meaningful for a copy of the app that is going to stay put.
struct SettingsView: View {
    @ObservedObject var client: DaemonClient
    @ObservedObject var agent: DaemonAgent

    static let logPath = "/tmp/portkeeper.log"

    // An ObservableObject rather than @State: under the Command Line Tools toolchain the
    // SwiftUI macro plugin behind @State is missing, and a plain `swift build` must work.
    @StateObject private var model = LoginItemsModel()

    private var openAtLogin: Bool { model.openAtLogin }
    private var errorText: String? { model.errorText }

    var body: some View {
        Form {
            Section("General") {
                Toggle("Open at login", isOn: Binding(
                    get: { openAtLogin },
                    set: { model.setOpenAtLogin($0) }))
            }

            Section("Background daemon") {
                LabeledContent("Status", value: agentState)
                Text("The daemon is the bundled portkeeperd, run by launchd as a login agent. Quitting the app does not stop it, which is what keeps tunnels up. Replacing the app restarts it on the new version the next time the app opens.")
                    .font(.callout)
                    .foregroundStyle(.secondary)
                if case .development = agent.state {
                    Text("A development daemon from `make install` owns the job. Install here replaces it with this app's daemon.")
                        .font(.callout)
                        .foregroundStyle(Palette.amberText)
                }
                HStack {
                    Button(agent.state == .thisApp ? "Restart" : "Install") { agent.install() }
                    Button("Uninstall") { agent.uninstall() }
                        .disabled(agent.state == .notInstalled)
                }
            }

            Section("Daemon") {
                LabeledContent("Address") {
                    Text(verbatim: DaemonClient.base.absoluteString).textSelection(.enabled)
                }
                LabeledContent("State", value: daemonState)
                Button("Open log") {
                    NSWorkspace.shared.open(URL(fileURLWithPath: Self.logPath))
                }
            }

            if let errorText = errorText ?? agent.errorText {
                Text(errorText).font(.callout).foregroundStyle(.red)
            }
        }
        .formStyle(.grouped)
        .frame(width: 480)
        .onAppear { model.refresh(); agent.refresh() }
        .onReceive(NotificationCenter.default.publisher(for: NSApplication.didBecomeActiveNotification)) { _ in
            model.refresh()
            agent.refresh()
        }
    }

    private var daemonState: String {
        if let err = client.lastError { return err }
        guard let s = client.status else { return "checking" }
        return "running, \(s.forwards) mapping\(s.forwards == 1 ? "" : "s"), API \(s.apiVersion.map(String.init) ?? "unversioned")"
    }

    private var agentState: String {
        switch agent.state {
        case .notInstalled: return "not installed"
        case .thisApp: return "installed, runs this app's daemon"
        case .otherApp(let path): return "runs another copy: \(path)"
        case .development(let path): return "development daemon: \(path)"
        }
    }
}

/// The app's own login item, and its last error.
@MainActor
final class LoginItemsModel: ObservableObject {
    @Published private(set) var openAtLogin = false
    @Published private(set) var errorText: String?

    func refresh() {
        openAtLogin = SMAppService.mainApp.status == .enabled
    }

    func setOpenAtLogin(_ on: Bool) {
        act { on ? try SMAppService.mainApp.register() : try SMAppService.mainApp.unregister() }
    }

    func act(_ body: () throws -> Void) {
        do {
            try body()
            errorText = nil
        } catch {
            errorText = error.localizedDescription
        }
        refresh()
    }
}
