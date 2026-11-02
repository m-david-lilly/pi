package main

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func TestPolicyDefaultsToIntercept(t *testing.T) {
	db := testDB(t)
	p := &Policy{db: db}
	p.reload()

	if !p.Intercept("") {
		t.Error("empty MAC should be inspected")
	}
	if !p.Intercept("aa:bb:cc:dd:ee:05") {
		t.Error("unknown device should be inspected")
	}

	mac := "aa:bb:cc:dd:ee:05"
	if err := setDevicePolicy(db, mac, false, "cannot install the CA", true); err != nil {
		t.Fatalf("setDevicePolicy: %v", err)
	}
	p.reload()
	if p.Intercept(mac) {
		t.Error("device policy off was not applied")
	}

	if ok, err := deleteDevicePolicy(db, mac); err != nil || !ok {
		t.Fatalf("deleteDevicePolicy = %v, %v", ok, err)
	}
	p.reload()
	if !p.Intercept(mac) {
		t.Error("removing the policy did not restore the default")
	}
}

func TestDevicePolicyKeepsNoteWhenNotPassed(t *testing.T) {
	db := testDB(t)
	mac := "aa:bb:cc:dd:ee:06"
	if err := setDevicePolicy(db, mac, false, "old tablet", true); err != nil {
		t.Fatalf("setDevicePolicy: %v", err)
	}
	if err := setDevicePolicy(db, mac, true, "", false); err != nil {
		t.Fatalf("setDevicePolicy (toggle): %v", err)
	}
	var note string
	if err := db.QueryRow(`SELECT note FROM device_policy WHERE mac=?`, mac).Scan(&note); err != nil {
		t.Fatalf("select: %v", err)
	}
	if note != "old tablet" {
		t.Errorf("note = %q, want it preserved", note)
	}
}

func TestSetCATrustIfChanged(t *testing.T) {
	db := testDB(t)
	mac := "aa:bb:cc:dd:ee:07"
	if err := setDevicePolicy(db, mac, false, "", false); err != nil {
		t.Fatalf("setDevicePolicy: %v", err)
	}

	changed, err := setCATrustIfChanged(db, mac, false)
	if err != nil || !changed {
		t.Fatalf("first probe = %v, %v; want true", changed, err)
	}
	// The same answer within the hour is throttled.
	changed, err = setCATrustIfChanged(db, mac, false)
	if err != nil || changed {
		t.Fatalf("repeat probe = %v, %v; want throttled", changed, err)
	}
	// A different answer is always recorded.
	changed, err = setCATrustIfChanged(db, mac, true)
	if err != nil || !changed {
		t.Fatalf("changed probe = %v, %v; want true", changed, err)
	}

	var (
		intercept int
		trust     int
	)
	if err := db.QueryRow(`SELECT intercept, ca_trust FROM device_policy WHERE mac=?`, mac).
		Scan(&intercept, &trust); err != nil {
		t.Fatalf("select: %v", err)
	}
	if intercept != 0 {
		t.Error("the trust probe changed the intercept policy")
	}
	if trust != 1 {
		t.Errorf("ca_trust = %d, want 1", trust)
	}
}

func TestListDevicesMergesPolicyAndLeases(t *testing.T) {
	db := testDB(t)
	tablet := "aa:bb:cc:dd:ee:11"
	phone := "aa:bb:cc:dd:ee:22"

	if err := setDevicePolicy(db, tablet, false, "", false); err != nil {
		t.Fatalf("setDevicePolicy: %v", err)
	}
	if _, err := setCATrustIfChanged(db, tablet, false); err != nil {
		t.Fatalf("setCATrustIfChanged: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO events (ts,src_ip,device,action,reason)
		VALUES (?,?,?,?,?)`, time.Now().Unix(), "192.168.30.22", "phone",
		"error", "handshake: remote error: tls: unknown certificate authority"); err != nil {
		t.Fatalf("insert event: %v", err)
	}

	old := leaseDetailsFunc
	leaseDetailsFunc = func() map[string]leaseInfo {
		return map[string]leaseInfo{
			"192.168.30.11": {MAC: tablet, Host: "Tablet"},
			"192.168.30.22": {MAC: phone, Host: "Phone"},
			"192.168.1.50":  {MAC: "aa:bb:cc:dd:ee:33", Host: "AdultLaptop"},
		}
	}
	defer func() { leaseDetailsFunc = old }()

	devices, err := listDevices(db, defaultConfig(), time.Now().Add(-24*time.Hour).Unix())
	if err != nil {
		t.Fatalf("listDevices: %v", err)
	}
	if len(devices) != 2 {
		t.Fatalf("devices = %d, want 2 (adult LAN must be excluded): %+v", len(devices), devices)
	}
	byMAC := map[string]deviceRow{}
	for _, d := range devices {
		byMAC[d.MAC] = d
	}

	t1 := byMAC[tablet]
	if !t1.Online || t1.IP != "192.168.30.11" || t1.Host != "Tablet" {
		t.Errorf("tablet lease fields = %+v", t1)
	}
	if !t1.Configured || t1.Intercept {
		t.Errorf("tablet policy not reflected: %+v", t1)
	}
	if t1.CATrust == nil || *t1.CATrust {
		t.Errorf("tablet CA trust = %v, want false", t1.CATrust)
	}

	t2 := byMAC[phone]
	if !t2.Online || t2.Configured || !t2.Intercept {
		t.Errorf("phone defaults = %+v", t2)
	}
	if t2.Failures24h != 1 {
		t.Errorf("phone handshake failures = %d, want 1", t2.Failures24h)
	}
}

func TestDeviceInterceptUsesLeaseMAC(t *testing.T) {
	db := testDB(t)
	mac := "aa:bb:cc:dd:ee:44"
	pol := &Policy{db: db}
	pol.reload()
	st := &Store{leases: map[string]string{"192.168.30.44": mac}, leaseAt: time.Now()}
	p := &Proxy{store: st, policy: pol, cfg: &Config{}}

	if !p.deviceIntercept("192.168.30.44") {
		t.Error("device with no policy row should be inspected")
	}
	if err := setDevicePolicy(db, mac, false, "", false); err != nil {
		t.Fatalf("setDevicePolicy: %v", err)
	}
	pol.reload()
	if p.deviceIntercept("192.168.30.44") {
		t.Error("policy off not applied to the leased MAC")
	}
	if !p.deviceIntercept("192.168.30.99") {
		t.Error("an IP without a lease should default to inspect")
	}
}

// TestDevicePolicyCreatesTableOnOldDB: the dashboard can be upgraded before the
// proxy has run, so a toggle must create the table rather than fail.
func TestDevicePolicyCreatesTableOnOldDB(t *testing.T) {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "old.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE events (ts INTEGER)`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	mac := "aa:bb:cc:dd:ee:08"
	if err := setDevicePolicy(db, mac, false, "old db", true); err != nil {
		t.Fatalf("setDevicePolicy: %v", err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM device_policy WHERE mac=?`, mac).Scan(&n); err != nil {
		t.Fatalf("select: %v", err)
	}
	if n != 1 {
		t.Errorf("rows = %d, want 1", n)
	}
}

func TestNormalizeMAC(t *testing.T) {
	got, err := normalizeMAC(" AA-BB-CC-DD-EE-FF ")
	if err != nil || got != "aa:bb:cc:dd:ee:ff" {
		t.Fatalf("normalizeMAC = %q, %v", got, err)
	}
	if _, err := normalizeMAC("not-a-mac"); err == nil {
		t.Error("invalid MAC accepted")
	}
}
