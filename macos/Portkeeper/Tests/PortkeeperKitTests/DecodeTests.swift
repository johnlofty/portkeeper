import XCTest
@testable import PortkeeperKit

/// The literals are the daemon's own output shapes (forwardView, and the /api/status
/// map in http.go). If a field is renamed on the Go side, this is where it shows.
final class DecodeTests: XCTestCase {
    func testDecodesForwardAndStatus() throws {
        let forwards = """
        [{"id":"code:local-forward:3000","host":"code","direction":"local-forward",
          "remote_host":"","remote_port":3000,"local_port":3000,"label":"dev server",
          "url":"http://127.0.0.1:3000/","created_at":"2026-09-24T10:00:00Z",
          "ttl":0,"expires_in":0,"age":42,"state":"alive","requester":"console",
          "auto_opened":false,"pinned":true},
         {"id":"code:remote-forward:9997","host":"code","direction":"remote-forward",
          "remote_host":"","remote_port":9997,"local_port":9997,"label":"notify-relay",
          "url":"","created_at":"2026-09-24T10:00:00Z","ttl":28800,"expires_in":28000,
          "age":800,"state":"reconnecting","requester":"","auto_opened":false,"pinned":false}]
        """
        let status = """
        {"api_version":1,"version":"v0.1.4","listen":"127.0.0.1:9996",
         "hosts":{"code":{"healthy":true,"attempts":0,"next_retry_in":0,"last_error":""},
                  "gcp-ubuntu":{"healthy":false,"attempts":3,"next_retry_in":40,
                                "last_error":"ssh: connect to host timed out"}},
         "eager_hosts":["code"],"max_forwards":64,"default_ttl":28800,"forwards":2,"pinned":1}
        """
        let fs = try JSONDecoder().decode([Forward].self, from: Data(forwards.utf8))
        XCTAssertEqual(fs.count, 2)
        XCTAssertEqual(fs[0].remoteHost, "")
        XCTAssertEqual(fs[0].localPort, 3000)
        XCTAssertTrue(fs[0].pinned && fs[0].isLocalForward && fs[0].isAlive)
        XCTAssertEqual(fs[1].direction, "remote-forward")
        XCTAssertEqual(fs[1].expiresIn, 28000)

        let st = try JSONDecoder().decode(Status.self, from: Data(status.utf8))
        XCTAssertEqual(st.apiVersion, 1)
        XCTAssertEqual(st.version, "v0.1.4")
        XCTAssertEqual(st.hosts?["gcp-ubuntu"]?.attempts, 3)
        XCTAssertEqual(st.hosts?["gcp-ubuntu"]?.nextRetryIn, 40)
        XCTAssertEqual(st.defaultTTL, 28800)

        // Go encodes a nil map or slice as null; an idle daemon must still decode.
        let idle = try JSONDecoder().decode(Status.self, from: Data("""
        {"listen":"127.0.0.1:9996","hosts":null,"eager_hosts":null,
         "max_forwards":64,"default_ttl":28800,"forwards":0,"pinned":0}
        """.utf8))
        XCTAssertNil(idle.apiVersion)
        XCTAssertNil(idle.version)
        XCTAssertNil(idle.hosts)

        let sections = HostSection.build(forwards: fs, status: st)
        XCTAssertEqual(sections.map(\.alias), ["code", "gcp-ubuntu"])
        XCTAssertEqual(sections[0].forwards.count, 2)
        XCTAssertFalse(sections[1].healthy)
    }
}
