// Command server records pings in SQLite and shows the most recent ones.
package main

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"flag"
	"fmt"
	"html/template"
	"log"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const (
	recentLimit = 50
	maxFieldLen = 200
	statsWindow = 24 * time.Hour
)

// batteryRE picks the level out of statuses such as "87% discharging".
var batteryRE = regexp.MustCompile(`^(\d{1,3})%`)

const schema = `
CREATE TABLE IF NOT EXISTS pings (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	client_id   TEXT    NOT NULL,
	status      TEXT    NOT NULL,
	received_at INTEGER NOT NULL, -- unix seconds, UTC
	ip          TEXT    NOT NULL DEFAULT '' -- address the ping came from
);
CREATE INDEX IF NOT EXISTS pings_client_id ON pings (client_id, id);`

// ping is one row as shown on the index page.
type ping struct {
	ClientID   string
	Status     string
	IP         string
	ReceivedAt time.Time
	Ago        string
	Stale      bool // only meaningful for the per-ID summary
}

// clientStats is the summary card for one ID: its latest ping plus figures
// computed over all of its pings and over the last statsWindow.
type clientStats struct {
	ping           // the latest ping
	Total      int // pings ever received
	FirstSeen  time.Time
	Tracked    string // how long ago FirstSeen was
	Recent     int    // pings within statsWindow
	LongestGap string // longest silence between pings within statsWindow
	Battery    string // lowest–highest battery level within statsWindow
}

type indexData struct {
	Clients []clientStats // one per ID, most recently seen first
	Pings   []ping        // newest pings across all IDs
	Limit   int
	Now     time.Time
}

type server struct {
	token      string // when non-empty, /ping requires it as a bearer token
	db         *sql.DB
	staleAfter time.Duration
}

// span renders a duration compactly, e.g. "42s", "5m", "2h 13m", "3d 4h".
func span(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd %dh", int(d.Hours())/24, int(d.Hours())%24)
	}
}

// ago renders a duration as a short "how long ago" phrase.
func ago(d time.Duration) string {
	if d < 5*time.Second {
		return "just now"
	}
	return span(d) + " ago"
}

