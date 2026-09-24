import Foundation

// These mirror the Go structs field for field, in the daemon's own snake_case, so a
// rename on either side shows up as a decode failure in the test rather than as a
// silently blank row.

/// One mapping, as `GET /admin/forwards` returns it (Go: forwardView).
public struct Forward: Codable, Identifiable, Equatable, Sendable {
    public var id: String
    public var host: String
    /// "local-forward" or "remote-forward".
    public var direction: String
    /// Empty for the remote's own localhost.
    public var remoteHost: String
    public var remotePort: Int
    public var localPort: Int
    public var label: String
    public var url: String
    public var createdAt: String
    /// Seconds. 0 means no expiry.
    public var ttl: Int
    public var expiresIn: Int
    public var age: Int
    /// "alive", "dead" or "reconnecting".
    public var state: String
    public var requester: String
    public var autoOpened: Bool
    public var pinned: Bool

    enum CodingKeys: String, CodingKey {
        case id, host, direction, label, url, ttl, age, state, requester, pinned
        case remoteHost = "remote_host"
        case remotePort = "remote_port"
        case localPort = "local_port"
        case createdAt = "created_at"
        case expiresIn = "expires_in"
        case autoOpened = "auto_opened"
    }

    public var isLocalForward: Bool { direction == "local-forward" }
    public var isAlive: Bool { state == "alive" }
}

/// One host's connection state, the values of `hosts` in `/admin/status` (Go: hostHealthView).
public struct HostHealth: Codable, Equatable, Sendable {
    public var healthy: Bool
    public var attempts: Int
    /// Seconds until the next attempt; 0 when none is scheduled.
    public var nextRetryIn: Int
    public var lastError: String

    enum CodingKeys: String, CodingKey {
        case healthy, attempts
        case nextRetryIn = "next_retry_in"
        case lastError = "last_error"
    }
}

/// `GET /admin/status`.
///
/// The collections are optional because Go encodes a nil map or slice as `null`, and an
/// idle daemon must not read as a broken one. api_version is optional because a daemon
/// older than the field is still a daemon this app can show.
public struct Status: Codable, Equatable, Sendable {
    public var apiVersion: Int?
    public var listen: String
    public var hosts: [String: HostHealth]?
    public var eagerHosts: [String]?
    public var maxForwards: Int
    public var defaultTTL: Int
    public var forwards: Int
    public var pinned: Int?

    enum CodingKeys: String, CodingKey {
        case listen, hosts, forwards, pinned
        case apiVersion = "api_version"
        case eagerHosts = "eager_hosts"
        case maxForwards = "max_forwards"
        case defaultTTL = "default_ttl"
    }
}

/// What the popover draws: one section per host, its health, and its mappings.
public struct HostSection: Identifiable, Equatable, Sendable {
    public var alias: String
    /// nil when a mapping names a host the status document does not (yet) know.
    public var health: HostHealth?
    public var forwards: [Forward]
    public var id: String { alias }
    public var healthy: Bool { health?.healthy ?? true }

    /// Groups mappings under their hosts. Every host in status gets a section, even with
    /// no mappings, because "the link to gcp-ubuntu is down" is worth seeing on its own.
    public static func build(forwards: [Forward], status: Status?) -> [HostSection] {
        let health = status?.hosts ?? [:]
        var aliases = Set(health.keys)
        for f in forwards { aliases.insert(f.host) }
        return aliases.sorted().map { alias in
            HostSection(alias: alias, health: health[alias],
                        forwards: forwards.filter { $0.host == alias })
        }
    }
}
