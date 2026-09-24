import Foundation

/// Polls the daemon. The base is the literal loopback address the daemon listens on: its
/// origin guard refuses any other Host header, so this is not a place to be clever.
///
/// URLSession sends no Fetch Metadata or Origin headers, so these requests read to the
/// daemon exactly like curl from a terminal, which is what they are.
@MainActor
public final class DaemonClient: ObservableObject {
    public static let base = URL(string: "http://127.0.0.1:9996")!
    public static let consoleURL = URL(string: "http://127.0.0.1:9996/")!
    public static let addURL = URL(string: "http://127.0.0.1:9996/#add")!
    /// The newest status document this build knows the meaning of.
    public static let apiVersion = 1

    @Published public private(set) var forwards: [Forward] = []
    @Published public private(set) var status: Status?
    /// Set when the last poll failed; the popover says "daemon not reachable" with it.
    @Published public private(set) var lastError: String?
    /// Called on the main actor after every successful poll with the previous and new
    /// snapshots, which is where notifications are derived from.
    public var onChange: ((_ old: [HostSection], _ new: [HostSection]) -> Void)?

    public var sections: [HostSection] { HostSection.build(forwards: forwards, status: status) }
    public var aliveCount: Int { forwards.filter(\.isAlive).count }
    public var anyUnhealthy: Bool { status?.hosts?.values.contains { !$0.healthy } ?? false }

    private let session: URLSession
    private var task: Task<Void, Never>?
    private var hasPolled = false

    public init() {
        let cfg = URLSessionConfiguration.ephemeral
        // Short enough that a wedged daemon cannot stack requests behind a 2s tick.
        cfg.timeoutIntervalForRequest = 1.5
        cfg.requestCachePolicy = .reloadIgnoringLocalCacheData
        session = URLSession(configuration: cfg)
    }

    public func start(every interval: Duration = .seconds(2)) {
        guard task == nil else { return }
        task = Task { [weak self] in
            while !Task.isCancelled {
                await self?.poll()
                try? await Task.sleep(for: interval)
            }
        }
    }

    public func stop() {
        task?.cancel()
        task = nil
    }

    public func poll() async {
        do {
            async let f: [Forward]? = get("/admin/forwards")
            async let s: Status = get("/admin/status")
            let (newForwards, newStatus) = try await (f, s)
            let old = sections
            forwards = newForwards ?? []
            status = newStatus
            if let v = newStatus.apiVersion, v > Self.apiVersion {
                lastError = "daemon speaks API version \(v); this app knows \(Self.apiVersion)"
            } else {
                lastError = nil
            }
            if hasPolled { onChange?(old, sections) }
            hasPolled = true
        } catch {
            lastError = "daemon not reachable at 127.0.0.1:9996"
        }
    }

    private func get<T: Decodable>(_ path: String) async throws -> T {
        var req = URLRequest(url: Self.base.appendingPathComponent(path))
        req.setValue("application/json", forHTTPHeaderField: "Accept")
        let (data, resp) = try await session.data(for: req)
        guard let http = resp as? HTTPURLResponse, http.statusCode == 200 else {
            throw URLError(.badServerResponse)
        }
        return try JSONDecoder().decode(T.self, from: data)
    }
}
