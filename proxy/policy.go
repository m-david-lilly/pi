package main

import (
	"database/sql"
	"strings"
	"sync"
	"time"
)

// Policy holds the per-device HTTPS inspection switch. Every device still goes
// through the proxy; the switch only decides whether its TLS is decrypted or
// blind-tunnelled. Devices that cannot install the CA are turned off here so
// they keep working (host-level filtering only) instead of failing closed with
// a certificate warning on every page.
//
// Like Rules, it is cached in memory and refreshed from the DB so the dashboard
// can change it without restarting the proxy. The table is tiny (one row per
// configured device), so a short interval is cheap and makes the toggle feel
// immediate.
type Policy struct {
	db        *sql.DB
	mu        sync.RWMutex
	intercept map[string]bool // MAC -> decrypt TLS
	stop      chan struct{}
	stopOnce  sync.Once
}

func newPolicy(db *sql.DB) *Policy {
	p := &Policy{db: db, stop: make(chan struct{})}
	p.reload()
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-p.stop:
				return
			case <-t.C:
				p.reload()
			}
		}
	}()
	return p
}

// Close stops the reload loop. Tests and Store.Close call it; the cache itself
// remains readable.
func (p *Policy) Close() {
	if p == nil || p.stop == nil {
		return
	}
	p.stopOnce.Do(func() { close(p.stop) })
}

func (p *Policy) reload() {
	if p == nil || p.db == nil {
		return
	}
	m := map[string]bool{}
	rows, err := p.db.Query(`SELECT mac, intercept FROM device_policy`)
	if err != nil {
		// Keep the last good map: a transient busy DB must not silently turn
		// inspection back on for every device.
		return
	}
	for rows.Next() {
		var (
			mac       string
			intercept int
		)
		if rows.Scan(&mac, &intercept) == nil {
			m[strings.ToLower(strings.TrimSpace(mac))] = intercept != 0
		}
	}
	rows.Close()
	p.mu.Lock()
	p.intercept = m
	p.mu.Unlock()
}

// Intercept reports whether TLS for this device should be decrypted. An unknown
// device (no policy row, or no DHCP lease) keeps the historical behaviour:
// inspect it. Turning inspection off is an explicit per-device decision.
func (p *Policy) Intercept(mac string) bool {
	if p == nil {
		return true
	}
	mac = strings.ToLower(strings.TrimSpace(mac))
	if mac == "" {
		return true
	}
	p.mu.RLock()
	v, ok := p.intercept[mac]
	p.mu.RUnlock()
	if !ok {
		return true
	}
	return v
}
