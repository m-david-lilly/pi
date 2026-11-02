package main

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
)

// leaseInfo is one dnsmasq lease: the MAC and the hostname the device asked
// for. The hostname is often "*" (a device that sends no DHCP name).
type leaseInfo struct {
	MAC  string
	Host string
}

var macRe = regexp.MustCompile(`^([0-9a-f]{2}:){5}[0-9a-f]{2}$`)

// leaseDetailsFunc is a seam for tests: the real implementation reads the
// router's lease file, which does not exist on a developer machine.
var leaseDetailsFunc = readLeaseDetails

func readLeaseDetails() map[string]leaseInfo {
	out := map[string]leaseInfo{}
	b, err := os.ReadFile("/tmp/dhcp.leases")
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 || net.ParseIP(f[2]) == nil {
			continue
		}
		host := ""
		if len(f) >= 4 && f[3] != "*" {
			host = f[3]
		}
		out[f[2]] = leaseInfo{MAC: strings.ToLower(f[1]), Host: host}
	}
	return out
}

// deviceRow is the dashboard's view of one device. It merges three sources:
// the DHCP lease (IP/hostname), the per-device policy row (intercept/CA trust),
// and recent handshake failures (a hint that the device may not trust the CA).
type deviceRow struct {
	MAC         string `json:"mac"`
	IP          string `json:"ip,omitempty"`
	Host        string `json:"host,omitempty"`
	Online      bool   `json:"online"`
	Intercept   bool   `json:"intercept"`
	Configured  bool   `json:"configured"`
	Note        string `json:"note,omitempty"`
	CATrust     *bool  `json:"ca_trust"`
	CATrustAt   string `json:"ca_trust_at,omitempty"`
	Failures24h int64  `json:"handshake_failures_24h"`
	UpdatedAt   string `json:"updated_at,omitempty"`
}

// listDevices unions policy rows with live leases. Policy rows survive a device
// going offline so a parent can still change the switch before the device
// rejoins. Only kids-subnet leases are included; the adult LAN is untouched.
func listDevices(db *sql.DB, cfg *Config, cutoff int64) ([]deviceRow, error) {
	byMAC := map[string]*deviceRow{}
	if db != nil {
		rows, err := db.Query(`SELECT mac, intercept, COALESCE(note,''),
			ca_trust, COALESCE(ca_trust_ts,0), COALESCE(updated_ts,0)
			FROM device_policy`)
		if err == nil {
			for rows.Next() {
				var (
					mac, note          string
					intercept          int
					trust              sql.NullInt64
					trustTS, updatedTS int64
				)
				if rows.Scan(&mac, &intercept, &note, &trust, &trustTS, &updatedTS) != nil {
					continue
				}
				mac = strings.ToLower(mac)
				r := &deviceRow{MAC: mac, Intercept: intercept != 0, Configured: true, Note: note}
				if trust.Valid {
					b := trust.Int64 != 0
					r.CATrust = &b
				}
				if trustTS > 0 {
					r.CATrustAt = time.Unix(trustTS, 0).Format(time.RFC3339)
				}
				if updatedTS > 0 {
					r.UpdatedAt = time.Unix(updatedTS, 0).Format(time.RFC3339)
				}
				byMAC[mac] = r
			}
			rows.Close()
		}
	}

	leases := leaseDetailsFunc()
	kidsNet := kidsSubnet(cfg)
	onlineIP := map[string]string{} // MAC -> IP, for the handshake count
	for ip, l := range leases {
		parsed := net.ParseIP(ip)
		if kidsNet != nil && (parsed == nil || !kidsNet.Contains(parsed)) {
			continue
		}
		r := byMAC[l.MAC]
		if r == nil {
			r = &deviceRow{MAC: l.MAC, Intercept: true}
			byMAC[l.MAC] = r
		}
		r.IP = ip
		r.Online = true
		r.Host = l.Host
		if r.Host == "" && cfg != nil {
			r.Host = cfg.Devices[l.MAC]
		}
		if r.Host == "" {
			r.Host = "unknown"
		}
		onlineIP[l.MAC] = ip
	}

	if db != nil && len(onlineIP) > 0 {
		counts := map[string]int64{}
		rows, err := db.Query(`SELECT src_ip, COUNT(*) FROM events
			WHERE ts>=? AND action='error' AND reason LIKE 'handshake%'
			GROUP BY src_ip`, cutoff)
		if err == nil {
			for rows.Next() {
				var ip string
				var n int64
				if rows.Scan(&ip, &n) == nil {
					counts[ip] = n
				}
			}
			rows.Close()
		}
		for mac, ip := range onlineIP {
			if r := byMAC[mac]; r != nil {
				r.Failures24h = counts[ip]
			}
		}
	}

	out := make([]deviceRow, 0, len(byMAC))
	for _, r := range byMAC {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Online != out[j].Online {
			return out[i].Online
		}
		if out[i].Host != out[j].Host {
			return out[i].Host < out[j].Host
		}
		return out[i].MAC < out[j].MAC
	})
	return out, nil
}

