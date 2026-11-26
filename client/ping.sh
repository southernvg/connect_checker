#!/bin/sh
# Pings the connect_checker server with this machine's battery status, forever.
# Override the defaults with environment variables: CLIENT_ID, SERVER_URL,
# INTERVAL (seconds between pings), PING_TOKEN (required if the server has one).
set -u

# The ID defaults to this machine's short hostname (e.g. "chris-mbp").
CLIENT_ID="${CLIENT_ID:-$(hostname -s 2>/dev/null || hostname)}"
SERVER_URL="${SERVER_URL:-http://127.0.0.1:8080}"
INTERVAL="${INTERVAL:-300}"
PING_TOKEN="${PING_TOKEN:-}"

battery_status() {
	# macOS: `pmset -g batt` prints a line such as
	#   -InternalBattery-0 (id=1234)	87%; discharging; 4:12 remaining present: true
	batt="$(pmset -g batt 2>/dev/null)"
	percent="$(printf '%s\n' "$batt" | grep -Eo '[0-9]+%' | head -n 1)"
	state="$(printf '%s\n' "$batt" | grep -Eo '(discharging|charging|charged|finishing charge|AC attached)' | head -n 1)"
	if [ -n "$percent" ]; then
		printf '%s\n' "$percent${state:+ $state}"
	else
		printf 'no battery\n'
	fi
}

echo "pinging $SERVER_URL as $CLIENT_ID every ${INTERVAL}s (Ctrl-C to stop)"

while :; do
	status="$(battery_status)"
	now="$(date '+%Y-%m-%d %H:%M:%S')"
	# A failed ping is logged and retried on the next round, not fatal.
	if curl --fail --silent --show-error --max-time 5 --output /dev/null \
		${PING_TOKEN:+--header "Authorization: Bearer $PING_TOKEN"} \
		--get \
		--data-urlencode "id=$CLIENT_ID" \
		--data-urlencode "status=$status" \
		"$SERVER_URL/ping"; then
		echo "$now sent: $status"
	else
		echo "$now ping failed" >&2
	fi
	sleep "$INTERVAL"
done
