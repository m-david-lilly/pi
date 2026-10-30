package main

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// query is the read-only access path used by the dashboard CGI. Keeping it in
// the binary avoids shipping the sqlite3 CLI and keeps one schema definition.
func queryCmd(args []string) {
	fs := flag.NewFlagSet("query", flag.ExitOnError)
	dbPath := fs.String("db", "/var/lib/kidproxy/events.db", "database path")
	kind := fs.String("kind", "events", "events|alerts|youtube|searches|domains|devices|rules")
	limit := fs.Int("limit", 100, "max rows")
	since := fs.String("since", "24h", "lookback window, e.g. 24h or 720h")
	host := fs.String("host", "", "filter by host (suffix match)")
	device := fs.String("device", "", "filter by device name or IP")
	q := fs.String("q", "", "substring match on URL/snippet/title/query")
	_ = fs.Parse(args)

	dur, err := time.ParseDuration(*since)
	if err != nil {
		dur = 24 * time.Hour
	}
	if *limit <= 0 || *limit > 5000 {
		*limit = 100
	}
	dsn := fmt.Sprintf("file:%s?mode=ro&_pragma=busy_timeout(5000)", *dbPath)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		fail(err)
	}
	defer db.Close()

	cutoff := time.Now().Add(-dur).Unix()
	var (
		sqlStr string
		params []any
	)
	switch *kind {
	case "alerts":
		sqlStr = `SELECT ts,severity,kind,device,host,url,detail FROM alerts WHERE ts>=?`
		params = []any{cutoff}
		if *host != "" {
			sqlStr += ` AND host LIKE ?`
			params = append(params, "%"+*host)
		}
		if *device != "" {
			sqlStr += ` AND (device LIKE ? OR device=?)`
			params = append(params, "%"+*device, *device)
		}
		if *q != "" {
			sqlStr += ` AND (url LIKE ? OR detail LIKE ?)`
			params = append(params, "%"+*q+"%", "%"+*q+"%")
		}
		sqlStr += ` ORDER BY ts DESC LIMIT ?`
		params = append(params, *limit)
	case "youtube":
		sqlStr = `SELECT ts,device,video_id,title,channel,seconds,query,url FROM youtube WHERE ts>=?`
		params = []any{cutoff}
		if *device != "" {
			sqlStr += ` AND (device LIKE ? OR device=?)`
			params = append(params, "%"+*device, *device)
		}
		if *q != "" {
			sqlStr += ` AND (title LIKE ? OR channel LIKE ? OR query LIKE ?)`
			params = append(params, "%"+*q+"%", "%"+*q+"%", "%"+*q+"%")
		}
		sqlStr += ` ORDER BY ts DESC LIMIT ?`
		params = append(params, *limit)
	case "searches":
		sqlStr = `SELECT ts,device,engine,query,url FROM searches WHERE ts>=?`
		params = []any{cutoff}
		if *device != "" {
			sqlStr += ` AND (device LIKE ? OR device=?)`
			params = append(params, "%"+*device, *device)
		}
		if *q != "" {
			sqlStr += ` AND query LIKE ?`
			params = append(params, "%"+*q+"%")
		}
		sqlStr += ` ORDER BY ts DESC LIMIT ?`
		params = append(params, *limit)
	case "domains":
		sqlStr = `SELECT host,first_ts,last_ts,hits FROM domains`
		if *host != "" {
			sqlStr += ` WHERE host LIKE ?`
			params = append(params, "%"+*host)
		}
		sqlStr += ` ORDER BY last_ts DESC LIMIT ?`
		params = append(params, *limit)
	case "devices":
		sqlStr = `SELECT device, COUNT(*) AS hits, MAX(ts) AS last_ts FROM events
			WHERE ts>=? GROUP BY device ORDER BY last_ts DESC LIMIT ?`
		params = []any{cutoff, *limit}
	case "rules":
		sqlStr = `SELECT id,kind,value,action,COALESCE(note,''),added_ts FROM rules ORDER BY id DESC LIMIT ?`
		params = []any{*limit}
	default:
		sqlStr = `SELECT ts,device,src_ip,method,scheme,host,url,status,mime,req_bytes,resp_bytes,snippet,action,reason
			FROM events WHERE ts>=?`
		params = []any{cutoff}
		if *host != "" {
			sqlStr += ` AND host LIKE ?`
			params = append(params, "%"+*host)
		}
		if *device != "" {
			sqlStr += ` AND (device LIKE ? OR device=?)`
			params = append(params, "%"+*device, *device)
		}
		if *q != "" {
			sqlStr += ` AND (url LIKE ? OR snippet LIKE ?)`
			params = append(params, "%"+*q+"%", "%"+*q+"%")
		}
		sqlStr += ` ORDER BY ts DESC LIMIT ?`
		params = append(params, *limit)
	}

	rows, err := db.Query(sqlStr, params...)
	if err != nil {
		fail(err)
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	out := make([]map[string]any, 0, 64)
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			fail(err)
		}
		m := make(map[string]any, len(cols))
		for i, c := range cols {
			v := vals[i]
			if b, ok := v.([]byte); ok {
				v = string(b)
			}
			if isTimeCol(c) {
				if n, ok := v.(int64); ok {
					m[c] = time.Unix(n, 0).Format(time.RFC3339)
					continue
				}
			}
			m[c] = v
		}
		out = append(out, m)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", " ")
	_ = enc.Encode(out)
}

func isTimeCol(c string) bool {
	return c == "ts" || c == "first_ts" || c == "last_ts" || c == "added_ts" ||
		strings.HasSuffix(c, "_ts")
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "kidproxy:", err)
	os.Exit(1)
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "router"
	}
	return h
}
