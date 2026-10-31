package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

type Event struct {
	TS       time.Time `json:"ts"`
	SrcIP    string    `json:"src_ip"`
	Device   string    `json:"device"`
	Method   string    `json:"method"`
	Scheme   string    `json:"scheme"`
	Host     string    `json:"host"`
	URL      string    `json:"url"`
	Status   int       `json:"status"`
	MIME     string    `json:"mime"`
	ReqBytes int64     `json:"req_bytes"`
	RespSize int64     `json:"resp_bytes"`
	Snippet  string    `json:"snippet,omitempty"`
	Action   string    `json:"action"` // bump | tunnel | block | error
	Reason   string    `json:"reason,omitempty"`
	Video    *YouTube  `json:"youtube,omitempty"`
	Search   *Search   `json:"search,omitempty"`
}

type Search struct {
	Engine string `json:"engine"`
	Query  string `json:"query"`
}

type YouTube struct {
	VideoID string `json:"video_id"`
	Title   string `json:"title"`
	Channel string `json:"channel"`
	Seconds int    `json:"seconds"`
	Query   string `json:"query,omitempty"`
}

type Alert struct {
	TS       time.Time `json:"ts"`
	Severity string    `json:"severity"`
	Kind     string    `json:"kind"`
	Device   string    `json:"device"`
	Host     string    `json:"host"`
	URL      string    `json:"url"`
	Detail   string    `json:"detail"`
}

const schema = `
CREATE TABLE IF NOT EXISTS events (
  id INTEGER PRIMARY KEY,
  ts INTEGER NOT NULL,
  src_ip TEXT, device TEXT, method TEXT, scheme TEXT, host TEXT, url TEXT,
  status INTEGER, mime TEXT, req_bytes INTEGER, resp_bytes INTEGER,
  snippet TEXT, action TEXT, reason TEXT
);
CREATE INDEX IF NOT EXISTS idx_events_ts ON events(ts);
CREATE INDEX IF NOT EXISTS idx_events_host ON events(host);
CREATE INDEX IF NOT EXISTS idx_events_src ON events(src_ip, ts);
CREATE TABLE IF NOT EXISTS alerts (
  id INTEGER PRIMARY KEY, ts INTEGER, severity TEXT, kind TEXT,
  device TEXT, host TEXT, url TEXT, detail TEXT, notified INTEGER DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_alerts_ts ON alerts(ts);
CREATE TABLE IF NOT EXISTS youtube (
  id INTEGER PRIMARY KEY, ts INTEGER, device TEXT, video_id TEXT,
  title TEXT, channel TEXT, seconds INTEGER, query TEXT, url TEXT
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_yt_video ON youtube(video_id, device);
CREATE TABLE IF NOT EXISTS searches (
  id INTEGER PRIMARY KEY, ts INTEGER, device TEXT, engine TEXT, query TEXT, url TEXT
);
CREATE INDEX IF NOT EXISTS idx_searches_ts ON searches(ts);
CREATE TABLE IF NOT EXISTS domains (
  host TEXT PRIMARY KEY, first_ts INTEGER, last_ts INTEGER, hits INTEGER
);
CREATE TABLE IF NOT EXISTS rules (
  id INTEGER PRIMARY KEY, kind TEXT, value TEXT, action TEXT, note TEXT,
  added_ts INTEGER
);
`

type Store struct {
	db        *sql.DB
	cfg       *Config
	rules     *Rules
	ch        chan *Event
	alerts    chan *Alert
	leases    map[string]string // ip -> mac
	leaseMu   sync.Mutex
	leaseAt   time.Time
	alertMu   sync.Mutex
	alertSeen map[string]time.Time
	stop      chan struct{}
	closeOnce sync.Once
}

func openStore(cfg *Config) (*Store, error) {
	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		return nil, err
	}
	path := filepath.Join(cfg.DataDir, "events.db")
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}
	s := &Store{
		db:        db,
		cfg:       cfg,
		ch:        make(chan *Event, 2048),
		alerts:    make(chan *Alert, 256),
		leases:    map[string]string{},
		alertSeen: map[string]time.Time{},
		stop:      make(chan struct{}),
	}
	s.rules = newRules(cfg, db)
	go s.writer()
	return s, nil
}

func (s *Store) Ingest(ev *Event) {
	select {
	case s.ch <- ev:
	default:
		log.Printf("store: queue full, dropping event for %s", ev.Host)
	}
}

func (s *Store) Alerts() <-chan *Alert { return s.alerts }

func (s *Store) writer() {
	prune := time.NewTicker(6 * time.Hour)
	defer prune.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-prune.C:
			s.prune()
		case ev := <-s.ch:
			if err := s.writeEvent(ev); err != nil {
				log.Printf("store: %v", err)
			}
		}
	}
}

