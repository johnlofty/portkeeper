import Foundation
import PortkeeperKit
import ServiceManagement

/// The daemon's launchd job: a plain LaunchAgent at
/// ~/Library/LaunchAgents/io.github.johnlofty.portkeeper.plist whose program is the
/// portkeeperd inside this bundle.
///
/// It used to be an SMAppService agent. That broke every upgrade: the app is ad-hoc
/// signed, so macOS records a launch constraint on the exact code hash of the helper it
/// first approved, and re-registering reuses that record. A new build then fails to
/// spawn with "Launch Constraint Violation" and the daemon is simply down (seen
/// 2026-09-25, v0.1.3 to v0.1.4). A plain agent carries no such constraint, so the app
/// can rewrite it and restart it on every upgrade.
///
/// The label is the one `make install` uses. There is one daemon, whichever copy it runs
/// from; a job pointing into a repo checkout rather than an app bundle is a developer's,
/// and the app leaves it alone.
@MainActor
final class DaemonAgent: ObservableObject {
    static let label = "io.github.johnlofty.portkeeper"
    static let legacyHelperPlist = "io.github.johnlofty.portkeeper.helper.plist"

    enum State: Equatable {
        case notInstalled
        case thisApp               // runs this bundle's daemon
        case otherApp(String)      // runs another copy of Portkeeper.app
        case development(String)   // runs a binary outside any app bundle
    }

    @Published private(set) var state: State = .notInstalled
    @Published private(set) var errorText: String?

    private var reconciled = false

    let plistURL = FileManager.default.homeDirectoryForCurrentUser
        .appendingPathComponent("Library/LaunchAgents/\(DaemonAgent.label).plist")
    let daemonPath = Bundle.main.bundleURL
        .appendingPathComponent("Contents/MacOS/portkeeperd").path

    /// The bundle's version, or nil for a development build (0.0.0), which never takes
    /// over the job on its own.
    private var releaseVersion: String? {
        let v = Bundle.main.object(forInfoDictionaryKey: "CFBundleShortVersionString") as? String
        return (v == nil || v == "0.0.0") ? nil : v
    }

    init() { refresh() }

    func refresh() {
        guard let program = installedProgram() else {
            state = .notInstalled
            return
        }
        if program == daemonPath {
            state = .thisApp
        } else if program.hasSuffix(".app/Contents/MacOS/portkeeperd") {
            state = .otherApp(program)
        } else {
            state = .development(program)
        }
    }

    /// Writes the job for this bundle and (re)starts it. Safe to repeat.
    func install() {
        do {
            retireLegacyHelper()
            try writePlist()
            // bootout fails when the job is not loaded, which is the first-install case.
            _ = launchctl(["bootout", "gui/\(getuid())/\(Self.label)"])
            waitUnloaded()
            let (ok, out) = launchctl(["bootstrap", "gui/\(getuid())", plistURL.path])
            if !ok { throw AgentError(message: "launchctl bootstrap failed: \(out)") }
            errorText = nil
        } catch {
            errorText = error.localizedDescription
        }
        refresh()
    }

    /// Stops the daemon and removes the job. Every mapping goes with it.
    func uninstall() {
        _ = launchctl(["bootout", "gui/\(getuid())/\(Self.label)"])
        do {
            if FileManager.default.fileExists(atPath: plistURL.path) {
                try FileManager.default.removeItem(at: plistURL)
            }
            errorText = nil
        } catch {
            errorText = error.localizedDescription
        }
        refresh()
    }

