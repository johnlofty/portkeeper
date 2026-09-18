# local-gateway

An SSH-based port-forwarding gateway: a daemon on the Mac that accepts port-forward
requests originating on a remote dev host, sets up the forward automatically against
an existing multiplexed SSH connection, manages its lifetime, and opens the resulting
URL in the local browser.

Status: **phase 2 implemented and validated end to end** (2026-09-16). Bidirectional
mappings, console CRUD behind a password login, and a public `/api` for remote tooling all
work against the live `code` host. Running under launchd. See Validation findings.

## Problem

Dev servers run on the remote host `code` (Azure box, see `~/.ssh/config`). To view one
in a browser on the Mac, the current workflow is to manually run:

```
ssh -N -L 8530:localhost:8530 code
```

...in a spare terminal, keep it alive, and remember to kill it later. This is manual,
requires knowing the port up front, costs a terminal per port, and leaks tunnels.

The concrete trigger is `markdown-preview.nvim` running inside nvim on `code`: it prints
a `http://localhost:<port>` URL that means nothing on the Mac until a tunnel exists.

## Goals

- A remote-side command (`expose 8530`) makes the port reachable on the Mac, with no
  manual SSH invocation and no new terminal.
- The Mac opens the browser at the right URL automatically. configurable, not needed in every use case.
- Forwards are tracked, deduplicated, and torn down — no leaked tunnels.
- Survives SSH reconnects (wifi flap, laptop sleep).

## Non-goals

- Exposing anything to the public internet (no ngrok/frp/Tailscale role).
- Forwarding to hosts outside `~/.ssh/config`'s known aliases.
- Replacing the existing notify-relay; this sits alongside it and reuses its pattern.

## Prior art

