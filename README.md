# knot

Browse and move files across every device on your tailnet from one place.

One Go binary with two frontends:

- `knot`: a two-pane terminal browser (source on the left, destination on the right)
- `knot serve`: the same browser as a web UI

No agent to install on the other machines. knot reuses the SSH you already have,
so a device only needs `sshd` running (Termux included).

## Build

```sh
go build -o ~/.local/bin/knot .
```

## Config

Created on first run at `~/.config/knot/config.json`:

```json
{
  "nodes": [
    { "name": "archbox",  "kind": "local" },
    { "name": "thinkpad", "kind": "ssh", "user": "himanshu", "port": 22 },
    { "name": "vps",      "kind": "ssh", "user": "himanshu", "port": 22 },
    { "name": "phone",    "kind": "ssh", "user": "u0_a430", "port": 8022,
      "home": "/data/data/com.termux/files/home" }
  ],
  "web": { "listen": ":8787" }
}
```

`addr` is optional: when omitted, knot resolves the node name against
`tailscale status --json`, so devices can change IPs without touching this file.
Resolution prefers the MagicDNS label, because a cloned machine often reports
the wrong `HostName`.

A bare `":8787"` means **tailnet only**: knot binds to this machine's tailnet
IPv4. Use `--listen 0.0.0.0:8787` if you really want it on the LAN.

## Usage

```sh
knot                      # terminal browser
knot serve                # web UI, prints a URL with an access token
knot nodes                # list devices and tailnet status
knot cp ./notes.md phone:/sdcard/notes.md
knot mv vps:/tmp/a.log thinkpad:/tmp/
```

### Web UI

The web UI is a tiling window manager over the same nodes. Each window is one
device at one path.

- **New window** splits the focused window along its longer axis (dwindle/BSP,
  the same scheme bspwm uses).
- **Drag a split divider** to change the ratio between two windows.
- **Drag a window's title bar** onto another window to re-tile: drop on an outer
  band to split that window on that side, or in the middle to swap the two.
- **Drag file rows** between windows to copy them; hold **shift** to move.
- The layout and each window's device/path persist in `localStorage`; **Reset**
  clears it.

Because the browser sandbox cannot hand files back to the OS, dragging out of a
window is not possible, so use the download link for that.

### TUI keys

| key | action |
| --- | --- |
| `tab` | switch pane |
| `n` / `N` | next / previous device in this pane |
| `enter` / `h` | open directory / go up |
| `space` | select (and advance) |
| `c` / `m` | copy / move selection to the other pane |
| `d` | delete |
| `R` | rename |
| `a` | new folder |
| `r` | refresh |
| `q` | quit |

Copy and move stream through the machine running knot. That costs bandwidth on
the host, but it means no device needs inbound access from any other device and
the phone works over a DERP relay with no extra setup.

### Auth

knot tries `ssh-agent`, then `~/.ssh/id_{ed25519,ecdsa,rsa}`, then a password.
Passwords are kept in memory only and never written to disk.

The TUI prompts for a password on the terminal before taking over the screen.
The web UI asks in a dialog when a device rejects the connection.

To avoid the prompt entirely:

```sh
ssh-copy-id -p 8022 u0_a430@phone
```

## Layout

```
main.go                 subcommands, preflight, cp/mv
internal/node           Node interface, local + SSH/SFTP implementations
internal/ops            recursive copy/move with progress
internal/cluster        config + tailnet -> ready nodes, password store
internal/tailnet        `tailscale status --json` parsing
internal/tui            bubbletea two-pane browser
internal/web            HTTP API + embedded UI
```

`node.Node` is the seam: `List`, `Open`, `Create`, `Mkdir`, `Remove`, `Rename`
over a POSIX path. A local disk and a phone across the tailnet look identical to
everything above it.

## Web API

All endpoints require the token, as a `knot_token` cookie, `X-Knot-Token`
header, or `?t=` query parameter.

| method | path | purpose |
| --- | --- | --- |
| POST | `/api/login` | exchange the token for a cookie |
| GET | `/api/nodes` | devices with tailnet status |
| GET | `/api/list?node=&path=` | directory listing |
| GET | `/api/download?node=&path=` | download a file |
| POST | `/api/upload` | multipart upload |
| POST | `/api/mkdir` | create a directory |
| POST | `/api/rename` | rename within a directory |
| POST | `/api/delete` | delete names within a directory |
| POST | `/api/auth` | set a device password (verified first) |
| POST | `/api/transfer` | start a copy/move job, returns an id |
| GET | `/api/jobs/{id}` | poll transfer progress |

Transfers run as background jobs so a multi-gigabyte copy does not block a
request, and the UI can show a progress bar.
