import Foundation
import PortkeeperKit
import UserNotifications

/// Posts a notification when a host's connection comes back, and when a mapping goes
/// away (closed, expired, or dropped). Everything else is visible in the popover.
@MainActor
final class Notifier {
    private var attached = false

    func attach(to client: DaemonClient) {
        guard !attached else { return }
        attached = true
        // UNUserNotificationCenter traps in a binary with no bundle, which is what
        // `swift run` produces. Only the assembled .app gets notifications.
        guard Bundle.main.bundleIdentifier != nil, Bundle.main.bundleURL.pathExtension == "app" else { return }
        UNUserNotificationCenter.current().requestAuthorization(options: [.alert]) { _, _ in }
        client.onChange = { [weak self] old, new in self?.diff(old: old, new: new) }
    }

    private func diff(old: [HostSection], new: [HostSection]) {
        let oldByAlias = Dictionary(uniqueKeysWithValues: old.map { ($0.alias, $0) })
        for s in new {
            if let was = oldByAlias[s.alias], !was.healthy, s.healthy {
                post("\(s.alias) reconnected", body: "The connection to \(s.alias) is back up.")
            }
        }
        let now = Set(new.flatMap { $0.forwards.map(\.id) })
        for f in old.flatMap(\.forwards) where !now.contains(f.id) {
            let name = f.label.isEmpty ? f.id : f.label
            post("Mapping gone: \(name)", body: "\(f.host):\(f.remotePort) and Mac:\(f.localPort) are no longer mapped.")
        }
    }

    private func post(_ title: String, body: String) {
        let c = UNMutableNotificationContent()
        c.title = title
        c.body = body
        UNUserNotificationCenter.current().add(
            UNNotificationRequest(identifier: UUID().uuidString, content: c, trigger: nil))
    }
}