func kidsSubnet(cfg *Config) *net.IPNet {
	subnet := ""
	if cfg != nil {
		subnet = strings.TrimSpace(cfg.Network.KidsSubnet)
	}
	if subnet == "" {
		subnet = "192.168.30.0/24"
	}
	_, n, err := net.ParseCIDR(subnet)
	if err != nil {
		return nil
	}
	return n
}

func normalizeMAC(v string) (string, error) {
	v = strings.ToLower(strings.TrimSpace(v))
	v = strings.ReplaceAll(v, "-", ":")
	if !macRe.MatchString(v) {
		return "", fmt.Errorf("invalid MAC address %q", v)
	}
	return v, nil
}

func parseOnOff(v string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "on", "true", "1", "yes":
		return true, nil
	case "off", "false", "0", "no":
		return false, nil
	}
	return false, fmt.Errorf("expected on or off, got %q", v)
}

// ensureDevicePolicy creates the policy table on a database that predates the
// feature. The proxy creates it too, but the dashboard CLI can be the first
// thing to touch the DB after an upgrade, and a toggle must not fail then.
func ensureDevicePolicy(db *sql.DB) error {
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master
		WHERE type='table' AND name='device_policy'`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS device_policy (
	  mac TEXT PRIMARY KEY, intercept INTEGER NOT NULL DEFAULT 1, note TEXT,
	  ca_trust INTEGER, ca_trust_ts INTEGER, updated_ts INTEGER)`)
	return err
}

// setDevicePolicy writes the inspection switch. noteSet is false when the
// caller did not pass --note, so a dashboard toggle does not erase a note set
// from the command line.
func setDevicePolicy(db *sql.DB, mac string, intercept bool, note string, noteSet bool) error {
	if err := ensureDevicePolicy(db); err != nil {
		return err
	}
	iv, ns := 0, 0
	if intercept {
		iv = 1
	}
	if noteSet {
		ns = 1
	}
	_, err := db.Exec(`INSERT INTO device_policy (mac,intercept,note,updated_ts)
		VALUES (?,?,?,?)
		ON CONFLICT(mac) DO UPDATE SET
		  intercept=excluded.intercept,
		  note=CASE WHEN ? THEN excluded.note ELSE device_policy.note END,
		  updated_ts=excluded.updated_ts`,
		mac, iv, note, time.Now().Unix(), ns)
	return err
}

