# portkeeper

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
| [ruiyangke/porthop](https://github.com/ruiyangke/porthop)             | Tauri/russh macOS app. A "tunnel" is a **named, persisted `-L` profile** you start and stop, with an explicit connection state machine (connecting/connected/retrying) surfaced in the UI, plus remote listener discovery | **Borrowed**: persisted mappings (our pins), the discovery-with-one-click-forward UI, and reporting reconnect state rather than a binary up/down. **Not borrowed**: in-process SSH (russh) and one TCP connection per tunnel — we keep OpenSSH and one multiplexed master. Still not request-driven from the remote |

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
| `cmd/portkeeperd/`                           | Go daemon. HTTP on `127.0.0.1:9996`, endpoints below. launchd `KeepAlive`, loopback-only bind           |
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

> **Superseded 2026-09-24.** It went after all. Nothing on the remote ever called `expose`, and the console's
> discovery table and pins cover both of the cases this section argued for. See
> "Retiring the control channel".


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

> **Superseded 2026-09-24.** There is one authority level now: every route except `GET /`, login and logout
> requires the admin session, and nothing on the remote can reach the daemon at all.
> The invariants below still hold; the public tier they were shaped around is gone.
>
> **Superseded 2026-09-24, again.** The admin session went too; there is no password and no
> login. See "Dropping the login".


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

> **Superseded 2026-09-24.** Invariants 1 and 2 have nothing left to protect: there is no admin
> secret and no session. Invariant 3's first sentence stands with its object changed — source
> address is still not evidence of anything — but "an unauthenticated request is public" no
> longer describes the model, because there is no authentication to lack. What the daemon
> now checks is whether a request came from a page it served, or from no browser at all. See
> "Dropping the login".

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

> **Superseded 2026-09-24.** The re-assert this section introduced is gone with the channel it asserted. The
> lesson stands, and it is the one that later found the stale-socket wedge.


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

## Pinned mappings, discovery, backoff (2026-09-23)

Four things the daemon could not say, each of which turned out to be the same kind of
omission: it knew something and had nowhere to put it.

### A pin records intent; the table records what ssh holds

`State — in memory only` argues that persisting forwards means persisting a claim about
another process's internals. That argument still stands, and a pin does not contradict
it, because a pin is not a forward. It is a sentence in the imperative mood: *this
mapping should exist*. The live table stays exactly what it was — a mirror of what the
master currently holds, empty at startup, consistent by construction. Reconcile's new
last step is to compare the two and fix the difference.

The distinction is the same one that already justified `~/.config/local-gateway/hosts`:
a host someone typed is configuration, a forward is runtime. Pins live one file over, at
`~/.config/local-gateway/pinned`, 0600 in a 0700 directory, re-validated on load —
because its contents become ssh argv, and "we wrote it ourselves last time" is not a
provenance check.

A pin carries **no TTL**. "Keep this across restarts" and "drop this in eight hours" are
contradictory instructions, so pinning clears the lease rather than racing it; a pinned
mapping that expired would be re-created within thirty seconds by the very loop that is
supposed to honour it, which is the daemon arguing with itself in public.

For the same reason, **closing a pinned mapping removes the pin**. Anything else means
clicking × and watching the row come back, which reads as the daemon ignoring you.

A pin to a host that only exists in `ssh_config` **forces that master up eagerly**, which
is a deliberate exception to "discovered hosts are dialled lazily". The lazy rule exists
because holding a connection to every alias in a config would be absurd; someone who
pinned a mapping to that box has said the mapping should exist whether or not anyone asks
for it today, and that is a different statement.

### `/api` cannot touch pins, and cannot name a third host

> **Superseded 2026-09-24.** There is no `/api`. Pins, `remote_host`, discovery and ranges are all admin
> operations because everything is; the refusals described here no longer exist as code.


The public tier's rule has not changed: it may create, list and close a local-forward to
`LG_HOSTS`. Everything added here fails that test for a specific reason rather than out
of caution.

- **Pins are configuration.** A process on the remote VM may ask for a tunnel; it may not
  edit what this Mac does at startup. It may *see* the pin flag in a listing — knowing a
  mapping is pinned is not authority over it — and it gets a 403 both for `"pinned": true`
  on create and for closing a pinned mapping.
- **`remote_host` is a reach.** A local-forward with a target other than localhost turns
  this daemon into a way onto the remote's *network*: `db`, the metadata endpoint, the
  neighbour's admin panel. That the remote can reach those is not a reason the caller may
  hand them out. 403.
- **Discovery is reconnaissance.** `GET /admin/hosts/{alias}/listeners` enumerates a
  machine's open ports and the processes behind them, which is admin-only for exactly the
  reason `GET /admin/hosts` already is.

Ranges are public, because a range is just N of the operation `/api` already permits.

### `remote_host` is validated, never escaped, and IPv6 is out of scope

The value is spliced into `-L 127.0.0.1:L:HOST:R`. There is nothing to escape it *with*:
the spec is colon-delimited and positional, so a colon in the host silently re-cuts the
whole string into different fields — `db:80:other` is not a host with a funny name, it is
a different target and a different port. A leading dash has the older problem: argv rules
out a shell, but it does not stop ssh reading its own flags.

So the same `safeAlias` class an ssh alias must pass applies here. IPv4 dotted quads pass
it. **IPv6 literals cannot, and are deliberately unsupported** — supporting them means
bracket syntax inside a colon-delimited field, which is precisely the ambiguity this rule
exists to remove. Nothing on the far side of this tunnel has needed one.

The id grows a field only when the target is set: `host:direction:port` when it is empty,
`host:direction:remote_host:port` when it is not. The short form is what `bin/expose`
builds by hand, so every deployed copy keeps naming the mappings it always did; the long
form is unambiguous because the value cannot contain a colon, and `find()` compares whole
ids rather than splitting them anyway.

### Backoff, and why it never gives up

`reconcile` used to spawn `ssh` every thirty seconds forever against a box that was simply
switched off, and log a line each time. The schedule is now 30s, 1m, 2m, 4m, 8m, capped at
10 minutes, reset by any success. A tick inside the window does *nothing*: no process, no
log line. An explicit admin `Open` ignores the schedule entirely — backoff is there to
stop a background loop being rude, and someone who has just asked for a forward has said
something the loop did not know.

It has **no terminal state**. A laptop sleeps for hours and wakes with the same mappings
still wanted; "gave up" would mean the operator has to notice and intervene, which is the
opposite of what a daemon is for. The cap exists to bound noise, not to declare defeat.

That state is also worth *reporting*, which is where the third row state comes from. A
forward on a host whose master is down is **`reconnecting`**, not `dead`. Calling it dead
sends someone off to re-create a mapping that is coming back on its own, and the daemon
knew better the whole time. `List()` therefore reads host health under the same lock as
the table, so a row's state cannot disagree with the connection it depends on, and
`/admin/status` grows a `hosts` map (attempts, next retry, last error) that the console
turns into "reconnecting, attempt 3, next try in 2m".

The flat allowlist that used to live at `status.hosts` moved to `public_hosts`. What an
operator wants from that route is whether the link is up, which a list of names cannot
answer.

### Discovery is a separate command from the liveness probe

`argvListeners` (`ss -ltnH`) stays exactly as it is: it answers one cheap yes/no question
for remote-forward reaping, several times a minute. Discovery is a different job and gets
its own fixed command — `ss -ltnpH || netstat -tlnp`, an unprivileged `sudo -n` retry for
process names, a marker line, then `docker ps`. Every stage swallows its own errors,
because the boxes this runs against do not agree on what is installed and a missing tool
must not cost us the stages that worked.

Nothing from a request is interpolated into that string. The only user-supplied value in
the whole invocation is the host alias, which is a separate argv element and has passed
`safeAlias` — the same rule that has kept every other remote command safe here without a
single quoting decision.

It is the one call with a **timeout** (15s, `exec.CommandContext`). Everything else the
daemon runs is a local mux round trip that returns in milliseconds; this one runs real
programs on the far side, and a wedged docker daemon there must not hold an HTTP handler
on this Mac open indefinitely.

An entry that is **already forwarded is marked, not hidden**. "Why is 3000 missing from
this list" is a worse question to leave someone with than one row saying it is patched.

### Ranges expand server-side into independent mappings

`8000-8010` is a convenience at the request layer only. It becomes eleven ordinary
records with eleven ids, because anything else would mean inventing a second kind of
thing that can be closed, edited and reaped — and the moment one port of a range dies,
the abstraction is a lie anyway.

They install sequentially (each mutates the same master, and the port allocator reads a
table the previous one just wrote), after a preflight that validates every member, and a
failure part way through **rolls back only what that call created**. A request that
overlapped an existing mapping reuses it, and tearing that one down over an unrelated
failure would break something that was working before the call arrived. The cap is 32 per
request; `MaxForwards` still applies on top.

The response keeps its old flat shape when no range was asked for, and only then:
`bin/expose` reads `.url` off the top level, and changing that for every caller in order
to serve a feature they did not use would be a poor trade. A range answers with
`{"forwards": [...]}`, which `expose` reads with jq and refuses to guess at without it.

## A stale control socket wedges the daemon (2026-09-23)

Found live. `expose` and the console both failed with "ssh connection to code did not come
up ... Control socket connect(...): Connection refused", while the Mac was online, the
remote answered on port 22, and the user's own interactive master was fine. The log showed
the same three lines every 30 seconds:

```
master code: gone, restarting
ssh[code]: ControlSocket .../local-gateway-dev@203.0.113.7-22 already exists, disabling multiplexing
master code: still not ready: ... Connection refused
```

It had been doing so for **two days**. Line 1 of the log is the daemon starting at 12:56:57
on the 21st, right after `make install`; line 2 is the warning above. The socket file dated
from 12:33 that day and belonged to the previous daemon instance, the one `make install`
had just replaced. Its `Shutdown()` ran `stop()`, which sent `-O exit` and then SIGKILLed
at once; a kill that lands before the master reaches its own cleanup leaves the file
behind. The new daemon then could not recover, and no daemon restart could either, because
`Start()` runs the same `stop()` then `start()` and hits the same wall. Nothing surfaced it
until someone tried to use it.

The assumption that broke: that OpenSSH would reuse or replace a leftover socket. It does
neither. Started with `-M` against a path that already exists, it disables multiplexing and
runs as a plain session, which answers no `-O check`. So the daemon saw a master that never
came up, killed it, started another, and got the same result.

Two changes in `sshMaster`:

- **`stop()` gives the master two seconds to exit on its own** after `-O exit` before it
  kills anything. A master that exits cleanly unlinks its socket. The kill is the fallback,
  not the first move.
- **Both `start()` and `stop()` remove a stale socket**: one that exists, is a socket, and
  refuses connections. A live socket belongs to whoever is serving it and is left alone, and
  a regular file at that path is not ours to touch. The path is the one ssh will actually
  use, learned from `ssh -G`, which prints the resolved configuration with `%r@%h-%p`
  expanded and never connects anywhere.

The immediate recovery on the live machine was `rm` of the one stale file, after confirming
that connecting to it was refused and that no process held it. The running daemon bound a
fresh master on its next tick.

### The second thing the recovery uncovered

With the socket gone, the master came up, and then died within a second. Twice:

```
15:18:22 master code: gone, restarting
ssh[code]: ssh_confirm_remote_forward: parse packet: incomplete message
15:18:52 master code: gone, restarting
ssh[code]: ssh_confirm_remote_forward: parse packet: incomplete message
15:19:22 master code: gone, restarting
```

The third start lived. That message is a `fatal` inside ssh's handler for the reply to a
`tcpip-forward` request, on a code path that is only reachable when the request asked for
port 0. Nothing here asks for port 0. What fits the evidence is this: at startup the master
sends one request per `RemoteForward` line in ssh_config (9996, 9997, 9998 here) and
registers a reply handler for each holding a pointer *into* its `options.remote_forwards`
array. The mux socket appears before those replies come back from Azure. The daemon's
`waitReady` saw the socket within 200ms and immediately sent `-O forward -R` for the control
channel, which appends to that same array; when it moves, the three pending handlers read
freed memory, and a garbage port of 0 sends one of them down the fatal path. The third
attempt survived because the replies happened to land first.

So "answers `-O check`" is not "ready for `-O forward -R`", and the mux protocol offers no
way to ask. **`waitReady` now waits a further three seconds after the first successful
check** before returning, which is well beyond any plausible round trip. Every path that
brings a master up goes through it, so the pause covers the control-channel re-assert, the
replay, and an admin's first forward to a lazily dialled host.

The alternative considered and rejected for now: `ClearAllForwardings=yes` on the master's
own command line, so it sends no startup requests at all and the race cannot exist. It is
clean, but decision 1 leans on the daemon's master carrying the 9997 and 9998 lines so
notify-relay and ccimgd stay reachable with no interactive session open. With pins those two
could become explicit remote-forward pins and the config lines could go; that is a change to
make deliberately, not as a side effect of a bug fix.

The general lesson joins the one under host discovery: **a restart is only a recovery when
it starts from a clean slate.** The daemon restarted its master every thirty seconds for two
days and not one of those restarts changed anything, because the thing that was wrong was
outside the process being restarted. And a health check that reports green on the first
possible instant is measuring "exists", not "ready".

## Retiring the control channel (2026-09-24)

`expose`, the public `/api`, and the `RemoteForward 9996` control channel are gone.

The case for keeping them was made twice in this document and both times it rested on the
same sentence: trusted remote tooling needs to request mappings dynamically. Checked on
2026-09-23 against the actual remote: `expose` was installed and nothing called it. No
editor hook, no script, four entries in a shell history. The markdown-preview hook that was
the original payoff case was never wired, and once `open` became admin-only in phase 2 it
could not have delivered the payoff anyway.

Meanwhile the console grew the two things the channel was really for. The **Listening on**
table with its Forward button is `expose <port>` with the process name filled in. Pins are
the standing mappings a hook would otherwise have had to re-request. Between them there
was nothing left for a remote caller to ask.

What the channel cost, in hindsight, is the more persuasive half of the argument:

- It was the daemon's **only unauthenticated surface**, and the whole "authorization by
  capability" model existed to make that surface safe.
- It needed a `RemoteForward` line in ssh_config that the user's own interactive session
  fought over, which is why the daemon re-asserted it every tick.
- That re-assert, fired within 200ms of a master answering, is what tripped the OpenSSH
  use-after-realloc that killed the master twice on 2026-09-23.
- It meant a client script to ship, a deploy step, a host file on the remote, and a
  two-tier host model whose difference the console had to explain in its picker.

### What the daemon is now

> **Superseded 2026-09-24.** Not the login, the logout or the 401s: those went the same day. The
> route table is now proved against the origin guard instead of the session cookie, and
> reaching the host list no longer requires a login. See "Dropping the login". The
> remote-cannot-reach-it half stands, and is what made dropping the login possible.

One authority level. `GET /` serves the console shell, which carries no data. `POST
/admin/login` starts a session, `POST /admin/logout` ends the one it is handed. Every other
route returns 401 without the session cookie, and there is a test that walks the whole
route table to prove it. The listener is still loopback-only, and with no RemoteForward
pointing at it, **a process on the remote VM cannot reach the daemon at all**. That is a
stronger position than the capability model ever was, and it needed no design to get there.

`LG_HOSTS` keeps exactly one of its two old meanings: the hosts whose masters are opened
eagerly at startup. The "reachable by `expose`" meaning is gone, and so is the marker in
the picker. Any host the console knows may be forwarded to, because getting that far
already required a login.

### What is given up

Scripted push from the remote. No process on `code` can make a port appear on the Mac
without a human at the console. If that is ever wanted again it comes back as a decision,
with its own threat model, rather than as the default this project started with.

### Operator steps outside the repo

- `~/.local/bin/expose` and `~/.config/local-gateway/host` on the remote: delete.
- `RemoteForward 9996 localhost:9996` and its two comment lines under `Host code` in
  ssh_config: remove. The 9997 and 9998 lines stay; they belong to notify-relay and ccimgd.
- The masterLog filter for "remote port forwarding failed" stays too, for the same reason:
  those two lines still replay on every `-O forward` and still collide with the
  interactive session.

## Renames (2026-09-24)

The project is now called portkeeper, and everything that carried the old name follows:

| Was | Is |
| --- | --- |
| repo and module `local-gateway` | `github.com/johnlofty/portkeeper` |
| `cmd/gatewayd`, `bin/gatewayd` | `cmd/portkeeperd`, `bin/portkeeperd` |
| launchd label `com.alvin.local-gateway` | `io.github.johnlofty.portkeeper`, plist rendered at install with `__REPO__` filled in |
| `~/.config/local-gateway/` | `~/.config/portkeeper/` (password, hosts, pins moved as-is) |
| `/tmp/local-gateway.log` | `/tmp/portkeeper.log` |
| ControlPath `~/.ssh/sockets/local-gateway-%r@%h-%p` | `~/.ssh/sockets/portkeeper-%r@%h-%p` |

The `LG_` environment prefix is unchanged. Everything above this section uses the old
names; it is a log, not a manual, and the names it records were the names at the time.

## Dropping the login (2026-09-24)

The password, the sessions, `POST /admin/login`, `POST /admin/logout` and the console's
login view are gone. They are replaced by a guard on every route that refuses what a
browser says is cross-site, and that the owner never sees.

### Why the password had nothing left to do

Every earlier section of this document names the remote as the threat, because it was:
the control channel put a port on `code`'s loopback that anything there could POST to.
With the channel retired there is no `RemoteForward` to the daemon, so nothing on the
remote can reach it. That leaves two callers on the Mac.

- **A process running as the owner.** Never defended against. The password file was mode
  0600, which is readable by exactly that process, and the same process can read the ssh
  keys the daemon's masters use; a password it can read is not a boundary against it.
  This is still not defended, and saying so is the point of this bullet.
- **A web page in the owner's browser.** This is the caller the password was quietly
  standing in for. A page on any site can make the browser send requests to
  `127.0.0.1:9996`. Until today what turned those into 401s was that the session cookie
  was `SameSite=Strict`, not that anyone knew the password. That threat was never named
  above, because the remote was the threat then.

So the question the daemon has to answer changes. It is **still not** "did this come from
loopback": source address remains no evidence of anything, as invariant 3 said. It is
now **"did this come from a page this daemon served, or from no browser at all"**. A
browser answers that honestly in its own headers, and a caller with no browser is the
owner at a terminal, who was never being defended against.

### The three checks

`originGuard` wraps the whole mux, in this order:

1. **Host.** `r.Host` must be `cfg.Listen` exactly, `localhost:<port>` or `[::1]:<port>`.
   Anything else is a 400, and the value is logged once per distinct name (capped, so a
   page minting names cannot grow the log). A hit is either misconfiguration or DNS
   rebinding: a hostile name re-pointed at 127.0.0.1, which the browser then treats as
   same-origin with the hostile page, so no other check here would catch it.
2. **Fetch Metadata and Origin.** If `Sec-Fetch-Site` is present it must be `same-origin`
   or `none`, else 403. If `Origin` is present it must equal `http://` + the Host that
   arrived, else 403; `Origin: null` is refused by the same comparison. `same-site` is
   refused deliberately, and it is the case that matters most here: sites ignore ports, so
   a dev server on `localhost:3000`, or any page this daemon has local-forwarded onto
   `127.0.0.1:<port>`, is same-site with the console. Those are pages from the remote,
   rendered in the owner's browser, and they are exactly who this guard is for. **Absence
   of both headers is allowed**: the rule is "reject when a browser says it is
   cross-site", not "require a browser", so curl keeps working.
3. **Content-Type.** A POST, PATCH or DELETE with a body must be `application/json`,
   optionally with a charset and nothing else, or it is a 415. That type is not
   CORS-safelisted, so a cross-origin page cannot send it without a preflight, and the
   daemon sends no CORS headers, so the preflight fails. This is the backstop for a
   browser too old to send Fetch Metadata on a plain form POST.

Cross-site GETs are refused too, not only writes. The responses are unreadable
cross-origin anyway, but `GET /admin/hosts/{alias}/listeners` dials a host and runs
programs on it, and "GET is harmless" is not a property this API has.

### `GET /` gets the Host check only

Clicking a link to the console from another site is a top-level navigation with
`Sec-Fetch-Site: cross-site`. The shell carries no data (every fetch it then makes is
same-origin and goes through the full guard), and refusing it would only make the console
look broken. It still gets the Host check, because a rebound name serving the shell is the
first step of a rebinding attack.

### The self-forward guard

A remote-forward whose local port is the daemon's own listen port would publish the
console on the remote, where nothing gates it any more — the one thing retiring the
control channel was meant to make impossible, reintroduced through the front door. It is
refused in `validate()` (so on create, on every member of a range, and on a pin loaded from
disk) and in `Edit()` when a local port is changed to it. A local-forward asking for that
port by name is refused as well: the daemon already holds it, and the mirror-then-fallback
allocator would otherwise quietly hand out some other port instead of saying so. A
local-forward that merely mirrors a remote 9996, with no local port asked for, takes the
ordinary fallback. The port is parsed from `cfg.Listen` once, in `newManager`, and the
refusal is `errSelfForward`, a 400.

### What is kept, and what is left

- **The `/admin` prefix stays.** It no longer means "requires the admin session"; it is
  just where the API lives. Renaming it means the console JS and every test for no change
  in behavior, so it is a follow-up rather than part of this.
- **The listener is still loopback-only**, and `requireLoopback` still refuses anything
  else at startup. The guard defends against browsers; it would defend against nothing if
  the socket were on a LAN.
- **`~/.config/portkeeper/admin-password`** (and `~/.config/local-gateway/admin-password`
  from before the rename, if it survived) is now unread. It was left in place; the owner
  may delete it. `LG_ADMIN_PASSWORD` and `LG_ADMIN_PASSWORD_FILE` are ignored.
- `/admin/status` no longer carries `admin_enabled`.

## A menu-bar app (2026-09-24)

Portkeeper.app, under `macos/`, is a second client of the daemon. It is Swift because the
popover on the design boards (per-host sections, a drawn cable per mapping, port chips) is
more than a tray library can draw: those libraries give you a system menu of one-line
items and nothing else. It is a **thin** client because the daemon is the tested, hard
part, and nothing about ssh, backoff, pins or discovery is worth writing twice.

### The API is the line

The app reads `GET /admin/forwards` and `GET /admin/status`, polled every two seconds,
against the literal `http://127.0.0.1:9996`. That string is not a preference. The origin
guard refuses any Host that is not the daemon's own address, so a friendlier name would
be a 400. URLSession sends no `Sec-Fetch-Site` and no `Origin`, so to the guard the app is
curl, which is what it is: a process running as the owner, never defended against.

`/admin/status` now carries `"api_version": 1`. The console cannot drift from the daemon,
because it is compiled into it; the app is built separately and can. The rule is to bump
the number when a field a client reads changes meaning or goes away, not when one is
added. The app decodes it as optional, so a daemon from before the field still shows, and
says so in Settings.

The Codable models mirror `forwardView` and `hostHealthView` field for field in the
daemon's snake_case. `hosts`, `eager_hosts` and the forwards list are decoded as
optional, because Go encodes a nil map or slice as `null` and an idle daemon must not read
as a broken one. One test decodes a literal of each shape.

The app writes nothing. **Add mapping** opens the console window on `/#add`, and the
console's own add sheet does the work: ranges, target hosts and pins are validated there
already, and a native form would be a second copy of that validation, drifting. Two
consequences worth knowing. From a loaded `/` to `/#add` is a fragment navigation, so the
console has to react to `hashchange`, not only read the hash at load. And asking for the
URL already showing reloads the page, so a second **Add mapping** still opens the sheet.

### Two launchd jobs, one port

The bundle carries its own job, `io.github.johnlofty.portkeeper.helper`, in
`Contents/Library/LaunchAgents`, registered through `SMAppService.agent(plistName:)`. It is
the dev plist with three differences:

- `BundleProgram` = `Contents/MacOS/portkeeperd` instead of an absolute `ProgramArguments`
  path. SMAppService resolves it against the bundle, which is the point of bundling.
- `KeepAlive` is `{SuccessfulExit: false}` instead of `true`: restart on a crash, not on a
  clean exit.
- Its own label. The app's bundle id, `io.github.johnlofty.portkeeper.app`, is distinct
  from both labels, because an app and a job are different things to macOS and sharing a
  name only confuses the Login Items list.

The dev agent from `make install` and the helper are both portkeeperd on 127.0.0.1:9996.
The listen port is the singleton lock, bound first thing in `main.go`:
the second one to start gets `listen: address already in use`, exits 1 before it touches
any ssh state, and, since that is not a successful exit, launchd restarts it into the same
failure about every ten seconds. Harmless, and noisy. `make app-install` therefore
unloads and removes the dev agent before copying the bundle, and prints that the helper
must then be registered from Settings; nothing registers it automatically. The app works
equally well against either daemon, since all it needs is the port.

A registration records where the bundle is. Register from `build/` and then rebuild or
move it, and launchd is left pointing at a copy that changed or is gone. So the app is
meant to be run from where it will stay, `~/Applications/Portkeeper.app`, and `make app`
never registers anything.

A registered helper can sit in `requiresApproval` until the owner approves it once in
System Settings > Login Items, and in that state launchd silently never starts it.
Settings shows the state and a button to that pane, because otherwise the failure is a
daemon that simply is not there.

### What is not verified

The bundle builds, lints, signs ad hoc and passes `codesign --verify --deep --strict`.
It has not been launched. In particular these are written to what the documentation
says and not yet observed: that a `Window` scene declared after `MenuBarExtra` does not
open at launch; that `NSApp.activate` before `openWindow` and `openSettings` brings those
windows in front for an `LSUIElement` app; the approval flow; and the notifications on
reconnect and on a mapping going away.

One toolchain scar from building it: under the Command Line Tools, the macOS 26 SDK's
SwiftUI expands `@State` through a macro plugin the CLT does not ship, so any `@State`
fails to compile there while building fine under Xcode. The app uses no `@State` (its
Settings state is an `ObservableObject`), so a plain `swift build` works under either.

## Routes move to `/api` (2026-09-24)

Every route lived under `/admin` because there used to be a public tier beside it. With
one authority level the prefix described nothing, so the routes are now `/api/forwards`,
`/api/forward`, `/api/forward/{ref}`, `/api/hosts`, `/api/hosts/{alias}`,
`/api/hosts/{alias}/listeners` and `/api/status`. The console, the Swift client and the
README follow.

The first three paths are byte-for-byte the retired public API's. That is fine, and worth
saying plainly: what the retirement changed was not the paths but the reach. No port is
forwarded to this listener from the remote, and the origin guard decides who gets an
answer. A path never was the security boundary, which is what "Authorization by
capability, not by port" argued from the other side. The route-table test now asserts that
`/admin/*` and the bare pre-`/api` routes are gone, and that `/api/*` is guarded like
everything else.

## Hosts are ssh_config stanzas, not names (2026-09-24)

Add host is broken by construction. The form takes an alias, and `hostBook.manual` is a
flat list of aliases, so there are only two outcomes. If ssh already knows the alias it is
already listed from `~/.ssh/config`, and Add refuses it as `that host is already known`.
If ssh does not know it, Add saves it, the sidebar shows it, and the first Discover or
mapping fails, because `ssh -G box2` resolves to `hostname box2`, `port 22` and the local
user, which is nowhere. A host has to be what ssh itself calls a host: a `Host` block with
HostName, User, Port, IdentityFile and the rest.

### Where the stanzas live

**`~/.ssh/config` is read, never written.** Discovery parses it, and the wrapper below
includes it so that ssh applies it, but nothing in portkeeper opens it for writing, and
the console never asks the user to edit it. It is a hand-kept file with Includes, Match
blocks and comments that a rewrite could damage, and a broken ssh config breaks every
terminal on the Mac, not just this app. Everything portkeeper knows about a host it added
lives under `~/.config/portkeeper/`, in two files the app owns:

- `~/.config/portkeeper/hosts.conf` holds only `Host` blocks, one for each host added in
  the console. The daemon rewrites the whole file on every change (temp file + rename,
  0600), and nothing else writes to it. These hosts exist for portkeeper's ssh
  invocations only. `ssh box2` in a terminal does not see them, and that is the intended
  cost of never touching the user's config.
- `~/.config/portkeeper/ssh_config` is a fixed wrapper, written at startup, which every
  daemon ssh invocation gets through `-F`:

  ```
  # generated by portkeeperd — edits are overwritten
  Include /Users/<you>/.config/portkeeper/hosts.conf
  Match all
  # cfg.SSHConfigPath, so LG_SSH_CONFIG still applies
  Include /Users/<you>/.ssh/config
  # -F drops the system file; bring it back
  Include /etc/ssh/ssh_config
  ```

  Every path is written absolute (through `expandHome`) and every comment gets its own
  line, because ssh_config has no trailing comments: `Include a # note` is three globs.

  The order matters. ssh keeps the first value it sees for most settings, so a portkeeper
  stanza's `User` wins over a `Host *` default in the user's config (`IdentityFile` is
  the exception: ssh collects every one and offers them in order, so the stanza's key is
  tried first and a `Host *` key after it), while anything the stanza leaves out
  (`ServerAliveInterval`, `UseKeychain`, `AddKeysToAgent`) still comes from there.
  `Match all` has to come before the Includes, because an `Include` inside a `Host` block
  applies only to that block, and here it would apply only to the last portkeeper host.

`sshArgv` becomes `-F <wrapper> -o ControlPath=...`. The ControlPath rule is unchanged,
and `-F` makes no difference to which master ssh talks to. `ssh -G` against the wrapper is
also how the daemon checks an effective config: the round-trip test asserts that
`ssh -F wrapper -G <alias>` reports the fields that were saved.

This was chosen over passing `-o HostName= -o User= ...` on argv from the old alias list.
That would put every new field into argv, and each one would need its own
`safeAlias`-style rule. It would also leave the user's terminal `ssh` unable to reach a
host the console knows, and give ssh two sources of truth for one name.

### What a host carries

| Field | ssh keyword | Required | Validation |
| ----- | ----------- | -------- | ---------- |
| alias | `Host` | yes | `safeAlias`, unchanged |
| host name | `HostName` | yes | DNS name or IPv4/IPv6 literal; no leading `-`, no whitespace |
| user | `User` | no | `[A-Za-z0-9_][A-Za-z0-9_.-]*` |
| port | `Port` | no (22) | 1–65535 |
| identity file | `IdentityFile` | no | `~/`-relative or absolute path to an existing, readable file; saved quoted |
| identities only | `IdentitiesOnly` | no | bool; defaults to yes when an identity file is set |
| jump host | `ProxyJump` | no | comma list of `[user@]alias-or-host[:port]`, each part checked as above |
| host key policy | `StrictHostKeyChecking` | fixed | always `accept-new` (see below) |
| known hosts | `UserKnownHostsFile` | fixed | always `<app>/known_hosts ~/.ssh/known_hosts` (see below) |

The keyword set is an allowlist, and the console cannot extend it. `ProxyCommand`,
`LocalCommand`, `PermitLocalCommand`, `KnownHostsCommand`, `Match` and `Include` can run
programs or change what the file means, and a free-form "extra options" box would let one
request write any of them. The origin guard is what stops a hostile page from reaching
`POST /api/hosts`, and a stanza is not worth making that guard the only thing between a
browser tab and a shell. Anyone who needs those keywords writes them in `~/.ssh/config`,
where the host shows up as `ssh-config`.

As with aliases, values are **validated and never escaped**. Any control character,
newline or `"` is rejected outright, because a newline in a value would start a new
directive. The file is produced by one serializer from a struct and never from a
template. A parse → serialize → parse test pins that down, plus one test that feeds each
field a newline and expects a 400.

### The first connection

A host added by hand is one this Mac has probably never connected to. The master runs with
no tty, so `StrictHostKeyChecking ask` has nothing to ask on and fails as
`Host key verification failed`. Manual stanzas therefore carry `accept-new`: the first key
is trusted and recorded, and a changed key is still refused. This is trust on first use,
the same as a person typing `yes` without reading the fingerprint. The key is recorded in
portkeeper's own `~/.config/portkeeper/known_hosts`. ssh writes new keys to the first file
listed in `UserKnownHostsFile` and checks against all of them, so a host already in
`~/.ssh/known_hosts` is still verified against what is recorded there, while nothing new
is written under `~/.ssh/`. The console shows the
recorded fingerprint (`ssh-keygen -F <hostname> -l`) on the host header after the first
connect, so anyone who wants to check it can.

A passphrase-protected key cannot be unlocked without a tty either. It has to be in the
agent or the Keychain already (`UseKeychain yes` / `AddKeysToAgent yes` in `Host *`, which
the wrapper includes). When the master fails with `Permission denied (publickey)` and an
identity file is set, the error says this instead of passing on ssh's text alone.

### API

- `GET /api/hosts`: each entry gains `port`, `identity_file`, `proxy_jump` and
  `identities_only`. `parseSSHConfig` fills these for `ssh-config` hosts too, for display
  only. Adding fields does not bump `api_version`.
- `POST /api/hosts` takes `{alias, hostname, user?, port?, identity_file?, proxy_jump?,
  identities_only?}`. It returns 400 with the failing field named
  (`{"error": "...", "field": "hostname"}`) so the console can mark the right input.
- `PUT /api/hosts/{alias}` (new) replaces a manual stanza. The body has the same shape,
  and the alias cannot change. Editing an `ssh-config` or `LG_HOSTS` host is refused just
  as Remove is (`errNotManual`). A running master for that alias is taken down with
  `-O exit`, and its mappings come back through the ordinary reconnect path, so the new
  settings take effect without the daemon keeping a second copy of them.
- `DELETE /api/hosts/{alias}`: unchanged. It also exits the master, which it should have
  done already.
- `POST /api/hosts/{alias}/test` (new) runs
  `ssh -F wrapper -o BatchMode=yes -o ConnectTimeout=8 <alias> true` against a saved host
  and returns `{ok, error?, fingerprint?}`. It also passes `-o ControlMaster=no
  -o ControlPath=none`. With `-F wrapper`, the user's `Host *` may carry `ControlMaster auto`,
  and the test must neither ride on their master nor on ours. It proves the stanza works,
  not that a master happens to be up.

### Precedence, and the old hosts file

An alias defined both in `hosts.conf` and in `~/.ssh/config` is ambiguous. Add refuses it,
and that stays. If the user later adds the same alias to their own config, the wrapper's
order means portkeeper's stanza wins inside the daemon while the terminal uses theirs.
`List()` reports that case as `source: "manual"` with `shadowed: true`, and the console
shows it on the host so it is not a silent disagreement.

The old `~/.config/portkeeper/hosts` (bare aliases) is read once at startup. A bare alias
that ssh can already resolve through the user's config is dropped, since the listing
already has it. Any other is kept as an **incomplete** manual host: listed with an amber
"needs a host name" line and refused as a target until it is edited. The first save
writes `hosts.conf` and replaces the old file.

### Console

The Add host form in the sidebar is too small for seven fields and grows into the same
dialog the mapping form uses:

- Alias and Host name first, both required. Typing `user@host:port` into Host name splits
  it into the three fields, because that is what most people will paste.
- User, Port and Identity file follow. The identity file is a text input with suggestions
  from `~/.ssh/id_*` (listed by a new `GET /api/identities`: filenames only, never
  contents). A browser file picker cannot supply a real path.
- Jump host sits under "More".
- A live, read-only preview of the stanza being written, in mono. It is the most honest
  description of what Save will do.
- Save saves and then runs `test`. A test failure keeps the dialog open with ssh's
  message and a "Save anyway" choice, because a box that is down right now can still be
  added.

For an `ssh-config` host, the host header shows its fields read-only with the note
"Defined in ~/.ssh/config", and only manual hosts get Edit and Remove.

### Not done, on purpose

- Writing to `~/.ssh/config`, or suggesting an edit to it, in any form. It is an input
  only.
- Importing a host from `~/.ssh/config` into portkeeper. It is already listed, and
  keeping two copies would only let them drift.
- Password authentication. The master has no tty, so key or agent auth is the only kind
  that can work unattended, and the form does not pretend otherwise.

### Verification plan

1. Unit: serializer round trip; one rejection per field for newline, `"`, leading `-` and
   an out-of-range port; wrapper order (`ssh -F wrapper -G` with a `Host *` `User`
   default in a fake user config must report the stanza's User).
2. Unit: add, edit and remove against a temp `LG_SSH_CONFIG`, then assert that file's
   bytes and mtime are unchanged. This is the test that holds the read-only rule.
3. Unit: migration of an old hosts file that mixes resolvable and unresolvable aliases.
4. Route table: `PUT /api/hosts/{alias}` and `POST /api/hosts/{alias}/test` are behind
   the origin guard like everything else, and `PUT` on an `ssh-config` host is 400.
5. By hand: add `pi` a second time as `pi2` with HostName `192.168.31.250`, User `pi` and
   an identity file. Test passes, Discover lists its listeners, a mapping opens,
   `~/.config/portkeeper/known_hosts` gains exactly one line, and nothing under `~/.ssh/`
   changes. Then change the port to a closed one: the
   master exits, the mapping shows reconnecting, and the host health shows ssh's refusal.

### As built (2026-09-24)

Implemented as designed, with these differences found on the way:

- **Remove refuses a host that is in use.** `DELETE /api/hosts/{alias}` answers 409 while
  any mapping or pin names the host. Stopping its master and dropping it from the book
  would have left a pin retrying against an unknown host on every tick, forever.
- **The old alias file sits next to `HostsFile`**, so pointing `LG_HOSTS_FILE` elsewhere
  (a test, a second daemon) never migrates the real one. It is kept, holding only the
  aliases that still need a host name, and removed once none do.
- **The fingerprint is shown once**, in the message after a successful Save and test,
  rather than kept on the host page. Nothing stores it, and `ssh-keygen -F` can answer
  again at any time.
- **`argvTest` is built by hand.** ssh keeps the first `-o` it sees, so a
  `ControlPath=none` after `sshArgv`'s own ControlPath would have been ignored, and the
  test would have ridden on the daemon's master. `TestConnectionTestUsesNoMaster` pins this.
- **Values passed to `-o` are not quoted.** A ControlPath with a space in it is split by
  ssh, which is why the real-ssh test keeps its sockets outside the spaced directory.
  The default path has no spaces, so nothing changes in practice.

Verified end to end against a second daemon on port 9990 with every portkeeper path in a
temp directory. It added a copy of `code` under a new alias, and the connection test
returned `ok` and the host key fingerprint. Discover listed its listeners, a local-forward
of :3000 answered on the Mac, Remove was refused with the mapping open, and an edit
restarted the master with the mapping coming back `alive`. `~/.ssh/config` and
`~/.ssh/known_hosts` had the same SHA-1 before and after. Not verified: a first connection
to a host whose key is in neither known_hosts file, which is the case that writes to
portkeeper's own file; the Pi used for it was unreachable at the time.
