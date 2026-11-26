# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

A local-only heartbeat checker with two parts:

- `server/main.go` — a Go web server that stores pings in SQLite and shows the latest ones.
- `client/ping.sh` — a POSIX shell script that loops forever, sending a ping every 5 minutes with this machine's battery status.

## Commands

```sh
go run ./server                      # start on 127.0.0.1:8080, creates ./pings.db
go run ./server -addr 127.0.0.1:9000 -db /path/to/pings.db -stale 11m
go build -o bin/server ./server      # bin/ is git-ignored
go vet ./...

./client/ping.sh                     # ping every 5 minutes until stopped
CLIENT_ID=laptop SERVER_URL=http://127.0.0.1:9000 INTERVAL=10 ./client/ping.sh
```

Deploy to a DigitalOcean droplet (or any systemd Linux host reachable over SSH):

```sh
./deploy/deploy.sh root@<droplet-ip>   # cross-compiles, uploads, installs and restarts the service
./deploy/deploy.sh --reset-db root@<droplet-ip>   # same, and deletes all pings stored on the droplet
ssh root@<droplet-ip> journalctl -u connect_checker -f
```

There are no tests yet. To check a change, run the server against a throwaway `-db` file on another port and point the client at it with `SERVER_URL` and a short `INTERVAL`.

## Design notes

- **Endpoints:** `/ping` takes `id` and `status` (query or form, GET or POST) and inserts a row along with the sender's IP (from `RemoteAddr`; forwarding headers are deliberately ignored); `/` renders, with `html/template`, a card per ID showing how long ago its last ping arrived plus stats (pings, longest gap and battery range over `statsWindow` = 24h; total pings; first seen), then the 50 newest pings (`recentLimit`).
- **SQLite driver:** `modernc.org/sqlite` (pure Go, driver name `sqlite`), chosen so the build needs no cgo. The schema is created on startup, and `migrate` adds columns missing from older database files (currently just `ip`) — extend it rather than editing `schema` alone when adding a column.
- **Timestamps:** stored as UTC unix seconds, set by the server, and converted to local time only when rendered.
- **Loopback by default, token when exposed:** locally the server binds `127.0.0.1` with no auth. If the `PING_TOKEN` environment variable is set, `/ping` requires `Authorization: Bearer <token>`; the client sends it when its own `PING_TOKEN` is set. The index page is never authenticated.
- **Deployment:** `deploy/deploy.sh` installs the binary to `/usr/local/bin/connect_checker` and `deploy/connect_checker.service` as a systemd unit (`DynamicUser`, database in `/var/lib/connect_checker/`). `/etc/connect_checker.env` holds `ADDR` and a `PING_TOKEN` generated on the first deploy and left alone on later ones. It serves plain HTTP on port 8080 — there is no TLS.
- **Client is macOS-specific:** battery is parsed from `pmset -g batt`; a machine with no battery reports `no battery`. A failed ping is logged to stderr and retried on the next round; the loop never exits on its own.
- **Stale IDs:** a card turns red when its ID's last ping is older than `-stale` (default 11m, i.e. two missed 5-minute pings). "Ago" text is computed server-side at render time; the page meta-refreshes every 30s rather than using JavaScript.
- **Status is free text, with one convention:** the server stores `status` verbatim, but the battery stat parses a leading `NN%` (`batteryRE`). Keep the client's status starting with the percentage, or update both sides together.
- **Index page layout:** all HTML and CSS live in `indexTmpl` in `server/main.go`. Below 640px the ping table is restyled by CSS into stacked rows (the header row, `tr.head`, is hidden), so a new column needs a `grid-area` in that media query as well as a `<td>`.
- **Build hash in the footer:** `version` reads the commit from the VCS info the Go toolchain embeds (`debug.ReadBuildInfo`), with no `-ldflags`. It shows the short hash, `-dirty` when built with uncommitted changes, and `dev` under `go run` (which embeds no VCS info) — so commit before deploying to get a clean hash.