| Project                                                               | What it covers                                                                                                                                                                                                  | Gap                                                                                                                                                                |
| --------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| [wangnan0916/ssh-forward](https://github.com/wangnan0916/ssh-forward) | Go daemon under launchd/systemd, owns one private OpenSSH master, adds/cancels forwards via control commands, CLI over a user-only Unix socket, +20-port collision fallback, forwards persist across reconnects | **Pull-based**: the Mac polls remote procfs over `ssh HOST sh -s` to discover listeners. No remote-initiated requests. 0 stars, v0.1.0 — reference, not dependency |
| [lemonade](https://github.com/lemonade-command/lemonade)              | Mature client-side daemon accepting `open <url>` / copy / paste from a remote over a reverse-forwarded TCP port                                                                                                 | Only opens the URL; assumes the tunnel already exists                                                                                                              |
| [PortProxy](https://pypi.org/project/portproxy/0.4.0/)                | "Forwards and manages ports dynamically upon request, for machines in your SSH config"                                                                                                                          | Closest in spirit; obscure, page wouldn't load for evaluation                                                                                                      |
| [pkarsy/sshportfw](https://github.com/pkarsy/sshportfw)               | ControlMaster-based forwarding helper                                                                                                                                                                           | Static/manual, not request-driven                                                                                                                                  |

**The gap:** nobody does push-triggered forwarding. ssh-forward discovers, lemonade opens,
neither lets the remote say _"expose this port and open it."_ This project is
lemonade's channel (already built here as notify-relay) + ssh-forward's manager.

## Verified mechanism

Checked on this machine, 2026-09-15:

- `ssh -O check code` → `Master running (pid=4642)`
- `ssh -V` → `OpenSSH_10.3p1`
- `ssh -O` accepts only `check`, `forward`, `cancel`, `exit`, `stop`, `proxy`. **There is
  no `list`** — the master cannot be asked what it has forwarded. See State.

So the daemon never spawns `ssh -N` processes. It mutates the existing multiplexed
connection:

```
ssh -O forward -L 127.0.0.1:8530:localhost:8530 code   # add, instantly
ssh -O cancel  -L 127.0.0.1:8530:localhost:8530 code   # remove
ssh -O check code                                       # liveness
```

No new TCP connection, no new auth, per-port teardown that leaves other forwards alone.
This is the reason the design is cheap; it is also the single load-bearing assumption.

**Correction (2026-09-15, found during implementation).** An earlier draft claimed port
9998 was "reserved and unused" because `RemoteForward 9998 localhost:9998` appears in
`ssh/config` and `9998` appears nowhere else in the dotfiles repo. That grep was
misleading: Mac-side 9998 is held by `ccimgd`, a launchd-managed binary at
`~/.local/bin/ccimgd` living entirely outside the repo. That RemoteForward line exists to
serve **ccimgd**, not us.

local-gateway therefore uses **9996**, free on both ends, and needs its own line — so
"zero ssh config change required" was wrong:

```
RemoteForward 9996 localhost:9996    # added to ssh/config Host code
```

The general lesson, worth keeping: a grep of one repo does not establish that a port is
free on the machine.

## Architecture

```
  remote host `code`                          Mac
  ─────────────────                           ───
  expose 8530
      │ POST 127.0.0.1:9996/forward
      │   {"remote_port":8530,"direction":"local-forward"}
      └──────────[ ssh RemoteForward 9996 ]──────────▶ local-gateway daemon
                                                            │
                                    ssh -O forward -L  ◀────┤  (data plane)
                                    open -u http://...  ◀───┤  (browser)
                                                            │
      ◀───────── {"url":"http://127.0.0.1:8530",       ─────┘
                  "local_port":8530}
```

Two planes:

- **Control plane** — remote → Mac, over `RemoteForward 9996` (one added line in
  `ssh/config`; see the correction above). Identical transport to notify-relay
  (`claude/relay/notify-relay-daemon.py`, port 9997).
- **Data plane** — the `-L` forward itself, added to the live ControlMaster.

Unlike notify-relay, which is fire-and-forget, **the response matters**: the daemon
returns the actual allocated URL, because the local port may differ from the remote one.

### Components

Standalone repo for now (decision 2); folded into the dotfiles `install.sh` once it works.
Daemon in Go (decision 3), remote client in shell (decision 4).

| Path                                      | Role                                                                                                    |
| ----------------------------------------- | ------------------------------------------------------------------------------------------------------- |
| `cmd/gatewayd/`                           | Go daemon. HTTP on `127.0.0.1:9996`, endpoints below. launchd `KeepAlive`, loopback-only bind           |
| `io.github.johnlofty.portkeeper.plist`           | launchd agent, same shape as `io.github.johnlofty.claude-notify-relay.plist`                                      |
| `bin/expose`                              | Remote client. POSIX shell, `curl` + `jq` — same pattern as `tmux-notify.sh`                            |
| `web/index.html`                          | The console. Single file, `go:embed`ed into the daemon — no build step, no framework                    |

No state file. See State below.

**Why the client is not Go.** `code` is x86_64 Linux with **no Go toolchain** (checked
2026-09-15); the Mac is arm64. A Go client would mean cross-compiling and either committing
a multi-MB binary to git or adding an scp deploy step on every change. The client is a
~30-line curl wrapper, so it ships as a shell script via the dotfiles repo — already cloned
on `code`, symlinked into `~/.local/bin` by `install.sh`, updated by `git pull`. Go is used
where it earns its keep: the long-running daemon (concurrency, launchd, single binary, no
runtime deps).

### Protocol

```
POST   /forward         {"remote_port":8530,"host":"code","label":"mkdp","open":true,"ttl":28800}
       → 200 {"local_port":8530,"url":"http://127.0.0.1:8530","reused":false}
DELETE /forward/8530    → 200
GET    /forwards        → [{host,remote_port,local_port,label,created_at,ttl,state}]
GET    /                → the console (HTML)
```

In-memory entry: `host`, `remote_port`, `local_port`, `label`, `created_at`, `last_seen`,
`ttl`, `requester` (pid/session), `auto_opened`.

`open` **defaults to false** when absent — a scripted or headless `expose` must not hijack
the screen. Browser-opening is opt-in per request (`expose --open`), which is what the mkdp
hook passes.

## State — in memory only, no database

A mutex-guarded map in the daemon. No JSON file, no SQLite. The forwards themselves live
inside the SSH master, so persisting a copy of them would be persisting a claim about
another process's internals.

One wrinkle makes this non-obvious, and it is worth stating plainly: **OpenSSH cannot be
queried.** `ssh -O` accepts only `check`, `forward`, `cancel`, `exit`, `stop`, `proxy` —
there is no `list`. So the master is the source of truth in the sense that it *holds* the
forwards, but it will never tell you what it holds. The daemon is the only bookkeeper, and
an in-memory table can therefore drift from reality rather than being reconciled against it.

What closes that gap is decision 1. Because the daemon owns the master's entire lifetime,
it can guarantee consistency by construction:

- **On startup**, `ssh -O exit` on its own ControlPath, then establish a fresh master.
  Empty table, empty master — consistent by definition. No orphaned forwards from a
  previous daemon that nothing can now cancel or enumerate.
- **On master loss** (wifi flap, sleep), the daemon is still running, so the table is still
  in memory: re-establish and replay it.
- **Verification is behavioral, not declarative.** To check a forward is alive, dial
  `127.0.0.1:<local_port>`. Microseconds, no SSH round trip — which is why the reconcile
  loop can afford to probe actively rather than reap lazily.

The tradeoff, stated honestly: a daemon restart drops every active forward and you re-run
`expose`. That is the price of not persisting, and it is small — launchd `KeepAlive` makes
restarts rare, and after a Mac reboot the remote dev servers are usually gone anyway.

## UI — the console

Served by the daemon at `http://127.0.0.1:9996/`. It is already an HTTP server, so this is
one handler plus one `go:embed`ed HTML file: no framework, no build step, no second binary.

- Table of live mappings: host, remote port → local port, label, age, TTL remaining.
- Each row links to its own `http://127.0.0.1:<local_port>` and has a close button
  (`DELETE /forward/<port>`).
- Polls `GET /forwards` every few seconds; dead rows (failed dial) render greyed.

It is the only surface that offers *close* without a CLI, which is what makes it worth
building over a terminal listing. Pleasingly, the port-forwarding gateway's own console is
just another local URL.

`expose --list` stays available as a convenience — it is the same `GET /forwards` endpoint
and about three lines of `jq` in a script that already speaks to it.

Not doing, for now: a macOS menu bar item (no SwiftBar/xbar installed, so it means a Swift
app and a second language) and a tmux status segment (`.tmux.conf:135`, catppuccin modules).
Both are ambient-awareness features; revisit if leaked tunnels turn out to actually bother
you in practice.

## Open decisions

### 1. ControlPersist ownership — SETTLED

`ssh/config` sets `ControlPersist 10m`. The master dies 10 minutes after the last
interactive session exits, silently taking **every forward** with it. The same bug
already affects notify-relay today: with no session open, the remote cannot reach the
Mac at all.

Options considered:

- **(a) Daemon owns a dedicated always-on master** with its own ControlPath.
- (b) Raise the interactive config to `ControlPersist yes`.
- (c) Daemon auto-heals: `ssh -O check` every 30s, re-establish and replay the in-memory
  table.

**Decided: (a) + (c).** The daemon runs its own master on a ControlPath distinct from the
interactive `~/.ssh/sockets/%r@%h-%p`, so the two never fight over lifetime. Because
`ssh/config` applies to it too, the daemon's master also carries the 9996/9997/9998
RemoteForwards — which fixes the notify-relay bootstrap gap as a side effect: the remote
can reach the Mac even with no interactive session open. Cost is one always-on SSH
connection, with reconnect churn on wifi flap and sleep/wake, which (c) absorbs.

### 2. Local port selection

Mirroring (local == remote) is what makes it feel magic: a URL printed on the remote
means the same thing on the Mac. But 3000/5173/8080 will collide with Mac-side processes.

**Recommendation:** mirror first, fall back to a free port, and _always_ return the real
URL rather than letting the caller assume. Minor TOCTOU when probing for a free port
(probe-then-ssh-binds) — acceptable.

### 3. Bind address

**Loopback, explicitly:** `-L 127.0.0.1:8530:...`, never `-L 8530:...`. ssh-forward binds
`0.0.0.0` for VM reachability; here that would publish the remote's dev server to
whatever café LAN the laptop is on.

### 4. Lifetime management

- Explicit `expose --close 8530`.
- Absolute TTL, default ~8h.
- 30s reconcile loop: dial each `127.0.0.1:<local_port>` and drop entries that no longer
  answer; re-add forwards after a master restart (replayed from memory — see State).
- Dedup: re-requesting an existing `(host, remote_port)` returns the existing mapping
  rather than erroring.
- Foreground mode `expose --wait 8530` closes on SIGINT — today's `ssh -N -L` ergonomics,
  but driven from the remote side.

### 5. Push vs pull

Phase 1 is push (explicit `expose`). ssh-forward's pull-based discovery — poll `ss -ltn`
on the remote, auto-expose new listeners — is the true VS Code experience and composes
with phase 1 rather than replacing it. See Phasing.

## Security model

This is a meaningful escalation over notify-relay. That daemon's worst case is a spurious
desktop notification. This one's worst case is **arbitrary `open` on the Mac and arbitrary
forwards**. A `RemoteForward` binds the remote's loopback, so _any process or user on
`code`_ — including a compromised dependency in a repo being built there — can POST to it.

Constraints, all mandatory:

- **Never accept a URL from the payload.** Construct it Mac-side from the allocated port;
  scheme hardcoded to `http://127.0.0.1:<port>`. Otherwise this is arbitrary-URL-open
  (phishing pages, `file://`, custom schemes).
- **Validate `remote_port`** as an integer in 1024–65535.
- **Resolve the target host from a static allowlist** of ssh aliases, never from the
  payload.
- **Cap concurrent forwards** (e.g. 20).
- **`exec.Command` with argv only** — never a shell. Go's `os/exec` does not invoke a
  shell by default; keep it that way (no `sh -c`).
- **Bind the HTTP listener to `127.0.0.1` only.**

Unresolved: because everything arrives over loopback, **the daemon cannot tell which host
sent a request**. Fine with one remote host today.

> **Superseded by phase 2.** The token idea below was dropped. Loopback origin is now
> treated as carrying no authority at all, and the split is public `/api/*` vs
> authenticated `/admin/*`. See "Authorization by capability, not by port".

## markdown-preview integration (the payoff case)

`lazyvim/lua/plugins/markdown-preview.lua` currently has `mkdp_port` commented out, with
`mkdp_auto_close = 0` and `mkdp_combine_preview = 0`. Multi-preview therefore yields
_several random ports_ — which is exactly why a static `LocalForward` block cannot solve
this.

Wiring: set `mkdp_browserfunc` to a Lua function that parses the port out of the URL it is
handed and shells out to `expose <port> --open`. Keep `mkdp_open_to_the_world = 0` so the
preview stays on the remote's loopback. Then `:MarkdownPreview` on `code` pops a browser
on the Mac, and the fixed-port workaround stays unnecessary.

Optionally, a `MarkdownPreviewStop` autocmd fires `expose --close <port>`.

## Phasing

- **Phase 0 (20 minutes, worth doing regardless):** a static `LocalForward` block for
  3000/5173/8080 in `ssh/config`. Cannot cover random ports, and eagerly binds Mac ports
  at connect time — but it is free.
- **Phase 1:** daemon + `expose` + the mkdp `browserfunc` hook. Solves the stated problem.
- **Phase 2:** pull-based auto-discovery (poll `ss -ltn` on the remote, auto-expose new
  listeners matching a working-directory glob). The VS Code experience.

## Settled

1. **Master ownership** — daemon owns a dedicated always-on master + 30s heal/replay loop.
   See Decision 1.
2. **Repo location** — standalone here for now; folded into the dotfiles/Claude config once
   it is implemented and working.
3. **Daemon language** — Go.
4. **Remote client** — POSIX shell (`curl` + `jq`), not Go. `code` has no Go toolchain and
   the client is a thin HTTP wrapper; shipping it via the dotfiles repo avoids binary
   distribution entirely.

5. **State** — in memory only, no database, no state file. See State.
6. **UI** — a web console served by the daemon itself. See UI.

## Still open

- Whether `expose` should print a plain URL or something the terminal will hyperlink
  (OSC 8), given the URL is the whole point of the command.
- Whether the console should show *remote* listeners that are not yet forwarded (a preview
  of phase 2's auto-discovery) or only what is currently mapped.

## Validation findings (2026-09-15)

Four defects the design did not anticipate, all found by running against the live host
rather than by reading code. Recorded because each one invalidates an assumption written
higher up in this document.

**1. Port 9998 was not free.** See the correction under Verified mechanism. Cost: one
`RemoteForward` line and a port change across three components.

**2. `ssh -O forward` reports failure when it has actually succeeded.** Every control
command replays ssh_config's `RemoteForward` lines to the master. Ours are already bound,
so the mux request fails — *after* correctly installing the `-L`. Trusting the exit status
meant every forward looked broken, and in `replay()` it would have dropped every forward on
each master restart, silently defeating the heal loop.

`ClearAllForwardings=yes` looks like the fix and is a trap: it returns success while
clearing the requested `-L` too, so nothing binds. Worse than the failure it replaces.

The real fix is the one this document already argued for on other grounds: **judge by
dialing, not by exit status.** OpenSSH cannot be queried, so behavior is the only truth.

**3. `probeFree` had a false positive that produced a silently wrong forward.** Go sets
`SO_REUSEADDR` on its listeners, so on macOS binding `127.0.0.1:p` *succeeds* while another
process holds the wildcard `*:p`. ssh does not set it, so ssh then failed to bind — and the
dial-based success check saw the *other* process answering and reported a working forward
pointing at entirely the wrong service. Observed with OrbStack on `:3030`.

`probeFree` now dials before binding: anything that answers owns the port, whatever `bind()`
is willing to claim. This one is worth remembering — it is invisible in unit tests, and the
symptom is a forward that works, just not to the machine you meant.

**4. `Close()` deleted the record before cancelling.** A failed cancel therefore left a
forward that ssh still held and nothing could list, retry or cancel — the exact orphan that
in-memory state is supposed to make impossible (see State). The record is now dropped only
once the port stops answering.

### Verified working

- Remote → Mac control channel over `RemoteForward 9996`.
- Data plane proven with a nonce: a unique payload served on the remote arrived byte-exact
  through the forward. Identical-looking HTTP responses are *not* proof — finding 3 came
  from two services returning the same `404`.
- Mirror-then-fallback: mirrors when genuinely free (9111→9111), falls back when not
  (8530→53525, because the user's own manual `ssh -L 8530` still holds that port).
- Dedup, TTL accounting, `--list`, close from both CLI and console, teardown releasing the
  OS-level listener.
- **Safety invariant held throughout**: the user's interactive master (pid 4642), their
  manual 8530 tunnel, notify-relay on 9997 and ccimgd on 9998 were untouched across every
  daemon start, stop and crash.

### Known gaps

- Fallback ports come from the ephemeral range (53525, 54301). ssh-forward's "try up to 20
  higher ports" would give friendlier, more memorable URLs (8531 rather than 53525).
- TTL is absolute and is not extended on a dedup hit, so a preview left open past 8h expires
  mid-use.
- `reconcile()` has no direct unit test; its components are tested individually.

---

# Phase 2 — bidirectional mappings, console CRUD

Decided 2026-09-16. Supersedes parts of phase 1 where they conflict.

## Terminology

The UI and API say **`local-forward`** and **`remote-forward`**, mapping one-to-one onto
ssh's `-L` and `-R`. "Import/export" was tried and rejected as ambiguous — it makes the
reader work out which end is which, and that is exactly the confusion worth designing out.

| Direction        | ssh  | Meaning                                                    |
| ---------------- | ---- | ---------------------------------------------------------- |
| `local-forward`  | `-L` | Remote's `:R` becomes reachable at `127.0.0.1:L` on the Mac |
| `remote-forward` | `-R` | Mac's `:L` becomes reachable at `127.0.0.1:R` on the remote |

## Why remote-forward earns its place

Not symmetry for its own sake. Remote processes need to reach selected localhost services
on the Mac. The motivating case: a remote Codex session going idle POSTs to `127.0.0.1:3000`
on the remote, which is remote-forwarded to a notification service on the Mac that fires
`osascript`. That is the existing notify-relay pattern generalized — and notify-relay's
hand-maintained `RemoteForward 9997` line in `ssh/config` is precisely the static config
this replaces with something dynamic.

## The control channel stays

Phase 1 considered deleting `RemoteForward 9996` to close the remote-facing attack surface.
**Rejected.** Trusted remote tooling needs to request mappings dynamically, and the Codex
case above is exactly that. The channel is **restricted rather than removed**: it is an
intentionally public capability API exposing only a small set of safe operations. See
"Authorization by capability, not by port".

## Interface

The console at `http://127.0.0.1:9996/` becomes the primary interface and gains full CRUD:
add (with a direction selector), edit, delete. `expose` survives as the thin scriptable
path for hooks — it is no longer the main way a human drives this.

## Verification, revisited

Phase 1 verifies a forward by dialing its local port. That does not generalize: a
`remote-forward`'s listening socket is on the *remote* host, so there is nothing local to
dial.

Worse, the phase-1 tolerance rule is too blunt. It treats *any* non-zero `ssh -O forward`
exit as benign if the port answers, which is how finding 3 (the OrbStack false positive)
slipped through.

**The precise rule:** ssh names the port in each failure line — `remote port forwarding
failed for listen port 9996`. The config-replay failures always name 9996/9997/9998; ours
names *our* port. So tolerate a non-zero exit only when **no reported failure references the
port being requested**. That discriminates correctly in both directions and does not depend
on dialing anything.

Dialing remains the liveness check for `local-forward` during reconcile. For
`remote-forward`, liveness needs a round trip to the remote, so reconcile checks it less
often than the local ones and never holds a lock across it.

## Authorization by capability, not by port

Revised 2026-09-16, replacing an earlier two-port design.

Authenticating the control channel runs into a real problem: the Mac's browser and the
remote both arrive as loopback traffic, so they are indistinguishable by origin. An earlier
draft answered this by splitting the surfaces across two ports (console on a never-forwarded
9995, control channel on 9996). That works, but it makes the port number the security
boundary, which is brittle — one stray `RemoteForward` line and the guarantee silently
evaporates.

**One listener on `127.0.0.1:9996`, authorization enforced per endpoint.**

| Surface    | Auth                | Authority                                             |
| ---------- | ------------------- | ----------------------------------------------------- |
| `/api/*`   | none, by design     | Small, safe capability set. Assume any process on the remote VM can call it. |
| `/admin/*` | session cookie      | Full CRUD both directions, edit, config, status, `open`. |

**Public `/api/*`** deliberately exposes only what is safe to hand an untrusted caller:
create and close a **local-forward**, and list. It rejects `remote-forward` (publishing an
arbitrary Mac service to the remote is an escalation) and rejects `open` (popping a browser
is a screen-hijack). This is the surface `expose` and remote tooling use.

**Admin `/admin/*`** is the console. `POST /admin/login` takes the password and returns a
random, short-lived session:

```
Set-Cookie: lg_session=<random>; HttpOnly; SameSite=Strict; Path=/
```

Subsequent admin requests carry the cookie rather than the password. The password is
supplied at launch via `LG_ADMIN_PASSWORD` (our existing `LG_` prefix; the same idea as the
`PORTD_ADMIN_PASSWORD` sketch). With it unset, admin fails closed and only the public API
works. macOS Keychain would be a better long-term store than an environment variable, but
env is reasonable for a localhost developer daemon.

### Invariants

These are the point of the design, not incidental hardening:

1. **The daemon may verify the admin secret but must never expose it.** Not in HTML or JS,
   not via a config or status endpoint, not in an error body. Verification only, never
   retrieval.
2. **Never log the password**, including on failed logins.
3. **Never treat `127.0.0.1` as evidence of admin authority.** Requests arriving through the
   SSH RemoteForward are indistinguishable from local ones at the socket level. Source IP and
   port are not evidence of anything. **An unauthenticated request is public, regardless of
   where it actually originated.**
4. The token-file mechanism from the earlier draft is **removed**. A secret copied onto the
   remote VM never defended against other processes on that same VM — which was the only
   threat it was nominally there for.

### The control plane is not the mappings

Worth stating because the two are easy to conflate: 9996 carries *requests about* mappings.
The mappings themselves are ordinary forwards carrying user traffic, independent of it:

```
remote Codex --POST--> remote :3000 --[ssh -R]--> Mac :3000 --> notification service --> osascript
```

That path involves the control plane only once, when the mapping is created.


## Phase 2 validation (2026-09-16)

Verified against the live host, not just unit tests:

- **Authorization holds from the real remote box.** From `code`: `/admin/forwards` → 401,
  a login attempt → 401, and `POST /api/forward` with `direction: remote-forward` → refused.
  Loopback origin grants nothing, which was the point.
- **The password is not retrievable.** Swept `/`, `/admin/forwards`, `/admin/status`,
  `/api/forwards`, `/config`, `/status` plus both login paths — zero occurrences in bodies
  or headers. Absent from logs; its length is not logged either. A failed login records only
  `admin login rejected`.
- **Password file guard works.** A `0644` file is refused with an actionable message and
  admin fails closed while `/api` keeps serving; `0600` is accepted; exactly one trailing
  newline is trimmed.
- **`remote-forward` proven with a nonce**: a service on the Mac was fetched from the remote
  through the mapping — the Codex-idle→notification path, end to end.
- **`local-forward` still works**: `expose 8530` from the remote via the public API.
- **Console works against the real daemon**: login, both directions rendered with the remote
  box tinted so the machine is obvious, edit/delete affordances, logout, and a correct
  `incorrect password` error state.
- Safety invariant held again: the interactive master, notify-relay on 9997 and ccimgd on
  9998 were untouched across every restart.

One reconciliation: the daemon had inlined its own login page in Go while the console built
one in `web/index.html`. Two login UIs would have drifted, so `GET /` now always serves the
console shell and the page decides for itself. The shell carries no mappings, no config and
no password — every data route is 401-gated — so serving it anonymously costs nothing. The
test that asserted the old behavior was rewritten to assert that invariant instead of the
mechanism.

### Still open

- **`expose --open` is now rejected**: `open` is an admin capability and the deployed client
  has no session, so the phase-1 mkdp payoff (preview on the remote pops a browser on the
  Mac) does not work. Either give hooks an admin path or exempt `open` on `/api` with the
  URL still server-constructed. Undecided.

## Host discovery (2026-09-18)

The console picks a host from a list rather than taking a typed string. Three sources feed
it: `LG_HOSTS`, `~/.ssh/config`, and hosts added by hand in the console.

**Discovery is not authorization.** This is the whole point. This machine's ssh config has
15 connectable aliases — a router, a Pi, several portals, a GCP box. Letting discovery
populate the allowlist would mean a process on the remote VM could ask the Mac to open a
tunnel to any of them, because `/api` is unauthenticated by design. So:

| Caller | May target |
| ------ | ---------- |
| `/admin` (you, authenticated) | anything the console lists: public, discovered or manual |
| `/api` (anything on the remote VM) | `LG_HOSTS` only — unchanged |

The console marks a host it can reach but `expose` cannot, so that difference shows up in
the picker rather than as a 403 later.

**Parsing.** `Host` lines only, with `HostName`/`User` for context and `Include` followed to
a depth of 4. Patterns are skipped rather than listed: `Host *` is a matching rule, not a
machine, and offering `*` as somewhere to forward to would be nonsense.

**Aliases are validated, not escaped.** An alias becomes an argv element handed to ssh.
argv already rules out a shell, but it does **not** stop ssh parsing its own flags, so
`-oProxyCommand=...` as a "host" would be read as an option. Anything not matching
`[A-Za-z0-9_][A-Za-z0-9_.-]*` is rejected at every entry point.

**Masters are opened lazily, with one exception.** Holding an ssh connection to every alias
in a config would be absurd, so a discovered host is dialled on first use. Public hosts stay
eager: their master carries the `RemoteForward` the control channel arrives on, so it has to
exist before anything out there calls in.

**Manual hosts persist**, unlike forwards. That is not a contradiction of State above: a
forward mirrors ssh's own internals and dies with the master, whereas a host someone typed
is configuration and belongs in `~/.config/local-gateway/hosts`.

## The control channel must be asserted, not assumed (2026-09-18)

Found by `expose` failing with connection-refused while the Mac looked entirely healthy:
the daemon was running, its master was up, and its listener was bound — but the remote had
**no listener on 9996 at all**, so nothing out there could reach us.

The cause is a race with the user's own shell. `ssh/config` carries
`RemoteForward 9996 localhost:9996`, so *every* connection to that host asks for the same
remote port — the daemon's master and the user's interactive session alike. Whichever
connects second loses, and ssh reports it as
`remote port forwarding failed for listen port 9996`, which `masterLog` deliberately
filters as noise. That filtering is right when the other holder is a live session: the
tunnel still works, and logging it every reconnect would be pure spam.

The trap is what happens *next*. The interactive session disconnects, the remote port falls
free, and nothing re-requests it. The daemon has no idea: from its side the master answers
`ssh -O check` and the local listener is fine. `expose` is simply dead until someone
restarts the daemon by hand.

So the daemon now **requests the forward itself** — `-R <listen>:localhost:<listen>` — every
time a master comes up and again on every reconcile tick for a public host. The request is a
local mux round trip and is idempotent, so re-asserting costs almost nothing, and an
already-bound port fails harmlessly.

Verified by breaking it deliberately: cancelling the forward behind the daemon's back left
the remote with 0 listeners and `expose` refused; 34 seconds later the daemon had re-bound
it and `expose` worked again.

The general lesson, which applies well beyond this line of code: **a health check that only
looks at your own side of a tunnel is not a health check.** Everything local was green while
the thing the feature exists for was broken.