func deleteDevicePolicy(db *sql.DB, mac string) (bool, error) {
	if err := ensureDevicePolicy(db); err != nil {
		return false, err
	}
	res, err := db.Exec(`DELETE FROM device_policy WHERE mac=?`, mac)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// setCATrustIfChanged records the result of the bootstrap page's trust probe.
// It is idempotent and throttled: a device reloading the page (or a kid posting
// the endpoint repeatedly) cannot flood the DB with writes.
func setCATrustIfChanged(db *sql.DB, mac string, ok bool) (bool, error) {
	v := 0
	if ok {
		v = 1
	}
	now := time.Now().Unix()
	var cur, at sql.NullInt64
	err := db.QueryRow(`SELECT ca_trust, ca_trust_ts FROM device_policy WHERE mac=?`, mac).Scan(&cur, &at)
	if err != nil && err != sql.ErrNoRows {
		return false, err
	}
	if cur.Valid && int(cur.Int64) == v && at.Valid && now-at.Int64 < 3600 {
		return false, nil
	}
	_, err = db.Exec(`INSERT INTO device_policy (mac,intercept,note,ca_trust,ca_trust_ts,updated_ts)
		VALUES (?,1,'',?,?,?)
		ON CONFLICT(mac) DO UPDATE SET
		  ca_trust=excluded.ca_trust,
		  ca_trust_ts=excluded.ca_trust_ts`,
		mac, v, now, now)
	return true, err
}

// openDeviceDB opens the store for the device commands, or returns (nil, nil)
// when it has not been created yet so the dashboard still lists the DHCP
// leases. It ensures the policy table exists so a toggle works on a DB written
// by an older binary.
func openDeviceDB(path string) (*sql.DB, error) {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	db, err := writableDB(path)
	if err != nil {
		return nil, err
	}
	if err := ensureDevicePolicy(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

func deviceCmd(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: kidproxy device list|set|rm [...]")
		os.Exit(2)
	}
	switch args[0] {
	case "list":
		deviceListCmd(args[1:])
	case "set":
		deviceSetCmd(args[1:])
	case "rm":
		deviceRmCmd(args[1:])
	default:
		fmt.Fprintln(os.Stderr, "usage: kidproxy device list|set|rm [...]")
		os.Exit(2)
	}
}

func deviceListCmd(args []string) {
	fs := flag.NewFlagSet("device list", flag.ExitOnError)
	dbPath := fs.String("db", "/var/lib/kidproxy/events.db", "database path")
	configPath := fs.String("config", "", "optional config file (kids_subnet, device names)")
	_ = fs.Parse(args)

	cfg := defaultConfig()
	if *configPath != "" {
		if c, err := loadConfig(*configPath); err == nil {
			cfg = c
		}
	}
	db, err := openDeviceDB(*dbPath)
	if err != nil {
		fail(err)
	}
	if db != nil {
		defer db.Close()
	}
	devices, err := listDevices(db, cfg, time.Now().Add(-24*time.Hour).Unix())
	if err != nil {
		fail(err)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", " ")
	_ = enc.Encode(map[string]any{"devices": devices})
}

func deviceSetCmd(args []string) {
	fs := flag.NewFlagSet("device set", flag.ExitOnError)
	dbPath := fs.String("db", "/var/lib/kidproxy/events.db", "database path")
	macFlag := fs.String("mac", "", "device MAC (aa:bb:cc:dd:ee:ff)")
	interceptFlag := fs.String("intercept", "", "on = decrypt TLS, off = tunnel at the host level")
	note := fs.String("note", "", "free-text note")
	_ = fs.Parse(args)

	mac, err := normalizeMAC(*macFlag)
	if err != nil {
		fail(err)
	}
	if *interceptFlag == "" {
		fail(fmt.Errorf("--intercept on|off is required"))
	}
	intercept, err := parseOnOff(*interceptFlag)
	if err != nil {
		fail(err)
	}
	noteSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "note" {
			noteSet = true
		}
	})

	db, err := writableDB(*dbPath)
	if err != nil {
		fail(err)
	}
	defer db.Close()
	if err := setDevicePolicy(db, mac, intercept, *note, noteSet); err != nil {
		fail(err)
	}
	state := "off"
	if intercept {
		state = "on"
	}
	fmt.Printf("%s: HTTPS inspection %s\n", mac, state)
}

func deviceRmCmd(args []string) {
	fs := flag.NewFlagSet("device rm", flag.ExitOnError)
	dbPath := fs.String("db", "/var/lib/kidproxy/events.db", "database path")
	macFlag := fs.String("mac", "", "device MAC")
	_ = fs.Parse(args)

	mac, err := normalizeMAC(*macFlag)
	if err != nil {
		fail(err)
	}
	db, err := writableDB(*dbPath)
	if err != nil {
		fail(err)
	}
	defer db.Close()
	ok, err := deleteDevicePolicy(db, mac)
	if err != nil {
		fail(err)
	}
	if !ok {
		fail(fmt.Errorf("no policy for %s", mac))
	}
	fmt.Printf("%s: back to the default (inspect)\n", mac)
}
