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

// YouTube is the metadata we can learn without downloading the video. The
// player response carries most of it; the watch page's meta tags and embedded
// ytInitialPlayerResponse are the fallback when the player call was not seen.
type YouTube struct {
	VideoID     string   `json:"video_id"`
	Title       string   `json:"title"`
	Channel     string   `json:"channel"`
	ChannelID   string   `json:"channel_id,omitempty"`
	Seconds     int      `json:"seconds"`
	Category    string   `json:"category,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	Description string   `json:"description,omitempty"`
	PublishDate string   `json:"publish_date,omitempty"`
	ViewCount   int64    `json:"view_count,omitempty"`
	FamilySafe  *bool    `json:"family_safe,omitempty"`
	Thumbnail   string   `json:"thumbnail,omitempty"`
	Live        bool     `json:"live,omitempty"`
	WatchURL    string   `json:"watch_url,omitempty"`
	Query       string   `json:"query,omitempty"`
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
  title TEXT, channel TEXT, seconds INTEGER, query TEXT, url TEXT,
  channel_id TEXT, category TEXT, tags TEXT, description TEXT,
  publish_date TEXT, view_count INTEGER, family_safe INTEGER,
  thumbnail TEXT, watch_url TEXT, live INTEGER DEFAULT 0
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_yt_video ON youtube(video_id, device);
CREATE INDEX IF NOT EXISTS idx_yt_ts ON youtube(ts);
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
CREATE TABLE IF NOT EXISTS device_policy (
  mac TEXT PRIMARY KEY, intercept INTEGER NOT NULL DEFAULT 1, note TEXT,
  ca_trust INTEGER, ca_trust_ts INTEGER, updated_ts INTEGER
);
`

// youtubeAdded are the columns added after the first release. CREATE TABLE IF
// NOT EXISTS never alters an existing table, so upgrades add them in place.
var youtubeAdded = []struct{ name, ddl string }{
	{"channel_id", "TEXT"},
	{"category", "TEXT"},
	{"tags", "TEXT"},
	{"description", "TEXT"},
	{"publish_date", "TEXT"},
	{"view_count", "INTEGER"},
	{"family_safe", "INTEGER"},
	{"thumbnail", "TEXT"},
	{"watch_url", "TEXT"},
	{"live", "INTEGER DEFAULT 0"},
}

// migrate brings a database created by an older kidproxy up to the current
// schema without losing the history parents have already collected.
func migrate(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(youtube)`)
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for rows.Next() {
		var (
			cid     int
			name    string
			ctype   string
			notnull int
			dflt    any
			pk      int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			rows.Close()
			return err
		}
		have[strings.ToLower(name)] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, c := range youtubeAdded {
		if have[c.name] {
			continue
		}
		if _, err := db.Exec(`ALTER TABLE youtube ADD COLUMN ` + c.name + ` ` + c.ddl); err != nil {
			return err
		}
	}
	_, err = db.Exec(`CREATE INDEX IF NOT EXISTS idx_yt_category ON youtube(category)`)
	return err
}

type Store struct {
	db        *sql.DB
	cfg       *Config
	rules     *Rules
	policy    *Policy
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
	if err := migrate(db); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
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
	s.policy = newPolicy(db)
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
		// fetches); keep the metadata learned from the player response or watch
		// page instead of blanking it. The device is stored as the MAC when the
		// lease is known so the analytics page can map it to a kid.
		v := ev.Video
		device := s.deviceMAC(ev.SrcIP)
		if device == "" {
			device = ev.Device
		}
		watch := v.WatchURL
		if watch == "" {
			watch = "https://www.youtube.com/watch?v=" + v.VideoID
		}
		tags := ""
		if len(v.Tags) > 0 {
			if b, err := json.Marshal(v.Tags); err == nil {
				tags = string(b)
			}
		}
		var family any
		if v.FamilySafe != nil {
			if *v.FamilySafe {
				family = 1
			} else {
				family = 0
			}
		}
		live := 0
		if v.Live {
			live = 1
		}
		_, _ = s.db.Exec(`INSERT INTO youtube
			(ts,device,video_id,title,channel,seconds,query,url,channel_id,category,
			 tags,description,publish_date,view_count,family_safe,thumbnail,watch_url,live)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT(video_id,device) DO UPDATE SET
			  title=COALESCE(NULLIF(excluded.title,''), title),
			  channel=COALESCE(NULLIF(excluded.channel,''), channel),
			  channel_id=COALESCE(NULLIF(excluded.channel_id,''), channel_id),
			  seconds=CASE WHEN excluded.seconds > 0 THEN excluded.seconds ELSE seconds END,
			  category=COALESCE(NULLIF(excluded.category,''), category),
			  tags=COALESCE(NULLIF(excluded.tags,''), tags),
			  description=COALESCE(NULLIF(excluded.description,''), description),
			  publish_date=COALESCE(NULLIF(excluded.publish_date,''), publish_date),
			  view_count=CASE WHEN excluded.view_count > 0 THEN excluded.view_count ELSE view_count END,
			  family_safe=COALESCE(excluded.family_safe, family_safe),
			  thumbnail=COALESCE(NULLIF(excluded.thumbnail,''), thumbnail),
			  watch_url=COALESCE(NULLIF(excluded.watch_url,''), watch_url),
			  live=CASE WHEN excluded.live > 0 THEN excluded.live ELSE live END,
			  query=COALESCE(NULLIF(excluded.query,''), query),
			  url=COALESCE(NULLIF(excluded.url,''), url),
			  ts=excluded.ts`,
			ts, device, v.VideoID, v.Title, v.Channel, v.Seconds, v.Query, ev.URL,
			v.ChannelID, v.Category, tags, truncate(v.Description, 4000), v.PublishDate,
			v.ViewCount, family, v.Thumbnail, watch, live)
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

// leaseMap returns IP -> MAC, refreshed at most every 20s so we do not stat
// /tmp on every request.
func (s *Store) leaseMap() map[string]string {
	s.leaseMu.Lock()
	defer s.leaseMu.Unlock()
	if time.Since(s.leaseAt) > 20*time.Second {
		s.leases = readLeases()
		s.leaseAt = time.Now()
	}
	return s.leases
}

// deviceName resolves an IP to a friendly name using the dnsmasq lease file
// and the MAC map in config.
func (s *Store) deviceName(ip string) string {
	mac := s.leaseMap()[ip]
	if mac == "" {
		return ip
	}
	if name, ok := s.cfg.Devices[strings.ToLower(mac)]; ok {
		return name
	}
	return mac
}

// deviceMAC returns the lease MAC for an IP, or "" when it is not known. The
// analytics page keys kids by MAC, so video rows use this rather than the
// friendly name.
func (s *Store) deviceMAC(ip string) string {
	return s.leaseMap()[ip]
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
		s.policy.Close()
		_ = s.db.Close()
	})
}

func jsonOut(v any) string {
	b, _ := json.MarshalIndent(v, "", "  ")
	return string(b)
}
