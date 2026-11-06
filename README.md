# knot

Browse and move files across every device on your tailnet from one place.

One Go binary with two frontends:

- `knot` — a two-pane terminal browser
- `knot serve` — the same browser as a web UI

No agent to install on the other machines. knot reuses the SSH you already have,
so a device only needs `sshd` running (Termux included).

## Build

```sh
go build -o ~/.local/bin/knot .
```

## Status

Work in progress: node abstraction, tailnet discovery and transfer logic are in
place. The terminal and web frontends are being built on top.
