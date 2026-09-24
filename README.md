# portkeeper

Port mappings between your Mac and a remote dev box, without hand-rolling `ssh -L`.

portkeeper is a daemon on the Mac that keeps SSH port mappings to your remote dev boxes
alive. It owns one OpenSSH ControlMaster per host and adds or drops forwards on it on
demand, and you drive it from a web console or a menu-bar app. Nothing on the remote can
reach the daemon: its listener is loopback-only and no port is forwarded to it.

Two directions, named after the ssh flags they become:

| Direction        | ssh  | What it gets you                                                     |
| ---------------- | ---- | -------------------------------------------------------------------- |
| `local-forward`  | `-L` | A dev server on `code` opens in your Mac's browser                    |
| `remote-forward` | `-R` | A service on your Mac (notifications, an API) is callable from `code` |

This README describes what portkeeper is today. `DESIGN.md` records why it works this
way, and what it used to be.

## Quick start

Portkeeper is a Mac app for Apple Silicon, macOS 14 or later. It needs your remote hosts
as `Host` entries in `~/.ssh/config`, and `ssh <alias>` has to connect without a password
prompt, from keys or an agent. Portkeeper never edits that file.

1. **Download** the latest `Portkeeper-<version>-macos-arm64.zip` from the
   [Releases page](https://github.com/johnlofty/portkeeper/releases).
2. **Move `Portkeeper.app` to `/Applications`** and open it from there. Do this before the
   next step: registering the background helper records where the app is, and a copy
   registered from `~/Downloads` breaks the moment it moves.
3. **Get past the first-launch block.** The app is ad-hoc signed and not notarized, so
   macOS refuses to open it once. Either open System Settings > Privacy & Security and
   click **Open Anyway**, or run

   ```sh
   xattr -dr com.apple.quarantine /Applications/Portkeeper.app
   ```

   and open it again. The Portkeeper icon appears in the menu bar.
4. **Start the daemon.** Click the icon, then the gear, then **Background helper >
   Register**. If macOS asks, allow Portkeeper in System Settings > General > Login Items.
   Until that approval the helper is registered but never starts, and Settings says so.
   Within a few seconds **Daemon** in Settings reads reachable. Turn on **Open at login**
   there if you want the app back after a reboot.
5. **Map a port.** Click the icon, then **Add mapping**. Pick a host in the left rail,
   type the remote port, and click **Add mapping**. The row appears with an arrow pointing
   at your Mac, and the open button beside it loads `http://127.0.0.1:<port>`.

## Using it

**The menu bar item** shows how many mappings are alive, or `!` when a host is
reconnecting or the daemon cannot be reached. Click it for the popover: each host with its
state, and under it each mapping as a remote chip, a cable and a Mac chip, with its label,
a pin mark if it is kept across restarts, and an open button for local-forwards. The
buttons at the bottom are **Add mapping**, **Open console** and **Quit**. Quitting the app
leaves the daemon and every mapping running; the daemon belongs to launchd, not the app.

**The console** is where mappings are made. Open it from the popover, or load
<http://127.0.0.1:9996/> in any browser on the Mac. The left rail lists your hosts: the
`Host` aliases from `~/.ssh/config` plus any you add there. Picking one shows its mappings
as a table of cables. Each row reads left to right from the remote to your Mac, and **the
arrow points where the port appears**: a local-forward brings a remote port to your Mac,
so its arrow points right; a remote-forward publishes a Mac port on the remote, so its
arrow points left.

**Add mapping** opens a sheet:

- **Local-forward** or **Remote-forward**, with a live diagram of what you are about to make.
- **Remote port**, or a range such as `8000-8010` for one mapping per port.
- **Local port**, blank to use the same number. If that port is busy on your Mac, a free
  one is used and the row says so.
- **Target host on the remote**, for a machine the remote can reach, such as a database
  box. Leave it empty for the remote itself.
- **Label**, **Expires after** (1h, 8h, 24h or never), **Keep across restarts** and
  **Open in the browser when it is ready**.

A mapping with **Keep across restarts** is pinned: it never expires and comes back on its
own after the daemon or the Mac restarts. The row's menu offers **Edit**, pinning or
unpinning, and **Close mapping**; closing a pinned mapping removes the pin too.

**Listening on <host>** shows what the remote is actually running. Click **Discover**
and the table lists every listening port with its process or docker container, a
**Forward** button per row that opens the sheet already filled in, and an "already mapped"
mark on ports you have. Nothing runs on the remote until you click Discover.

**A worked example.** A Vite dev server on `code` prints `http://localhost:5173`. In the
console, with `code` selected, click **Add mapping**, type `5173`, click **Add mapping**.
Open the row's link and the page loads in your browser as `http://127.0.0.1:5173`. Tick
**Keep across restarts** if you want it there every morning.

**Where things live.** Pins are in `~/.config/portkeeper/pinned`, hosts you added in
`~/.config/portkeeper/hosts.conf` (ssh_config `Host` blocks), the host keys they record in
`~/.config/portkeeper/known_hosts`, the daemon's log at `/tmp/portkeeper.log`, and its ssh
control sockets under `~/.ssh/sockets/`, separate from your own sessions. Portkeeper reads
`~/.ssh/config` and never writes to it.

**If something is off.**

- The popover says the daemon is not reachable: the helper is not registered or not yet
  approved (Settings > Background helper), or a daemon from a repo checkout is running,
  and only one can own `127.0.0.1:9996`.
- A host stays **reconnecting**: `ssh <alias>` from Terminal has to work without a
  prompt. The console shows the attempt count and when the next try is; the log shows why.
- The first launch was refused: step 3 above. This is the price of an unsigned build,
  not a sign anything is wrong with the download.

**Uninstall.** Settings > Background helper > **Unregister**, turn off **Open at login**,
quit, and delete `/Applications/Portkeeper.app`. Remove `~/.config/portkeeper` and
`/tmp/portkeeper.log` if you want nothing left.

## How it works

```mermaid
flowchart LR
    subgraph mac["Your Mac"]
        browser["Browser"]
        app["Portkeeper.app<br/>menu bar"]
        console["Console<br/>127.0.0.1:9996"]
        daemon["portkeeperd<br/>launchd agent"]
        pins[("pinned mappings<br/>~/.config/portkeeper")]
        master["OpenSSH ControlMaster<br/>one per host, private ControlPath"]
        macport["127.0.0.1:8530"]
        macsvc["Mac service<br/>localhost:3000"]
    end

    subgraph code["Remote dev box: code"]
        sshd["sshd"]
        dev["dev server<br/>localhost:8530"]
        remoteport["127.0.0.1:3000"]
        tool["remote tool"]
    end

    browser --> console --> daemon
    app -->|"/api, polled"| daemon
    daemon -->|"ssh -O forward, cancel, check<br/>ss for discovery"| master
    daemon <-->|"re-created at startup<br/>and on every 30s tick"| pins
    master ===|"one multiplexed SSH connection"| sshd

    browser -.-> macport
    macport -.->|"local-forward, ssh -L"| dev
    tool -.-> remoteport
    remoteport -.->|"remote-forward, ssh -R"| macsvc
```

Solid arrows are control: how a mapping is asked for and kept. The thick line is the one
SSH connection the daemon owns per host. Dotted arrows are traffic through a mapping, and
every one of them rides that thick line; ssh binds the listening end and delivers to the
other. Nothing points from `code` back at the daemon, because nothing there can reach it.

Every thirty seconds the daemon checks each master, restarts and replays a dead one with
backoff, dials every local-forward to prove it still answers, and re-creates any pin
whose mapping is missing.

## Install from the repo

| Target             | What it does |
| ------------------ | ------------ |
| `make install`     | Builds `bin/portkeeperd`, renders the tracked plist with this checkout's path into `~/Library/LaunchAgents/io.github.johnlofty.portkeeper.plist`, and loads it |
| `make uninstall`   | Unloads that agent and removes its plist |
| `make app`         | Builds `build/Portkeeper.app`, the Swift app with `portkeeperd` bundled inside, ad-hoc signed. Registers nothing |
| `make app-install` | Builds the app, unloads and removes the dev agent, and copies the bundle to `~/Applications/Portkeeper.app` |
| `make dist`        | Builds the app and zips it with `ditto` to `dist/Portkeeper-$(VERSION)-macos-arm64.zip` (`VERSION` defaults to `dev`) |
| `make build`, `make run` | Builds the daemon; runs it in the foreground |

**Only one daemon can run.** The dev agent (`io.github.johnlofty.portkeeper`, from
`make install`) and the bundled helper (`io.github.johnlofty.portkeeper.helper`, inside
the app) both bind 127.0.0.1:9996, and the listen port is the daemon's singleton lock:
whichever starts second exits on it, and launchd keeps retrying it, filling the log. To
try the app against the dev agent, open `build/Portkeeper.app` and leave the helper
unregistered. To switch to the helper, run `make app-install`, then register from
Settings in `~/Applications/Portkeeper.app`, the copy that will stay.

Either way the daemon uses its own private ControlPath, so it never fights your
interactive sessions for a socket. Quitting the app does not stop the daemon or drop any
mapping; launchd owns it.

## The console

<http://127.0.0.1:9996/> on the Mac (or `localhost:9996`). The left rail lists hosts: the
`LG_HOSTS` entries, the `Host` aliases in `~/.ssh/config` (patterns such as `Host *`
are skipped), and any you add there. **Add host** takes what an ssh config entry has: an
alias, host name, user, port, identity file and optionally a jump host. It saves them to
`~/.config/portkeeper/hosts.conf`, then connects once to check they work. Every ssh the
daemon runs reads that file first and then your own `~/.ssh/config`, so the settings you
entered win and anything you left blank comes from your config. A host added this way is
not visible to `ssh` in a terminal; your ssh config is never changed. Picking one shows its mappings as a table of cables, remote
port on one side and Mac port on the other, with the arrow pointing where the port
appears, and a row's state (alive, dead, or reconnecting with the attempt count).

**Add mapping** opens a sheet: direction, remote port or range, local port (blank to
mirror), target host on the remote, a label, an expiry of 1h, 8h, 24h or never, **Keep
across restarts**, and whether to open it in the browser when ready. Rows can be edited,
pinned and closed. **Listening on** asks the host what it is running and offers a Forward
button per port. Opening the console at `/#add` goes straight to the sheet. The page
follows the system's light or dark appearance.

There is no login. What protects the console is that the daemon refuses cross-site
browser requests:

- a request whose `Host` is not the daemon's own address gets a 400;
- one the browser marks as coming from another site or origin (`Sec-Fetch-Site` other
  than `same-origin` or `none`, or a foreign `Origin`) gets a 403;
- a write whose body is not `application/json` gets a 415.

`GET /`, the page shell, gets the Host check only, so following a link to it still works.
So a web page open in your browser, including a dev server you have forwarded to
`127.0.0.1`, cannot drive it. A request with no browser headers at all is served, so
`curl` from a terminal works:

```sh
curl -s http://127.0.0.1:9996/api/status
curl -s http://127.0.0.1:9996/api/forwards
curl -s -X POST -H 'Content-Type: application/json' \
     -d '{"remote_port":8530}' http://127.0.0.1:9996/api/forward
```

The other routes are `PATCH` and `DELETE /api/forward/{ref}`, `GET` and `POST /api/hosts`,
`DELETE /api/hosts/{alias}`, and `GET /api/hosts/{alias}/listeners`.

What is deliberately not defended: anything running as you on the Mac can reach the
daemon, as it can read the ssh keys the daemon uses. The listener refuses to bind anything
but loopback, and no mapping may use the daemon's own port as its local port, since a
remote-forward of it would publish the console on the remote.

Upgrading from an older version: the login, the `/admin` routes, `expose` and the
`RemoteForward 9996` control channel are removed; see `DESIGN.md`. Nothing reads
`~/.config/portkeeper/admin-password` any more, so delete it when you like.

## The menu-bar app

The menu-bar item shows the count of mappings that are alive, or `!` when a host is
reconnecting or the daemon cannot be reached. The popover lists each host, connected or
reconnecting with its next retry, and under it each mapping as a remote chip, a cable and
a Mac chip, with its label, a pin mark if it is kept across restarts, and a button to open
a local-forward in the browser. It polls `GET /api/forwards` and `GET /api/status` on
`http://127.0.0.1:9996` every two seconds, and posts a notification when a host
reconnects or a mapping goes away.

**Open console** shows the web console in a window of its own; **Add mapping** opens that
window on `/#add`. The app has no native form and writes nothing itself.

Settings has **Open at login** (the app as a login item), **Background helper** (its
status, Register and Unregister, and a button to Login Items when approval is pending),
and **Daemon** (its address, state and API version, and a button to open the log). Both
registrations go through SMAppService.

## Configuration

The daemon reads environment variables; in the launchd plist they go under
`EnvironmentVariables`.

| Variable          | Default                                  | Meaning |
| ----------------- | ---------------------------------------- | ------- |
| `LG_LISTEN`       | `127.0.0.1:9996`                         | Console and API address; must be loopback |
| `LG_HOSTS`        | none                                     | Comma-separated hosts whose connections open at startup; the repo's dev plist sets `code` |
| `LG_SSH_CONFIG`   | `~/.ssh/config`                          | Where `Host` entries are discovered |
| `LG_HOSTS_FILE`   | `~/.config/portkeeper/hosts.conf`        | Hosts added in the console, as ssh_config |
| `LG_SSH_WRAPPER`  | `~/.config/portkeeper/ssh_config`        | Generated config every daemon ssh gets with `-F` |
| `LG_KNOWN_HOSTS`  | `~/.config/portkeeper/known_hosts`       | Where keys of console-added hosts are recorded |
| `LG_PINNED_FILE`  | `~/.config/portkeeper/pinned`            | Mappings kept across restarts |
| `LG_MAX_FORWARDS` | `20`                                     | Cap on mappings |
| `LG_DEFAULT_TTL`  | `28800` (seconds)                        | Expiry when a request gives none |
| `LG_CONTROL_PATH` | `~/.ssh/sockets/portkeeper-%r@%h-%p`     | The daemon's own ControlPath |

Hosts not in `LG_HOSTS` are dialled on first use. The hosts, wrapper and pinned files are
written 0600 in a 0700 directory. Both launchd jobs log to `/tmp/portkeeper.log`.

## Behaviors worth knowing

**Pins.** A pinned mapping is re-created at startup and on any tick that finds it
missing. Pinning clears the expiry, and closing a pinned mapping removes the pin. A pin to
a host known only from `~/.ssh/config` brings that host's connection up at startup.

**Discovery.** **Listening on** runs `ss` (or `netstat`) and `docker ps` on the host,
capped at 15 seconds. Ports already mapped are marked rather than hidden.

**Target host past the remote.** A local-forward may target a machine the remote can
reach, such as a database box, via **target host on remote** or `remote_host`. It is
validated like a host alias: letters, digits, dot, dash, underscore, no leading dash, no
colons. IPv4 addresses pass; IPv6 literals are not supported. These mappings get a longer
id, `code:local-forward:db:5432`.

**Ranges.** `8000-8010` in the remote port field makes one independent mapping per port,
up to 32 per request, within `LG_MAX_FORWARDS`. If one fails, only the ones that request
created are rolled back. Editing does not take ranges.

**Reconnecting.** A mapping on a host whose connection is down reads **reconnecting**, not
dead. Retries back off 30s, 1m, 2m, 4m, 8m, then every 10 minutes, and never stop. Asking
for a forward to that host retries at once. `GET /api/status` carries the per-host detail
under `hosts`, and the `LG_HOSTS` list under `eager_hosts`.

**Clean restarts.** A control socket left behind by a killed master is detected and
removed before a new one starts, and a new master gets three seconds to settle before any
forward is sent to it.

`DESIGN.md` has the reasoning for each of these, and the incidents behind the last two.

## Checking it works

```sh
make status                                  # is the dev agent loaded
make logs                                    # tail /tmp/portkeeper.log
ssh -o 'ControlPath=~/.ssh/sockets/portkeeper-%r@%h-%p' -O check code
                                             # is the daemon's master up
```

The log starts with `listening on 127.0.0.1:9996, hosts [code]` (`hosts []` for a
downloaded app, which opens connections on first use) and says `master code: up` a few
seconds after a host is used. If it says `still not ready` on every retry, the
host is unreachable or the connection is failing, and the console's rows for that host
read **reconnecting** with the attempt count. `make status` greps `launchctl list` for
`io.github.johnlofty.portkeeper`, which matches the helper's label too; the app's Settings
also shows the helper's state.

## Development

`make test`, `make vet` and `make fmt` run `go test`, `go vet` and `gofmt` over the
daemon. `make check` lints the launchd plist with `plutil`, since no compiler reads it.

The Swift package is in `macos/Portkeeper`: `swift build -c release` and `swift test`.
`swift test` needs XCTest from Xcode: if `xcode-select -p` points at the Command Line
Tools, prefix it with `DEVELOPER_DIR=/Applications/Xcode.app/Contents/Developer`.
`macos/build-app.sh`, behind `make app`, does that for itself.

CI (`.github/workflows/ci.yml`) runs on every push to `main` and every pull request, on a
`macos-15` runner: gofmt, vet, `go test -race`, `make check`, the Swift build and tests,
then `make dist`, uploading the zip as a workflow artifact. The release workflow
(`.github/workflows/release.yml`) runs on every `v*` tag: it builds with
`make dist VERSION=<tag>`, verifies the bundle's signature and plists, and publishes
`Portkeeper-<tag>-macos-arm64.zip` to GitHub Releases. To cut a release:

```sh
git tag v0.1.0 && git push origin v0.1.0
```

## License

[Anti 996 License, Version 1.0](https://github.com/996icu/996.ICU/blob/master/LICENSE) — see [LICENSE](LICENSE).
