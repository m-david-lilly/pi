package main

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"
)

// writableDB opens the store read-write. The proxy runs with WAL and a busy
// timeout, so a short write from the dashboard CLI is safe alongside it.
func writableDB(path string) (*sql.DB, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

type statsOut struct {
	Events24h   int64  `json:"events_24h"`
	Alerts24h   int64  `json:"alerts_24h"`
	Devices24h  int64  `json:"devices_24h"`
	YouTube24h  int64  `json:"youtube_24h"`
	Searches24h int64  `json:"searches_24h"`
	Rules       int64  `json:"rules"`
	DBBytes     int64  `json:"db_bytes"`
	LastEvent   string `json:"last_event"`
}

func collectStats(db *sql.DB, cutoff int64) (statsOut, error) {
	var out statsOut
	count := func(q string, dst *int64, params ...any) error {
		return db.QueryRow(q, params...).Scan(dst)
	}
	if err := count(`SELECT COUNT(*) FROM events WHERE ts>=?`, &out.Events24h, cutoff); err != nil {
		return out, err
	}
	if err := count(`SELECT COUNT(*) FROM alerts WHERE ts>=?`, &out.Alerts24h, cutoff); err != nil {
		return out, err
	}
	if err := count(`SELECT COUNT(DISTINCT device) FROM events WHERE ts>=? AND device!=''`, &out.Devices24h, cutoff); err != nil {
		return out, err
	}
	if err := count(`SELECT COUNT(*) FROM youtube WHERE ts>=?`, &out.YouTube24h, cutoff); err != nil {
		return out, err
	}
	if err := count(`SELECT COUNT(*) FROM searches WHERE ts>=?`, &out.Searches24h, cutoff); err != nil {
		return out, err
	}
	if err := count(`SELECT COUNT(*) FROM rules`, &out.Rules); err != nil {
		return out, err
	}
	var last sql.NullInt64
	if err := db.QueryRow(`SELECT MAX(ts) FROM events`).Scan(&last); err != nil && err != sql.ErrNoRows {
		return out, err
	}
	if last.Valid {
		out.LastEvent = time.Unix(last.Int64, 0).Format(time.RFC3339)
	}
	return out, nil
}

// statsCmd prints the counters the dashboard needs in one round trip, so the
// CGI never has to count JSON rows in shell.
func statsCmd(args []string) {
	fs := flag.NewFlagSet("stats", flag.ExitOnError)
	dbPath := fs.String("db", "/var/lib/kidproxy/events.db", "database path")
	since := fs.String("since", "24h", "lookback window")
	_ = fs.Parse(args)

	dur, err := parseSince(*since)
	if err != nil {
		fail(err)
	}
	db, err := sql.Open("sqlite", fmt.Sprintf("file:%s?mode=ro&_pragma=busy_timeout(5000)", *dbPath))
	if err != nil {
		fail(err)
	}
	defer db.Close()

	out, err := collectStats(db, time.Now().Add(-dur).Unix())
	if err != nil {
		fail(err)
	}
	if fi, err := os.Stat(*dbPath); err == nil {
		out.DBBytes = fi.Size()
		// WAL mode keeps recent writes in a side file; report both so the
		// dashboard does not understate disk use.
		if w, err := os.Stat(*dbPath + "-wal"); err == nil {
			out.DBBytes += w.Size()
		}
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", " ")
	_ = enc.Encode(out)
}

func normalizeDomain(v string) (string, error) {
	v = strings.ToLower(strings.TrimSpace(v))
	if v == "" || strings.ContainsAny(v, " \t/") || strings.Contains(v, "..") {
		return "", fmt.Errorf("invalid domain %q", v)
	}
	return v, nil
}

func insertRule(db *sql.DB, kind, action, value, note string) (int64, error) {
	// Re-adding the same rule replaces it instead of stacking duplicates.
	if _, err := db.Exec(`DELETE FROM rules WHERE kind=? AND value=? AND action=?`, kind, value, action); err != nil {
		return 0, err
	}
	res, err := db.Exec(`INSERT INTO rules(kind,value,action,note,added_ts) VALUES(?,?,?,?,?)`,
		kind, value, action, note, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func deleteRule(db *sql.DB, id int64) (bool, error) {
	res, err := db.Exec(`DELETE FROM rules WHERE id=?`, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// deleteAlerts removes alert rows, optionally restricted to one kind. Returns
// the number of rows removed.
func deleteAlerts(db *sql.DB, kind string) (int64, error) {
	var (
		res sql.Result
		err error
	)
	if kind == "" {
		res, err = db.Exec(`DELETE FROM alerts`)
	} else {
		res, err = db.Exec(`DELETE FROM alerts WHERE kind=?`, kind)
	}
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// alertsCmd is the maintenance path for alert history. Alert noise is easy to
// generate (a pinned app can produce hundreds of rows before a parent looks),
// and there is no sqlite3 CLI on the router, so the binary has to do it.
func alertsCmd(args []string) {
	if len(args) == 0 || args[0] != "clear" {
		fmt.Fprintln(os.Stderr, "usage: kidproxy alerts clear [--kind tls-pin]")
		os.Exit(2)
	}
	fs := flag.NewFlagSet("alerts clear", flag.ExitOnError)
	dbPath := fs.String("db", "/var/lib/kidproxy/events.db", "database path")
	kind := fs.String("kind", "", "only this alert kind (default: all)")
	_ = fs.Parse(args[1:])

	db, err := writableDB(*dbPath)
	if err != nil {
		fail(err)
	}
	defer db.Close()
	n, err := deleteAlerts(db, strings.TrimSpace(*kind))
	if err != nil {
		fail(err)
	}
	fmt.Printf("cleared %d alert(s)\n", n)
}

// ruleCmd manages dashboard-added domain rules. The proxy reloads them every
// 60s, so no restart is needed.
func ruleCmd(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: kidproxy rule add|rm|list [...]")
		os.Exit(2)
	}
	switch args[0] {
	case "add":
		fs := flag.NewFlagSet("rule add", flag.ExitOnError)
		dbPath := fs.String("db", "/var/lib/kidproxy/events.db", "database path")
		kind := fs.String("kind", "domain", "rule kind (domain)")
		action := fs.String("action", "block", "block or allow")
		value := fs.String("value", "", "domain, e.g. example.com")
		note := fs.String("note", "", "free-text note shown on the block page")
		_ = fs.Parse(args[1:])

		if *kind != "domain" {
			fail(fmt.Errorf("unsupported rule kind %q", *kind))
		}
		if *action != "block" && *action != "allow" {
			fail(fmt.Errorf("action must be block or allow"))
		}
		v, err := normalizeDomain(*value)
		if err != nil {
			fail(err)
		}
		db, err := writableDB(*dbPath)
		if err != nil {
			fail(err)
		}
		defer db.Close()
		id, err := insertRule(db, *kind, *action, v, strings.TrimSpace(*note))
		if err != nil {
			fail(err)
		}
		fmt.Printf("added rule %d: %s %s %s\n", id, *action, *kind, v)
	case "rm":
		fs := flag.NewFlagSet("rule rm", flag.ExitOnError)
		dbPath := fs.String("db", "/var/lib/kidproxy/events.db", "database path")
		id := fs.Int64("id", 0, "rule id")
		_ = fs.Parse(args[1:])
		if *id <= 0 {
			fail(fmt.Errorf("--id is required"))
		}
		db, err := writableDB(*dbPath)
		if err != nil {
			fail(err)
		}
		defer db.Close()
		ok, err := deleteRule(db, *id)
		if err != nil {
			fail(err)
		}
		if !ok {
			fail(fmt.Errorf("no rule with id %d", *id))
		}
		fmt.Printf("removed rule %d\n", *id)
	case "list":
		queryCmd([]string{"--kind", "rules", "--limit", "200"})
	default:
		fmt.Fprintln(os.Stderr, "usage: kidproxy rule add|rm|list [...]")
		os.Exit(2)
	}
}