var indexTmpl = template.Must(template.New("index").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta http-equiv="refresh" content="30">
<title>connect_checker</title>
<style>
  :root {
    --bg: #f5f6f8; --card: #ffffff; --text: #1c2026; --muted: #6b7280;
    --line: #e5e7eb; --ok: #16a34a; --stale: #dc2626; --accent: #2563eb;
  }
  @media (prefers-color-scheme: dark) {
    :root {
      --bg: #0f1216; --card: #181c22; --text: #e6e8eb; --muted: #8b93a1;
      --line: #272c35; --ok: #4ade80; --stale: #f87171; --accent: #7aa7ff;
    }
  }
  * { box-sizing: border-box; }
  html { -webkit-text-size-adjust: 100%; text-size-adjust: 100%; }
  body {
    margin: 0; padding: 2rem 1rem 3rem; background: var(--bg); color: var(--text);
    font: 15px/1.5 system-ui, -apple-system, "Segoe UI", sans-serif;
  }
  main { max-width: 860px; margin: 0 auto; }
  header { display: flex; align-items: baseline; justify-content: space-between; gap: 1rem; flex-wrap: wrap; }
  h1 { font-size: 1.5rem; margin: 0; letter-spacing: -.01em; }
  h2 { font-size: .8rem; text-transform: uppercase; letter-spacing: .06em; color: var(--muted); margin: 2rem 0 .75rem; }
  .meta { color: var(--muted); font-size: .85rem; }
  .cards { display: grid; grid-template-columns: repeat(auto-fill, minmax(min(260px, 100%), 1fr)); gap: .75rem; }
  .card {
    background: var(--card); border: 1px solid var(--line); border-radius: 12px;
    padding: 1rem 1.1rem; min-width: 0;
  }
  .card .id { display: flex; align-items: center; gap: .5rem; font-weight: 600; overflow-wrap: anywhere; }
  .dot { flex: none; width: .6rem; height: .6rem; border-radius: 50%; background: var(--ok); }
  .dot.stale { background: var(--stale); }
  .card .ago { font-size: 1.5rem; font-weight: 600; margin: .35rem 0 .1rem; font-variant-numeric: tabular-nums; }
  .card .ago.stale { color: var(--stale); }
  .card .detail { color: var(--muted); font-size: .85rem; overflow-wrap: anywhere; }
  .stats {
    display: grid; grid-template-columns: 1fr auto; gap: .3rem 1rem;
    margin: .8rem 0 0; padding-top: .8rem; border-top: 1px solid var(--line); font-size: .85rem;
  }
  .stats dt { color: var(--muted); }
  .stats dd { margin: 0; text-align: right; font-weight: 600; font-variant-numeric: tabular-nums; }
  .panel { background: var(--card); border: 1px solid var(--line); border-radius: 12px; overflow-x: auto; }
  table { border-collapse: collapse; width: 100%; }
  th, td { text-align: left; padding: .6rem 1.1rem; border-bottom: 1px solid var(--line); }
  tr:last-child td { border-bottom: 0; }
  th { font-size: .75rem; text-transform: uppercase; letter-spacing: .06em; color: var(--muted); font-weight: 600; white-space: nowrap; }
  td.time { white-space: nowrap; font-variant-numeric: tabular-nums; }
  td.time span { color: var(--muted); margin-left: .5rem; font-size: .85rem; }
  td.id { font-weight: 600; }
  td.id, td.status { overflow-wrap: anywhere; }
  .ip { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; font-size: .85rem; }
  td.ip { color: var(--muted); white-space: nowrap; }
  .empty { background: var(--card); border: 1px dashed var(--line); border-radius: 12px; padding: 2.5rem 1rem; text-align: center; color: var(--muted); }
  code { background: var(--bg); padding: .1rem .35rem; border-radius: 4px; }

  /* Phones: tighter spacing, and each table row becomes a stacked block
     (ID and IP on top, then status, then time) instead of scrolling sideways. */
  @media (max-width: 640px) {
    body {
      padding: 1.25rem max(.75rem, env(safe-area-inset-right)) calc(2rem + env(safe-area-inset-bottom)) max(.75rem, env(safe-area-inset-left));
    }
    h1 { font-size: 1.3rem; }
    h2 { margin-top: 1.5rem; }
    .panel { overflow-x: visible; }
    table, tbody { display: block; }
    tr.head { display: none; }
    tr {
      display: grid; grid-template-columns: 1fr auto; grid-template-areas: "id ip" "status status" "time time";
      gap: .15rem .75rem; padding: .7rem 1rem; border-bottom: 1px solid var(--line);
    }
    tr:last-child { border-bottom: 0; }
    td { padding: 0; border: 0; min-width: 0; }
    td.id { grid-area: id; }
    td.ip { grid-area: ip; align-self: center; }
    td.status { grid-area: status; }
    td.time { grid-area: time; color: var(--muted); font-size: .85rem; white-space: normal; }
    td.time span { margin-left: .4rem; }
  }
</style>
</head>
<body>
<main>
<header>
  <h1>connect_checker</h1>
  <span class="meta">Updated {{.Now.Format "15:04:05"}} · refreshes every 30s</span>
</header>
{{if .Pings}}
<h2>Last ping by ID</h2>
<div class="cards">
  {{range .Clients}}
  <div class="card">
    <div class="id"><span class="dot{{if .Stale}} stale{{end}}" title="{{if .Stale}}No recent ping{{else}}Pinging{{end}}"></span>{{.ClientID}}</div>
    <div class="ago{{if .Stale}} stale{{end}}">{{.Ago}}</div>
    <div class="detail">{{.Status}}</div>
    <div class="detail">{{.ReceivedAt.Format "2006-01-02 15:04:05"}}{{if .IP}} · <span class="ip">{{.IP}}</span>{{end}}</div>
    <dl class="stats">
      <dt>Pings, last 24h</dt><dd>{{.Recent}}</dd>
      <dt>Longest gap, last 24h</dt><dd>{{.LongestGap}}</dd>
      <dt>Battery, last 24h</dt><dd>{{.Battery}}</dd>
      <dt>Total pings</dt><dd>{{.Total}}</dd>
      <dt>First seen</dt><dd title="{{.FirstSeen.Format "2006-01-02 15:04:05"}}">{{.Tracked}}</dd>
    </dl>
  </div>
  {{end}}
</div>

<h2>Last {{.Limit}} pings</h2>
<div class="panel">
<table>
  <tr class="head"><th>Received</th><th>ID</th><th>Status</th><th>IP address</th></tr>
  {{range .Pings}}
  <tr>
    <td class="time">{{.ReceivedAt.Format "2006-01-02 15:04:05"}}<span>{{.Ago}}</span></td>
    <td class="id">{{.ClientID}}</td>
    <td class="status">{{.Status}}</td>
    <td class="ip">{{if .IP}}{{.IP}}{{else}}—{{end}}</td>
  </tr>
  {{end}}
</table>
</div>
{{else}}
<h2>Pings</h2>
<div class="empty">No pings received yet. Run <code>./client/ping.sh</code> to send one.</div>
{{end}}
</main>
</body>
</html>
`))

func (s *server) handlePing(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.token != "" {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
			http.Error(w, "missing or wrong token", http.StatusUnauthorized)
			return
		}
	}
	id := strings.TrimSpace(r.FormValue("id"))
	status := strings.TrimSpace(r.FormValue("status"))
	if id == "" || status == "" {
		http.Error(w, "id and status are required", http.StatusBadRequest)
		return
	}
	if len(id) > maxFieldLen || len(status) > maxFieldLen {
		http.Error(w, "id or status too long", http.StatusBadRequest)
		return
	}
	ip := remoteIP(r)
	_, err := s.db.ExecContext(r.Context(),
		`INSERT INTO pings (client_id, status, received_at, ip) VALUES (?, ?, ?, ?)`,
		id, status, time.Now().Unix(), ip)
	if err != nil {
		log.Printf("insert ping: %v", err)
		http.Error(w, "could not store ping", http.StatusInternalServerError)
		return
	}
	log.Printf("ping id=%q status=%q ip=%s", id, status, ip)
	w.Write([]byte("ok\n"))
}

// remoteIP returns the address of the connection the request arrived on.
// Forwarding headers are ignored: the server is not meant to sit behind a
// proxy, and they are trivially spoofed.
func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// migrate creates the schema and brings an older database up to date.
func migrate(db *sql.DB) error {
	if _, err := db.Exec(schema); err != nil {
		return err
	}
	// Databases created before the ip column existed need it added.
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('pings') WHERE name = 'ip'`).Scan(&n)
	if err != nil {
		return err
	}
	if n == 0 {
		_, err = db.Exec(`ALTER TABLE pings ADD COLUMN ip TEXT NOT NULL DEFAULT ''`)
	}
	return err
}

