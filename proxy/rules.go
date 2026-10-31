package main

import (
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Rules holds the fast-path blocklist. It is refreshed from the DB so the
// dashboard can add/remove domains without restarting the proxy.
type Rules struct {
	cfg     *Config
	db      *sql.DB
	mu      sync.RWMutex
	hosts   map[string]string // host -> note
	allow   []string          // patterns that override the blocklist
	keyword []string
}

func newRules(cfg *Config, db *sql.DB) *Rules {
	r := &Rules{cfg: cfg, db: db}
	r.reload()
	go func() {
		t := time.NewTicker(60 * time.Second)
		defer t.Stop()
		for range t.C {
			r.reload()
		}
	}()
	return r
}

func (r *Rules) reload() {
	hosts := map[string]string{}
	for _, d := range r.cfg.Block.Domains {
		hosts[strings.ToLower(strings.TrimSpace(d))] = "config"
	}
	var allow []string
	if r.db != nil {
		rows, err := r.db.Query(`SELECT value, COALESCE(note,''), action FROM rules WHERE kind='domain'`)
		if err == nil {
			for rows.Next() {
				var v, note, action string
				if rows.Scan(&v, &note, &action) == nil {
					if action == "allow" {
						allow = append(allow, strings.ToLower(strings.TrimSpace(v)))
						continue
					}
					hosts[strings.ToLower(v)] = note
				}
			}
			rows.Close()
		}
	}
	kws := make([]string, 0, len(r.cfg.Block.Keywords))
	for _, k := range r.cfg.Block.Keywords {
		if k = strings.ToLower(strings.TrimSpace(k)); k != "" {
			kws = append(kws, k)
		}
	}
	r.mu.Lock()
	r.hosts = hosts
	r.allow = allow
	r.keyword = kws
	r.mu.Unlock()
}

// blocked reports whether a hostname is on the blocklist, with a human note.
// An allow rule for the same host wins over both the config list and the DB.
func (r *Rules) blocked(host string) (bool, string) {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, pat := range r.allow {
		if hostMatches(host, pat) {
			return false, ""
		}
	}
	for pat, note := range r.hosts {
		if hostMatches(host, pat) {
			return true, note
		}
	}
	return false, ""
}

func hostMatches(host, pattern string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	pattern = strings.ToLower(strings.TrimSpace(pattern))
	if pattern == "" {
		return false
	}
	if strings.HasPrefix(pattern, "*.") {
		suffix := pattern[1:] // ".example.com"
		return strings.HasSuffix(host, suffix) || host == strings.TrimPrefix(suffix, ".")
	}
	return host == pattern || strings.HasSuffix(host, "."+pattern)
}

func (r *Rules) keywordHit(text string) (string, bool) {
	text = strings.ToLower(text)
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, k := range r.keyword {
		if strings.Contains(text, k) {
			return k, true
		}
	}
	return "", false
}

// evaluate turns one stored event into zero or more alerts.
func (s *Store) evaluate(ev *Event, evID int64, firstSeen bool) []*Alert {
	now := ev.TS
	var out []*Alert
	add := func(sev, kind, detail string) {
		out = append(out, &Alert{TS: now, Severity: sev, Kind: kind, Host: ev.Host, URL: ev.URL, Detail: detail})
	}

	switch ev.Action {
	case "block":
		add("warn", "blocked", "blocked: "+ev.Reason)
	case "error":
		if strings.Contains(ev.Reason, "handshake") {
			add("info", "tls-pin", "TLS handshake failed (pinned app or bad cert): "+ev.Reason)
		}
	}

	text := ev.URL
	if ev.Snippet != "" {
		text += " " + ev.Snippet
	}
	if ev.Video != nil {
		text += " " + ev.Video.Title + " " + ev.Video.Channel
	}
	if ev.Search != nil {
		text += " " + ev.Search.Query
	}
	if kw, ok := s.rules.keywordHit(text); ok {
		add("high", "keyword", "matched keyword: "+kw)
	}

	nav := ev.Action != "block" && ev.Method == "GET" && ev.Status == 200 &&
		strings.HasPrefix(strings.ToLower(ev.MIME), "text/html")
	if nav && firstSeen && s.cfg.Alert.FirstSeen {
		add("info", "new-site", "first visit to "+ev.Host)
	}
	if nav && s.cfg.isLateNight(now) {
		add("warn", "late-night", fmt.Sprintf("browsing %s at %s", ev.Host, now.Format("15:04")))
	}
	return out
}
