package main

import (
	"bufio"
	"crypto/x509"
	"database/sql"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHostMatches(t *testing.T) {
	cases := []struct {
		host, pattern string
		want          bool
	}{
		{"example.com", "example.com", true},
		{"www.example.com", "example.com", true},
		{"a.b.example.com", "example.com", true},
		{"badexample.com", "example.com", false},
		{"example.com.evil.net", "example.com", false},
		{"example.com.", "example.com", true},
		{"WWW.Example.COM", "example.com", true},
		{"example.com", "*.example.com", true},
		{"a.example.com", "*.example.com", true},
		{"badexample.com", "*.example.com", false},
		{"example.com", "*.com", true},
		{"anything", "", false},
		{"dns.google", "dns.google", true},
		{"foo.dns.google", "dns.google", true},
		{"notdns.google", "dns.google", false},
	}
	for _, c := range cases {
		if got := hostMatches(c.host, c.pattern); got != c.want {
			t.Errorf("hostMatches(%q, %q) = %v, want %v", c.host, c.pattern, got, c.want)
		}
	}
}

func TestParseYouTubeURLs(t *testing.T) {
	cases := []struct {
		url  string
		want string
	}{
		{"https://www.youtube.com/watch?v=dQw4w9WgXcQ", "dQw4w9WgXcQ"},
		{"https://youtube.com/watch?v=abc123&t=30", "abc123"},
		{"https://youtu.be/xyz789", "xyz789"},
		{"https://youtu.be/xyz789?t=10", "xyz789"},
		{"https://www.youtube.com/shorts/short1", "short1"},
		{"https://www.youtube.com/live/live1?feature=share", "live1"},
	}
	for _, c := range cases {
		yt := parseYouTube(c.url, nil, nil)
		if yt == nil {
			t.Errorf("parseYouTube(%q) = nil, want %q", c.url, c.want)
			continue
		}
		if yt.VideoID != c.want {
			t.Errorf("parseYouTube(%q).VideoID = %q, want %q", c.url, yt.VideoID, c.want)
		}
	}
}

func TestParseYouTubeIgnoresOtherHosts(t *testing.T) {
	for _, u := range []string{
		"https://example.com/watch?v=abc",
		"https://notyoutube.com/watch?v=abc",
		"https://www.youtube.com/feed/subscriptions",
	} {
		if yt := parseYouTube(u, nil, nil); yt != nil {
			t.Errorf("parseYouTube(%q) = %+v, want nil", u, yt)
		}
	}
}

func TestParseYouTubePlayerResponse(t *testing.T) {
	body := []byte(`{"videoDetails":{"videoId":"vid42","title":"A Video","author":"A Channel","lengthSeconds":"213"}}`)
	yt := parseYouTube("https://www.youtube.com/youtubei/v1/player?key=x", nil, body)
	if yt == nil {
		t.Fatal("parseYouTube returned nil")
	}
	if yt.VideoID != "vid42" || yt.Title != "A Video" || yt.Channel != "A Channel" || yt.Seconds != 213 {
		t.Errorf("unexpected metadata: %+v", yt)
	}
}

func TestParseYouTubeBadJSON(t *testing.T) {
	yt := parseYouTube("https://www.youtube.com/youtubei/v1/player", nil, []byte(`{not json`))
	if yt != nil {
		t.Errorf("expected nil for bad JSON, got %+v", yt)
	}
}

func TestParseSearch(t *testing.T) {
	cases := []struct {
		name   string
		url    string
		body   []byte
		engine string
		query  string
	}{
		{"google", "https://www.google.com/search?q=how+to+code", nil, "google", "how to code"},
		{"google country", "https://www.google.co.uk/search?q=cats", nil, "google", "cats"},
		{"bing", "https://www.bing.com/search?q=dogs", nil, "bing", "dogs"},
		{"ddg", "https://duckduckgo.com/?q=birds&ia=web", nil, "duckduckgo", "birds"},
		{"brave", "https://search.brave.com/search?q=fish", nil, "brave", "fish"},
		{"youtube html", "https://www.youtube.com/results?search_query=music", nil, "youtube", "music"},
		{"youtube innertube", "https://www.youtube.com/youtubei/v1/search?key=x",
			[]byte(`{"query":"space videos"}`), "youtube", "space videos"},
	}
	for _, c := range cases {
		s := parseSearch(c.url, c.body)
		if s == nil {
			t.Errorf("%s: parseSearch(%q) = nil", c.name, c.url)
			continue
		}
		if s.Engine != c.engine || s.Query != c.query {
			t.Errorf("%s: got %+v, want engine=%q query=%q", c.name, s, c.engine, c.query)
		}
	}
}

func TestParseSearchIgnoresNonSearch(t *testing.T) {
	for _, u := range []string{
		"https://example.com/?q=hello",
		"https://www.google.com/maps?q=coffee",
		"https://www.youtube.com/watch?v=abc",
	} {
		if s := parseSearch(u, nil); s != nil {
			t.Errorf("parseSearch(%q) = %+v, want nil", u, s)
		}
	}
}

func TestExtractTextHTML(t *testing.T) {
	body := []byte(`<html><head><title>T</title><style>.x{color:red}</style>
<script>var secret = "not text";</script></head>
<body><h1>Hello</h1><p>World   of
spans</p><noscript>ignored</noscript></body></html>`)
	got := extractText("text/html; charset=utf-8", body, 4096)
	if !strings.Contains(got, "Hello") || !strings.Contains(got, "World of spans") {
		t.Errorf("missing visible text: %q", got)
	}
	for _, bad := range []string{"not text", "color:red", "ignored"} {
		if strings.Contains(got, bad) {
			t.Errorf("extracted hidden text %q in %q", bad, got)
		}
	}
	if strings.Contains(got, "  ") || strings.Contains(got, "\n") {
		t.Errorf("whitespace not collapsed: %q", got)
	}
}

func TestExtractTextLimit(t *testing.T) {
	body := []byte(strings.Repeat("a", 100))
	if got := extractText("text/plain", body, 10); len(got) != 10 {
		t.Errorf("len = %d, want 10", len(got))
	}
	if got := extractText("text/plain", nil, 10); got != "" {
		t.Errorf("empty body = %q, want empty", got)
	}
	if got := extractText("text/plain", body, 0); got != "" {
		t.Errorf("zero limit = %q, want empty", got)
	}
}

func TestExtractTextJSONPassesThrough(t *testing.T) {
	got := extractText("application/json", []byte("{\"a\": 1}"), 100)
	if got != `{"a": 1}` {
		t.Errorf("got %q", got)
	}
}

func TestAtoi(t *testing.T) {
	cases := map[string]int{"": 0, "0": 0, "42": 42, "007": 7, "12a": 12, "-5": 0, "99999999999999": 0}
	for in, want := range cases {
		if got := atoi(in); got != want {
			t.Errorf("atoi(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestConfigSeverity(t *testing.T) {
	c := defaultConfig()
	c.Notify.Email.MinSeverity = "warn"
	for sev, want := range map[string]bool{"info": false, "warn": true, "high": true, "": false, "bogus": false} {
		if got := c.severityAtLeast(sev); got != want {
			t.Errorf("severityAtLeast(%q) = %v, want %v", sev, got, want)
		}
	}
}

func TestConfigLateNight(t *testing.T) {
	c := defaultConfig()
	c.Alert.LateNight = true
	c.Alert.LateNightFrom = 23
	c.Alert.LateNightTo = 6
	at := func(h, m int) time.Time { return time.Date(2026, 10, 30, h, m, 0, 0, time.UTC) }
	for _, tc := range []struct {
		h, m int
		want bool
	}{
		{22, 59, false}, {23, 0, true}, {23, 59, true},
		{0, 0, true}, {5, 59, true}, {6, 0, false}, {12, 0, false},
	} {
		if got := c.isLateNight(at(tc.h, tc.m)); got != tc.want {
			t.Errorf("isLateNight(%02d:%02d) = %v, want %v", tc.h, tc.m, got, tc.want)
		}
	}
	c.Alert.LateNight = false
	if c.isLateNight(at(2, 0)) {
		t.Error("disabled late-night alert still fired")
	}
	c.Alert.LateNight = true
	c.Alert.LateNightFrom, c.Alert.LateNightTo = 9, 9
	if c.isLateNight(at(9, 0)) {
		t.Error("empty late-night window fired")
	}
}

func TestConfigCaptureTypes(t *testing.T) {
	c := defaultConfig()
	for _, ct := range []string{"text/html", "text/html; charset=utf-8", "TEXT/PLAIN", "application/json"} {
		if !c.isCapturableType(ct) {
			t.Errorf("isCapturableType(%q) = false, want true", ct)
		}
	}
	for _, ct := range []string{"image/png", "video/mp4", "application/octet-stream", ""} {
		if c.isCapturableType(ct) {
			t.Errorf("isCapturableType(%q) = true, want false", ct)
		}
	}
}

func TestConfigSkipHost(t *testing.T) {
	c := defaultConfig()
	for _, h := range []string{"googlevideo.com", "r1.googlevideo.com", "i.ytimg.com"} {
		if !c.skipHost(h) {
			t.Errorf("skipHost(%q) = false, want true", h)
		}
	}
	for _, h := range []string{"example.com", "notgooglevideo.com"} {
		if c.skipHost(h) {
			t.Errorf("skipHost(%q) = true, want false", h)
		}
	}
}

func TestRulesBlocked(t *testing.T) {
	cfg := defaultConfig()
	cfg.Block.Domains = []string{"dns.google", "*.example.com"}
	r := &Rules{cfg: cfg}
	r.reload()

	for _, h := range []string{"dns.google", "foo.dns.google", "example.com", "a.example.com", "A.EXAMPLE.COM."} {
		if ok, _ := r.blocked(h); !ok {
			t.Errorf("blocked(%q) = false, want true", h)
		}
	}
	for _, h := range []string{"example.org", "notdns.google", "google.com"} {
		if ok, _ := r.blocked(h); ok {
			t.Errorf("blocked(%q) = true, want false", h)
		}
	}
}

func TestRulesKeyword(t *testing.T) {
	cfg := defaultConfig()
	cfg.Block.Keywords = []string{"Send Nudes", "self harm"}
	r := &Rules{cfg: cfg}
	r.reload()

	if kw, ok := r.keywordHit("please SEND NUDES now"); !ok || kw != "send nudes" {
		t.Errorf("keywordHit = %q, %v", kw, ok)
	}
	if _, ok := r.keywordHit("nothing to see here"); ok {
		t.Error("keywordHit matched unrelated text")
	}
}

// buildClientHello hand-encodes a minimal ClientHello so the SNI parser can be
// tested without a live TLS peer.
func buildClientHello(sni string) []byte {
	var ext []byte
	if sni != "" {
		name := []byte(sni)
		var list []byte
		list = append(list, 0x00) // host_name
		list = binary.BigEndian.AppendUint16(list, uint16(len(name)))
		list = append(list, name...)

		var e []byte
		e = binary.BigEndian.AppendUint16(e, 0x0000)
		e = binary.BigEndian.AppendUint16(e, uint16(len(list)+2))
		e = binary.BigEndian.AppendUint16(e, uint16(len(list)))
		e = append(e, list...)
		ext = append(ext, e...)
	}

	var body []byte
	body = append(body, 0x03, 0x03)                              // legacy version
	body = append(body, make([]byte, 32)...)                     // random
	body = append(body, 0x00)                                    // session id len
	body = binary.BigEndian.AppendUint16(body, 2)                // cipher suites len
	body = binary.BigEndian.AppendUint16(body, 0x1301)           // TLS_AES_128_GCM_SHA256
	body = append(body, 0x01, 0x00)                              // compression: null
	body = binary.BigEndian.AppendUint16(body, uint16(len(ext))) // extensions len
	body = append(body, ext...)

	hs := []byte{0x01}
	hs = append(hs, byte(len(body)>>16), byte(len(body)>>8), byte(len(body)))
	hs = append(hs, body...)

	rec := []byte{0x16, 0x03, 0x01}
	rec = binary.BigEndian.AppendUint16(rec, uint16(len(hs)))
	return append(rec, hs...)
}

func TestParseClientHello(t *testing.T) {
	rec := buildClientHello("www.example.com")
	if rec[0] != 0x16 {
		t.Fatal("helper did not build a handshake record")
	}
	sni, err := parseClientHello(rec[5:])
	if err != nil {
		t.Fatalf("parseClientHello: %v", err)
	}
	if sni != "www.example.com" {
		t.Errorf("sni = %q", sni)
	}
}

func TestParseClientHelloNoSNI(t *testing.T) {
	rec := buildClientHello("")
	if _, err := parseClientHello(rec[5:]); err == nil {
		t.Error("expected error for ClientHello without SNI")
	}
	if _, err := parseClientHello([]byte{0x02, 0x00, 0x00, 0x00}); err == nil {
		t.Error("expected error for non-ClientHello handshake type")
	}
	if _, err := parseClientHello([]byte{0x01, 0xff, 0xff, 0xff}); err == nil {
		t.Error("expected error for truncated ClientHello")
	}
}

func TestCALeafSignedByCA(t *testing.T) {
	dir := t.TempDir()
	if err := initCA(dir); err != nil {
		t.Fatalf("initCA: %v", err)
	}
	ca, err := loadCA(filepath.Join(dir, "ca.crt"), filepath.Join(dir, "ca.key"), 7)
	if err != nil {
		t.Fatalf("loadCA: %v", err)
	}
	if got := ca.cert.Subject.CommonName; got != "Kids Filter CA" {
		t.Errorf("CA CN = %q", got)
	}

	leaf, err := ca.leaf("example.com")
	if err != nil {
		t.Fatalf("leaf: %v", err)
	}
	parsed, err := x509.ParseCertificate(leaf.Certificate[0])
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}
	if got := parsed.Issuer.CommonName; got != "Kids Filter CA" {
		t.Errorf("leaf issuer = %q, want Kids Filter CA", got)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca.cert)
	if _, err := parsed.Verify(x509.VerifyOptions{DNSName: "example.com", Roots: roots}); err != nil {
		t.Errorf("leaf does not verify against the CA: %v", err)
	}

	ipLeaf, err := ca.leaf("192.168.30.1")
	if err != nil {
		t.Fatalf("ip leaf: %v", err)
	}
	ipParsed, err := x509.ParseCertificate(ipLeaf.Certificate[0])
	if err != nil {
		t.Fatalf("parse ip leaf: %v", err)
	}
	if _, err := ipParsed.Verify(x509.VerifyOptions{Roots: roots}); err != nil {
		t.Errorf("ip leaf does not verify: %v", err)
	}
	if len(ipParsed.IPAddresses) != 1 || ipParsed.IPAddresses[0].String() != "192.168.30.1" {
		t.Errorf("ip SANs = %v", ipParsed.IPAddresses)
	}
}

// TestPlainHTTPReplaysPeekedBytes pins the fix for a transparent-mode hang:
// handleConn Peeks the first byte (which buffers the whole request) and then
// hands the connection to the HTTP server. Pushing the raw conn dropped those
// buffered bytes, so every plain-HTTP request stalled until it timed out.
func TestPlainHTTPReplaysPeekedBytes(t *testing.T) {
	old := originalDstFunc
	originalDstFunc = func(net.Conn) (string, int, error) { return "192.168.30.1", 80, nil }
	defer func() { originalDstFunc = old }()

	dir := t.TempDir()
	if err := initCA(dir); err != nil {
		t.Fatalf("initCA: %v", err)
	}
	ca, err := loadCA(filepath.Join(dir, "ca.crt"), filepath.Join(dir, "ca.key"), 7)
	if err != nil {
		t.Fatalf("loadCA: %v", err)
	}
	cfg := &Config{}
	cfg.Network.Gateway = "192.168.30.1"
	p := newProxy(cfg, ca, nil, nil)
	go func() { _ = p.srv.Serve(p.queue) }()
	defer func() { _ = p.srv.Close() }()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err == nil {
			p.handleConn(c, true)
		}
	}()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: 192.168.30.1\r\n\r\n"); err != nil {
		t.Fatalf("write request: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "Kids network setup") {
		t.Errorf("bootstrap page not served, got %d bytes", len(body))
	}
}

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := writableDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(schema); err != nil {
		t.Fatalf("schema: %v", err)
	}
	return db
}

func TestRulesAllowOverridesBlock(t *testing.T) {
	db := testDB(t)
	cfg := &Config{}
	cfg.Block.Domains = []string{"example.com"}
	r := newRules(cfg, db)

	if ok, _ := r.blocked("www.example.com"); !ok {
		t.Fatal("config block not applied")
	}
	if _, err := insertRule(db, "domain", "allow", "example.com", "parent approved"); err != nil {
		t.Fatalf("insertRule: %v", err)
	}
	r.reload()
	if ok, note := r.blocked("www.example.com"); ok {
		t.Errorf("allow rule did not override the block (note %q)", note)
	}
	if ok, _ := r.blocked("unrelated.test"); ok {
		t.Error("unrelated host blocked")
	}

	// Removing the allow rule restores the block.
	if _, err := insertRule(db, "domain", "block", "other.test", ""); err != nil {
		t.Fatalf("insertRule: %v", err)
	}
	r.reload()
	if ok, _ := r.blocked("other.test"); !ok {
		t.Error("dashboard block rule not applied")
	}
}

func TestInsertDeleteRule(t *testing.T) {
	db := testDB(t)
	id, err := insertRule(db, "domain", "block", "bad.test", "test note")
	if err != nil {
		t.Fatalf("insertRule: %v", err)
	}
	var value, note string
	if err := db.QueryRow(`SELECT value, note FROM rules WHERE id=?`, id).Scan(&value, &note); err != nil {
		t.Fatalf("select: %v", err)
	}
	if value != "bad.test" || note != "test note" {
		t.Errorf("stored %q / %q", value, note)
	}
	ok, err := deleteRule(db, id)
	if err != nil || !ok {
		t.Fatalf("deleteRule = %v, %v", ok, err)
	}
	if ok, _ := deleteRule(db, id); ok {
		t.Error("second delete reported a row")
	}
}

func TestCollectStats(t *testing.T) {
	db := testDB(t)
	now := time.Now().Unix()
	for _, ts := range []int64{now - 60, now - 120, now - 86400*3} {
		if _, err := db.Exec(`INSERT INTO events(ts,device,host,url,action) VALUES(?,?,?,?,?)`,
			ts, "kid-tablet", "example.com", "https://example.com/", "bump"); err != nil {
			t.Fatalf("insert event: %v", err)
		}
	}
	if _, err := db.Exec(`INSERT INTO alerts(ts,severity,kind,device,host,detail) VALUES(?,?,?,?,?,?)`,
		now-30, "high", "keyword", "kid-tablet", "example.com", "matched keyword: x"); err != nil {
		t.Fatalf("insert alert: %v", err)
	}
	if _, err := insertRule(db, "domain", "block", "bad.test", ""); err != nil {
		t.Fatalf("insertRule: %v", err)
	}

	st, err := collectStats(db, now-3600)
	if err != nil {
		t.Fatalf("collectStats: %v", err)
	}
	if st.Events24h != 2 {
		t.Errorf("events_24h = %d, want 2", st.Events24h)
	}
	if st.Alerts24h != 1 {
		t.Errorf("alerts_24h = %d, want 1", st.Alerts24h)
	}
	if st.Devices24h != 1 {
		t.Errorf("devices_24h = %d, want 1", st.Devices24h)
	}
	if st.Rules != 1 {
		t.Errorf("rules = %d, want 1", st.Rules)
	}
	if st.LastEvent == "" {
		t.Error("last_event empty")
	}
}

func TestAllowAlertDedupesPerHour(t *testing.T) {
	s := &Store{}
	now := time.Now()
	if !s.allowAlert("tls-pin|a.test|tablet", now) {
		t.Fatal("first alert should pass")
	}
	if s.allowAlert("tls-pin|a.test|tablet", now.Add(30*time.Minute)) {
		t.Error("repeat within the hour should be suppressed")
	}
	if !s.allowAlert("tls-pin|a.test|tablet", now.Add(61*time.Minute)) {
		t.Error("alert after an hour should pass")
	}
	if !s.allowAlert("tls-pin|b.test|tablet", now.Add(30*time.Minute)) {
		t.Error("different host should alert")
	}
}

func TestDeleteAlerts(t *testing.T) {
	db := testDB(t)
	now := time.Now().Unix()
	for _, kind := range []string{"tls-pin", "tls-pin", "blocked"} {
		if _, err := db.Exec(`INSERT INTO alerts(ts,severity,kind,device,host,detail) VALUES(?,?,?,?,?,?)`,
			now, "info", kind, "tablet", "example.com", ""); err != nil {
			t.Fatalf("insert alert: %v", err)
		}
	}
	n, err := deleteAlerts(db, "tls-pin")
	if err != nil {
		t.Fatalf("deleteAlerts: %v", err)
	}
	if n != 2 {
		t.Errorf("deleted %d tls-pin alerts, want 2", n)
	}
	var left int64
	if err := db.QueryRow(`SELECT COUNT(*) FROM alerts`).Scan(&left); err != nil {
		t.Fatalf("count: %v", err)
	}
	if left != 1 {
		t.Errorf("alerts left = %d, want 1", left)
	}
	if _, err := deleteAlerts(db, ""); err != nil {
		t.Fatalf("deleteAlerts all: %v", err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM alerts`).Scan(&left); err != nil {
		t.Fatalf("count: %v", err)
	}
	if left != 0 {
		t.Errorf("alerts left after clear = %d", left)
	}
}