// queryPings runs a query returning (client_id, status, received_at, ip) rows.
func (s *server) queryPings(ctx context.Context, now time.Time, query string, args ...any) ([]ping, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var pings []ping
	for rows.Next() {
		var p ping
		var ts int64
		if err := rows.Scan(&p.ClientID, &p.Status, &ts, &p.IP); err != nil {
			return nil, err
		}
		p.ReceivedAt = time.Unix(ts, 0).Local()
		age := now.Sub(p.ReceivedAt)
		p.Ago = ago(age)
		p.Stale = age > s.staleAfter
		pings = append(pings, p)
	}
	return pings, rows.Err()
}

// loadClients builds the per-ID summary cards.
func (s *server) loadClients(ctx context.Context, now time.Time) ([]clientStats, error) {
	// Latest ping, lifetime count and first-seen time for every ID.
	rows, err := s.db.QueryContext(ctx,
		`SELECT p.client_id, p.status, p.received_at, p.ip, a.total, a.first_at
		 FROM pings p
		 JOIN (SELECT MAX(id) AS last_id, COUNT(*) AS total, MIN(received_at) AS first_at
		       FROM pings GROUP BY client_id) a ON p.id = a.last_id
		 ORDER BY p.id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var clients []clientStats
	for rows.Next() {
		var c clientStats
		var ts, first int64
		if err := rows.Scan(&c.ClientID, &c.Status, &ts, &c.IP, &c.Total, &first); err != nil {
			return nil, err
		}
		c.ReceivedAt = time.Unix(ts, 0).Local()
		age := now.Sub(c.ReceivedAt)
		c.Ago = ago(age)
		c.Stale = age > s.staleAfter
		c.FirstSeen = time.Unix(first, 0).Local()
		c.Tracked = ago(now.Sub(c.FirstSeen))
		clients = append(clients, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()

	// Walk the window's pings in order to find gaps and battery range per ID.
	type window struct {
		count, minBatt, maxBatt int
		prev, maxGap            int64
		hasBatt                 bool
	}
	windows := map[string]*window{}
	wrows, err := s.db.QueryContext(ctx,
		`SELECT client_id, status, received_at FROM pings WHERE received_at >= ? ORDER BY id`,
		now.Add(-statsWindow).Unix())
	if err != nil {
		return nil, err
	}
	defer wrows.Close()
	for wrows.Next() {
		var id, status string
		var ts int64
		if err := wrows.Scan(&id, &status, &ts); err != nil {
			return nil, err
		}
		w := windows[id]
		if w == nil {
			w = &window{}
			windows[id] = w
		}
		if w.count > 0 && ts-w.prev > w.maxGap {
			w.maxGap = ts - w.prev
		}
		w.prev = ts
		w.count++
		if m := batteryRE.FindStringSubmatch(status); m != nil {
			level, _ := strconv.Atoi(m[1])
			if !w.hasBatt || level < w.minBatt {
				w.minBatt = level
			}
			if !w.hasBatt || level > w.maxBatt {
				w.maxBatt = level
			}
			w.hasBatt = true
		}
	}
	if err := wrows.Err(); err != nil {
		return nil, err
	}

	for i := range clients {
		c := &clients[i]
		c.LongestGap, c.Battery = "—", "—"
		w := windows[c.ClientID]
		if w == nil {
			continue
		}
		c.Recent = w.count
		if w.count > 1 {
			c.LongestGap = span(time.Duration(w.maxGap) * time.Second)
		}
		switch {
		case w.hasBatt && w.minBatt == w.maxBatt:
			c.Battery = fmt.Sprintf("%d%%", w.minBatt)
		case w.hasBatt:
			c.Battery = fmt.Sprintf("%d–%d%%", w.minBatt, w.maxBatt)
		}
	}
	return clients, nil
}

func (s *server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	data := indexData{Limit: recentLimit, Now: time.Now()}
	var err error
	data.Pings, err = s.queryPings(r.Context(), data.Now,
		`SELECT client_id, status, received_at, ip FROM pings ORDER BY id DESC LIMIT ?`,
		recentLimit)
	if err == nil {
		data.Clients, err = s.loadClients(r.Context(), data.Now)
	}
	if err != nil {
		log.Printf("load pings: %v", err)
		http.Error(w, "could not load pings", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := indexTmpl.Execute(w, data); err != nil {
		log.Printf("render index: %v", err)
	}
}

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "address to listen on")
	dbPath := flag.String("db", "pings.db", "path to the SQLite database file")
	staleAfter := flag.Duration("stale", 11*time.Minute, "mark an ID as stale when its last ping is older than this")
	flag.Parse()

	db, err := sql.Open("sqlite", *dbPath)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1) // SQLite allows one writer; keep it simple
	if err := migrate(db); err != nil {
		log.Fatalf("set up database: %v", err)
	}

	// The token comes from the environment so it stays out of the process list.
	s := &server{db: db, staleAfter: *staleAfter, token: os.Getenv("PING_TOKEN")}
	if s.token == "" {
		log.Printf("PING_TOKEN is not set: /ping accepts requests from anyone who can reach %s", *addr)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/ping", s.handlePing)
	mux.HandleFunc("/", s.handleIndex)

	log.Printf("listening on http://%s (db: %s)", *addr, *dbPath)
	log.Fatal(http.ListenAndServe(*addr, mux))
}
