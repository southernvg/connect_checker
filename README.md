# connect_checker

A small heartbeat checker: machines ping a server every few minutes with their battery status, and the server shows when each one was last heard from.

It has two parts:

- **Server** (`server/main.go`) — a Go web server that stores pings in SQLite and serves a status page.
- **Client** (`client/ping.sh`) — a POSIX shell script that sends a ping every 5 minutes, forever. It reads the battery with `pmset`, so it is macOS-only.

## What the status page shows

Open the server's address in a browser to see:

- A card per client ID with how long ago its last ping arrived, plus stats: pings, longest gap and battery range over the last 24 hours, total pings, and when it was first seen.
- The 50 most recent pings, with the sender's IP address.

A card turns red when its client has been silent for longer than the stale threshold (11 minutes by default, i.e. two missed pings). The page refreshes itself every 30 seconds.

## Requirements

- Go 1.26 or newer (no cgo needed; SQLite is the pure-Go `modernc.org/sqlite` driver)
- `curl` and macOS on the machines that run the client

## Running locally

Start the server:

```sh
go run ./server
```

It listens on `127.0.0.1:8080` and creates `./pings.db`. Then, in another terminal, start the client:

```sh
./client/ping.sh
```

Open <http://127.0.0.1:8080> to see the pings arrive.

### Server options

| Flag | Default | Meaning |
| --- | --- | --- |
| `-addr` | `127.0.0.1:8080` | Address to listen on |
| `-db` | `pings.db` | Path to the SQLite database file |
| `-stale` | `11m` | How long a client can be silent before its card turns red |

```sh
go run ./server -addr 127.0.0.1:9000 -db /path/to/pings.db -stale 11m
```

If the `PING_TOKEN` environment variable is set, `/ping` requires `Authorization: Bearer <token>`. The status page is never authenticated.

### Client options

The client is configured with environment variables:

| Variable | Default | Meaning |
| --- | --- | --- |
| `CLIENT_ID` | this machine's short hostname | ID the pings are recorded under |
| `SERVER_URL` | `http://127.0.0.1:8080` | Base URL of the server |
| `INTERVAL` | `300` | Seconds between pings |
| `PING_TOKEN` | unset | Bearer token, required if the server has one |

```sh
CLIENT_ID=laptop SERVER_URL=http://127.0.0.1:9000 INTERVAL=10 ./client/ping.sh
```

A failed ping is logged to stderr and retried on the next round; the script never exits on its own.

## Deploying to a server

`deploy/deploy.sh` cross-compiles the server for Linux, uploads it over SSH, and installs it as a systemd service. It works with a DigitalOcean droplet or any systemd Linux host you can reach as root or as a user with passwordless sudo.

```sh
./deploy/deploy.sh root@<droplet-ip>
```

The first deploy generates a ping token and writes it, with the listen address, to `/etc/connect_checker.env`. When it finishes, the script prints the command to start a client against the droplet. Set `PORT` on the first deploy to listen on something other than 8080.

Re-running the script replaces the binary and restarts the service, keeping the database and token. To also delete every ping stored on the droplet (no backup is taken):

```sh
./deploy/deploy.sh --reset-db root@<droplet-ip>
```

Follow the service logs with:

```sh
ssh root@<droplet-ip> journalctl -u connect_checker -f
```

**The deployed server speaks plain HTTP — there is no TLS.** The ping token and the status page travel unencrypted, and the status page is open to anyone who can reach the port.

The footer of the status page shows the commit the binary was built from, with `-dirty` appended if there were uncommitted changes, so commit before deploying.

## The ping endpoint

Anything that can make an HTTP request can send a ping; the shell client is just one way to do it.

```sh
curl --get http://127.0.0.1:8080/ping \
  --data-urlencode "id=my-machine" \
  --data-urlencode "status=87% discharging"
```

`/ping` accepts `id` and `status` as query or form parameters, by GET or POST. `status` is free text, but start it with a percentage such as `87%` if you want it counted in the battery range.

## Development

```sh
go build -o bin/server ./server   # bin/ is git-ignored
go vet ./...
```

There are no tests yet. To check a change, run the server against a throwaway `-db` file on another port and point the client at it with `SERVER_URL` and a short `INTERVAL`.
