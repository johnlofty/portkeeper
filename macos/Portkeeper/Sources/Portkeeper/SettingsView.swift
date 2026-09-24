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

    static let helperPlist = "io.github.johnlofty.portkeeper.helper.plist"
    static let logPath = "/tmp/portkeeper.log"

    // An ObservableObject rather than @State: under the Command Line Tools toolchain the
    // SwiftUI macro plugin behind @State is missing, and a plain `swift build` must work.
    @StateObject private var model = LoginItemsModel(helperPlist: SettingsView.helperPlist)

    private var openAtLogin: Bool { model.openAtLogin }
    private var helperStatus: SMAppService.Status { model.helperStatus }
    private var errorText: String? { model.errorText }

    var body: some View {
        Form {
            Section("General") {
                Toggle("Open at login", isOn: Binding(
                    get: { openAtLogin },
                    set: { model.setOpenAtLogin($0) }))
            }

            Section("Background helper") {
                LabeledContent("Status", value: describe(helperStatus))
                Text("The helper is the bundled portkeeperd, run by launchd from this app. Quitting the app does not stop it, which is what keeps tunnels up.")
                    .font(.callout)
                    .foregroundStyle(.secondary)
                Text("Only one daemon can own 127.0.0.1:9996. If the development agent from `make install` is loaded, run `make uninstall` first, or the helper will exit on start.")
                    .font(.callout)
                    .foregroundStyle(.secondary)
                if helperStatus == .requiresApproval {
                    Text("macOS needs a one-time approval in Login Items before the helper will run. Until it is approved, the helper is registered but silently never starts.")
                        .font(.callout)
                        .foregroundStyle(Palette.amberText)
                    Button("Open Login Items settings") { SMAppService.openSystemSettingsLoginItems() }
                }
                HStack {
                    Button("Register") { model.act { try model.helper.register() } }
                        .disabled(helperStatus == .enabled)
                    Button("Unregister") { model.act { try model.helper.unregister() } }
                        .disabled(helperStatus == .notRegistered || helperStatus == .notFound)
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

            if let errorText {
                Text(errorText).font(.callout).foregroundStyle(.red)
            }
        }
        .formStyle(.grouped)
        .frame(width: 480)
        .onAppear { model.refresh() }
        .onReceive(NotificationCenter.default.publisher(for: NSApplication.didBecomeActiveNotification)) { _ in
            model.refresh()
        }
    }

    private var daemonState: String {
        if let err = client.lastError { return err }
        guard let s = client.status else { return "checking" }
        return "running, \(s.forwards) mapping\(s.forwards == 1 ? "" : "s"), API \(s.apiVersion.map(String.init) ?? "unversioned")"
    }

    private func describe(_ s: SMAppService.Status) -> String {
        switch s {
        case .notRegistered: return "not registered"
        case .enabled: return "registered and enabled"
        case .requiresApproval: return "waiting for approval in Login Items"
        case .notFound: return "not found in this bundle"
        @unknown default: return "unknown"
        }
    }
}

/// Both SMAppService registrations and their last error.
@MainActor
final class LoginItemsModel: ObservableObject {
    @Published private(set) var openAtLogin = false
    @Published private(set) var helperStatus: SMAppService.Status = .notRegistered
    @Published private(set) var errorText: String?

    let helper: SMAppService

    init(helperPlist: String) {
        helper = SMAppService.agent(plistName: helperPlist)
    }

    func refresh() {
        openAtLogin = SMAppService.mainApp.status == .enabled
        helperStatus = helper.status
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