func (s *Store) writeEvent(ev *Event) error {
	ev.Device = s.deviceName(ev.SrcIP)
	host := strings.ToLower(ev.Host)
	ts := ev.TS.Unix()

	res, err := s.db.Exec(`INSERT INTO events
		(ts,src_ip,device,method,scheme,host,url,status,mime,req_bytes,resp_bytes,snippet,action,reason)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		ts, ev.SrcIP, ev.Device, ev.Method, ev.Scheme, host, ev.URL, ev.Status,
		ev.MIME, ev.ReqBytes, ev.RespSize, truncate(ev.Snippet, s.cfg.Capture.SnippetBytes),
		ev.Action, ev.Reason)
	if err != nil {
		return err
	}
	evID, _ := res.LastInsertId()

	if ev.Search != nil && ev.Search.Query != "" {
		_, _ = s.db.Exec(`INSERT INTO searches (ts,device,engine,query,url) VALUES (?,?,?,?,?)`,
			ts, ev.Device, ev.Search.Engine, ev.Search.Query, ev.URL)
	}

	if ev.Video != nil && ev.Video.VideoID != "" {
		// Most hits for a video carry only its ID (watch-page reloads, caption
		// fetches); keep the title/channel/length learned from the player
		// response instead of blanking them.
		_, _ = s.db.Exec(`INSERT INTO youtube (ts,device,video_id,title,channel,seconds,query,url)
			VALUES (?,?,?,?,?,?,?,?)
			ON CONFLICT(video_id,device) DO UPDATE SET
			  title=COALESCE(NULLIF(excluded.title,''), title),
			  channel=COALESCE(NULLIF(excluded.channel,''), channel),
			  seconds=CASE WHEN excluded.seconds > 0 THEN excluded.seconds ELSE seconds END,
			  query=COALESCE(NULLIF(excluded.query,''), query),
			  ts=excluded.ts`,
			ts, ev.Device, ev.Video.VideoID, ev.Video.Title, ev.Video.Channel,
			ev.Video.Seconds, ev.Video.Query, ev.URL)
	}

	first := false
	if host != "" {
		var exists int
		_ = s.db.QueryRow(`SELECT 1 FROM domains WHERE host=?`, host).Scan(&exists)
		first = exists == 0
		if first {
			_, _ = s.db.Exec(`INSERT INTO domains (host,first_ts,last_ts,hits) VALUES (?,?,?,1)`, host, ts, ts)
		} else {
			_, _ = s.db.Exec(`UPDATE domains SET last_ts=?, hits=hits+1 WHERE host=?`, ts, host)
		}
	}

	for _, a := range s.evaluate(ev, evID, first) {
		a.Device = ev.Device
		// A pinned app can fail every single request; one alert per hour per
		// (kind, host, device) is enough for a parent to notice.
		if !s.allowAlert(a.Kind+"|"+a.Host+"|"+a.Device, a.TS) {
			continue
		}
		if _, err := s.db.Exec(`INSERT INTO alerts (ts,severity,kind,device,host,url,detail)
			VALUES (?,?,?,?,?,?,?)`,
			a.TS.Unix(), a.Severity, a.Kind, a.Device, a.Host, a.URL, a.Detail); err != nil {
			log.Printf("store: alert: %v", err)
		}
		select {
		case s.alerts <- a:
		default:
		}
	}
	return nil
}

// allowAlert suppresses repeat alerts for the same key within an hour.
func (s *Store) allowAlert(key string, ts time.Time) bool {
	s.alertMu.Lock()
	defer s.alertMu.Unlock()
	if s.alertSeen == nil {
		s.alertSeen = map[string]time.Time{}
	}
	if last, ok := s.alertSeen[key]; ok && ts.Sub(last) < time.Hour {
		return false
	}
	if len(s.alertSeen) > 5000 {
		s.alertSeen = map[string]time.Time{}
	}
	s.alertSeen[key] = ts
	return true
}

// prune applies retention_days to every table that records browsing history,
// not just events: searches, videos and visited hosts are the most sensitive.
func (s *Store) prune() {
	cutoff := time.Now().AddDate(0, 0, -s.cfg.Retention).Unix()
	for _, q := range []string{
		`DELETE FROM events WHERE ts < ?`,
		`DELETE FROM alerts WHERE ts < ?`,
		`DELETE FROM youtube WHERE ts < ?`,
		`DELETE FROM searches WHERE ts < ?`,
		`DELETE FROM domains WHERE last_ts < ?`,
	} {
		if _, err := s.db.Exec(q, cutoff); err != nil {
			log.Printf("store: prune: %v", err)
		}
	}
}

// deviceName resolves an IP to a friendly name using the dnsmasq lease file
// and the MAC map in config. Cached for 20s so we do not stat /tmp per request.
func (s *Store) deviceName(ip string) string {
	s.leaseMu.Lock()
	defer s.leaseMu.Unlock()
	if time.Since(s.leaseAt) > 20*time.Second {
		s.leases = readLeases()
		s.leaseAt = time.Now()
	}
	mac := s.leases[ip]
	if mac == "" {
		return ip
	}
	if name, ok := s.cfg.Devices[strings.ToLower(mac)]; ok {
		return name
	}
	return mac
}

func readLeases() map[string]string {
	out := map[string]string{}
	b, err := os.ReadFile("/tmp/dhcp.leases")
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) >= 3 {
			if net.ParseIP(f[2]) != nil {
				out[f[2]] = strings.ToLower(f[1])
			}
		}
	}
	return out
}

func truncate(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	return s[:n]
}

func (s *Store) Close() {
	s.closeOnce.Do(func() {
		close(s.stop)
		_ = s.db.Close()
	})
}

func jsonOut(v any) string {
	b, _ := json.MarshalIndent(v, "", "  ")
	return string(b)
}
