# local-gateway

Port mappings between your Mac and a remote dev box, without hand-rolling `ssh -L`.

A daemon on the Mac owns one SSH ControlMaster and adds or drops forwards on it on
demand. You drive it from a web console on the Mac; hooks and scripts drive it from
the remote with `expose`.

Two directions, named after the ssh flags they become:

| Direction        | ssh  | What it gets you                                                     |
| ---------------- | ---- | -------------------------------------------------------------------- |
| `local-forward`  | `-L` | A dev server on `code` opens in your Mac's browser                    |
| `remote-forward` | `-R` | A service on your Mac (notifications, an API) is callable from `code` |

`DESIGN.md` covers why it works this way.

## Two levels

**The console** — <http://127.0.0.1:9996/> on the Mac, behind a password login. Full
control: create, edit and delete mappings in either direction, plus configuration.

**`expose`** — the public API, callable by anything on the remote box, no
authentication. It can only create a local-forward, list, and close.

The split is the security model. The control channel is reachable by every process on
the remote VM, so it is kept safe by offering a deliberately small set of operations
rather than by a shared secret — a secret copied onto that box would be readable by
those same processes anyway. Publishing a Mac service to the remote, or popping a
browser window on your screen, are the operations worth protecting, so they live
behind the login.

## Install

On the Mac:

```sh
make install          # builds bin/gatewayd, loads the launchd agent
make deploy-client    # scp bin/expose to code:~/.local/bin/
```

The control channel needs one line in `~/.ssh/config` under `Host code`:

```
RemoteForward 9996 localhost:9996
```

## From the remote

```sh
expose 8530                             # forward it, print the Mac URL
expose 8530 --label mkdp --ttl 3600
expose --list
expose --close code:local-forward:8530  # or just: expose --close 8530
expose --wait 8530                      # hold it; Ctrl-C drops it
```

The URL is the only thing on stdout, so `url="$(expose 8530)"` works in a script.

`--list` shows everything the daemon holds, including remote-forwards made from the
console, so the FLOW column spells out which way each mapping points.

## Checking it works

```sh
make status                                  # is the agent loaded
make logs                                    # tail /tmp/local-gateway.log
curl -s 127.0.0.1:9996/api/forwards | jq     # on the Mac
expose --list                                # from the remote, proves the tunnel too
```

`expose` failing with "cannot reach the local-gateway daemon" means one of two things:
no SSH session from the Mac is currently up (the `RemoteForward` only exists while one
is), or the daemon isn't running. It tells you which to check.

## Hosts

The console picks a host from a list rather than taking a typed name. It merges three
sources: `LG_HOSTS`, the `Host` entries in `~/.ssh/config`, and hosts added in the console
(persisted to `~/.config/local-gateway/hosts`).

Discovery does not grant access. Two tiers, because `/api` has no authentication:

| Caller | May forward to |
| ------ | -------------- |
| the console (password) | anything in the list |
| `expose` on a remote box | `LG_HOSTS` only |

That split is deliberate. Your ssh config probably names your router and a few boxes you
would not want a compromised dependency on a dev VM to request a tunnel to. The console
marks any host it can reach that `expose` cannot, so the difference is visible in the picker
instead of surfacing as a 403.

`expose` does not guess which host it is on: a box cannot know what this Mac's ssh config
calls it. It sends nothing, and the daemon fills in the single public host — or says so
plainly when there are several. `make deploy-client` writes the alias to
`~/.config/local-gateway/host` on the remote so it is explicit once you have more than one.
