#!/bin/sh
# Builds the server for Linux and installs it on a droplet as a systemd service.
#
#   ./deploy/deploy.sh root@203.0.113.10
#   ./deploy/deploy.sh --reset-db root@203.0.113.10   # also delete all stored pings
#
# Safe to re-run: it replaces the binary and restarts the service, and keeps
# the existing database and ping token. --reset-db permanently deletes the
# droplet's pings.db (no backup is taken); the token is kept. Needs SSH access as root or as a user
# with passwordless sudo. Set PORT to listen on something other than 8080
# (only applied on the first deploy; edit /etc/connect_checker.env afterwards).
set -eu

reset_db=0
if [ "${1:-}" = "--reset-db" ]; then
	reset_db=1
	shift
fi
if [ $# -ne 1 ] || [ "${1#-}" != "$1" ]; then
	echo "usage: $0 [--reset-db] user@host" >&2
	exit 2
fi
target="$1"
host="${target#*@}"
PORT="${PORT:-8080}"
cd "$(dirname "$0")/.."

case "$(ssh "$target" uname -m)" in
	x86_64) goarch=amd64 ;;
	aarch64 | arm64) goarch=arm64 ;;
	*) echo "unsupported droplet architecture" >&2; exit 1 ;;
esac

echo "==> building for linux/$goarch"
mkdir -p bin
GOOS=linux GOARCH="$goarch" CGO_ENABLED=0 go build -trimpath -o "bin/connect_checker-linux-$goarch" ./server

echo "==> uploading to $target"
scp -q "bin/connect_checker-linux-$goarch" "$target:/tmp/connect_checker.new"
scp -q deploy/connect_checker.service "$target:/tmp/connect_checker.service.new"

echo "==> installing"
ssh "$target" "PORT='$PORT' RESET_DB='$reset_db' sh -s" <<'REMOTE'
set -eu
if [ "$(id -u)" -eq 0 ]; then sudo=""; else sudo="sudo"; fi

$sudo install -m 0755 /tmp/connect_checker.new /usr/local/bin/connect_checker
$sudo install -m 0644 /tmp/connect_checker.service.new /etc/systemd/system/connect_checker.service
rm -f /tmp/connect_checker.new /tmp/connect_checker.service.new

# First deploy only: choose the listen address and generate the ping token.
if ! $sudo test -f /etc/connect_checker.env; then
	token="$(head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n')"
	printf 'ADDR=0.0.0.0:%s\nPING_TOKEN=%s\n' "$PORT" "$token" |
		$sudo install -m 0600 /dev/stdin /etc/connect_checker.env
fi

$sudo systemctl daemon-reload
$sudo systemctl enable --quiet connect_checker
if [ "$RESET_DB" = 1 ]; then
	# Stop first so the server is not holding the files, then remove the
	# database and its SQLite side files. The server recreates it on start.
	$sudo systemctl stop connect_checker
	$sudo sh -c 'rm -f /var/lib/connect_checker/pings.db*'
	echo "    database reset: all stored pings deleted"
fi
$sudo systemctl restart connect_checker

# Open the port if the droplet's own firewall is on.
port="$($sudo sed -n 's/^ADDR=.*:\([0-9]*\)$/\1/p' /etc/connect_checker.env)"
if command -v ufw >/dev/null 2>&1 && $sudo ufw status | grep -q '^Status: active'; then
	$sudo ufw allow "$port/tcp" >/dev/null
fi

sleep 1
if ! $sudo systemctl is-active --quiet connect_checker; then
	echo "service failed to start:" >&2
	$sudo journalctl -u connect_checker -n 20 --no-pager >&2
	exit 1
fi
REMOTE

port="$(ssh "$target" 'if [ "$(id -u)" -eq 0 ]; then s=""; else s=sudo; fi; $s sed -n "s/^ADDR=.*:\([0-9]*\)$/\1/p" /etc/connect_checker.env')"
echo "==> running at http://$host:$port/"
echo
echo "Start a client with (the token is stored in /etc/connect_checker.env on the droplet):"
echo
echo "  PING_TOKEN=\$(ssh $target 'sudo sed -n s/^PING_TOKEN=//p /etc/connect_checker.env') \\"
echo "    SERVER_URL=http://$host:$port ./client/ping.sh"