    /// Once per launch, after the first poll, whether or not the daemon answered (nil).
    /// A release build takes the job over when it runs another copy of the app, or when
    /// the daemon answering is not this version, or is not answering at all. This is what
    /// makes replacing Portkeeper.app restart the daemon.
    func reconcile(_ status: Status?) {
        guard !reconciled else { return }
        reconciled = true
        refresh()

        guard let mine = releaseVersion else { return }
        let theirs = status?.version.map { $0.hasPrefix("v") ? String($0.dropFirst()) : $0 }
        switch state {
        case .notInstalled:
            // An upgrade from a release that used the SMAppService helper: the user
            // already chose to run the daemon, and that helper cannot start this build,
            // so move them over. Otherwise installing is the user's call, in Settings.
            if legacyHelperRegistered {
                NSLog("Portkeeper: moving the daemon from the old helper to a login agent")
                install()
            }
        case .development:
            // A checkout's job is a developer's.
            return
        case .otherApp:
            NSLog("Portkeeper: the daemon job runs another copy of the app; moving it to %@", daemonPath)
            install()
        case .thisApp:
            if status == nil {
                NSLog("Portkeeper: the daemon job is installed but not answering; restarting it")
                install()
            } else if theirs != mine {
                NSLog("Portkeeper: daemon is %@, app is %@; restarting it", theirs ?? "older than 0.1.4", mine)
                install()
            }
        }
    }

    /// Unregisters the SMAppService helper earlier releases used. Its plist stays in the
    /// bundle for now so this call can still name it.
    private var legacyHelperRegistered: Bool {
        let s = SMAppService.agent(plistName: Self.legacyHelperPlist).status
        return s == .enabled || s == .requiresApproval
    }

    private func retireLegacyHelper() {
        guard legacyHelperRegistered else { return }
        let legacy = SMAppService.agent(plistName: Self.legacyHelperPlist)
        do {
            try legacy.unregister()
            NSLog("Portkeeper: unregistered the old SMAppService helper")
        } catch {
            NSLog("Portkeeper: could not unregister the old helper: %@", error.localizedDescription)
        }
    }

    /// bootout returns before launchd has let go of the job, and a bootstrap in that
    /// window fails with an I/O error. Wait up to five seconds for it to be gone.
    private func waitUnloaded() {
        for _ in 0..<50 {
            if !launchctl(["print", "gui/\(getuid())/\(Self.label)"]).0 { return }
            Thread.sleep(forTimeInterval: 0.1)
        }
    }

    private func installedProgram() -> String? {
        guard let data = try? Data(contentsOf: plistURL),
              let plist = try? PropertyListSerialization.propertyList(from: data, format: nil) as? [String: Any]
        else { return nil }
        if let args = plist["ProgramArguments"] as? [String], let first = args.first { return first }
        return plist["Program"] as? String
    }

    private func writePlist() throws {
        let plist: [String: Any] = [
            "Label": Self.label,
            "ProgramArguments": [daemonPath],
            "RunAtLoad": true,
            // Restart on a crash, not on a clean exit: the daemon exits non-zero when
            // the listen port is taken, and the throttle keeps that to one try in ten
            // seconds.
            "KeepAlive": ["SuccessfulExit": false],
            // The Aqua session is what gives the daemon a pasteboard and open(1).
            "ProcessType": "Interactive",
            "LimitLoadToSessionType": "Aqua",
            "EnvironmentVariables": ["PATH": "/usr/bin:/bin:/usr/sbin:/sbin:/opt/homebrew/bin"],
            "StandardOutPath": SettingsView.logPath,
            "StandardErrorPath": SettingsView.logPath,
        ]
        let data = try PropertyListSerialization.data(fromPropertyList: plist, format: .xml, options: 0)
        try FileManager.default.createDirectory(at: plistURL.deletingLastPathComponent(), withIntermediateDirectories: true)
        try data.write(to: plistURL, options: .atomic)
    }

    private func launchctl(_ args: [String]) -> (Bool, String) {
        let p = Process()
        p.executableURL = URL(fileURLWithPath: "/bin/launchctl")
        p.arguments = args
        let pipe = Pipe()
        p.standardOutput = pipe
        p.standardError = pipe
        do {
            try p.run()
            p.waitUntilExit()
        } catch {
            return (false, error.localizedDescription)
        }
        let out = String(data: pipe.fileHandleForReading.readDataToEndOfFile(), encoding: .utf8) ?? ""
        return (p.terminationStatus == 0, out.trimmingCharacters(in: .whitespacesAndNewlines))
    }
}

struct AgentError: LocalizedError {
    let message: String
    var errorDescription: String? { message }
}
