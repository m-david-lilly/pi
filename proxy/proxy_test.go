package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/binary"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
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

func TestParseYouTubeRichPlayerResponse(t *testing.T) {
	body := []byte(`{
	 "videoDetails":{"videoId":"dQw4w9WgXcQ","title":"Never Gonna Give You Up",
	  "author":"Rick Astley","channelId":"UCuAX","lengthSeconds":"213",
	  "keywords":["rick astley","80s music"],"shortDescription":"Official video",
	  "viewCount":"1832600234","isLiveContent":false,
	  "thumbnail":{"thumbnails":[
	   {"url":"https://i.ytimg.com/vi/dQw4w9WgXcQ/hq.jpg","width":480,"height":360},
	   {"url":"https://i.ytimg.com/vi/dQw4w9WgXcQ/max.jpg","width":1280,"height":720}]}},
	 "microformat":{"playerMicroformatRenderer":{"lengthSeconds":213,"category":"Music",
	  "publishDate":"2009-10-24T23:57:33-07:00","uploadDate":"2009-10-25T00:00:00-07:00",
	  "viewCount":1832600234,"isFamilySafe":true,"ownerChannelName":"Rick Astley",
	  "externalChannelId":"UCuAX","thumbnail":{"thumbnails":[
	   {"url":"https://i.ytimg.com/vi/dQw4w9WgXcQ/maxresdefault.jpg","width":1280,"height":720}]}}}
	}`)
	yt := parseYouTube("https://www.youtube.com/youtubei/v1/player?prettyPrint=false",
		[]byte(`{"videoId":"dQw4w9WgXcQ"}`), body)
	if yt == nil {
		t.Fatal("parseYouTube returned nil")
	}
	if yt.VideoID != "dQw4w9WgXcQ" || yt.Title != "Never Gonna Give You Up" || yt.Channel != "Rick Astley" {
		t.Errorf("identity: %+v", yt)
	}
	if yt.ChannelID != "UCuAX" || yt.Seconds != 213 || yt.Category != "Music" {
		t.Errorf("details: %+v", yt)
	}
	if len(yt.Tags) != 2 || yt.Tags[0] != "rick astley" || yt.Tags[1] != "80s music" {
		t.Errorf("tags: %+v", yt.Tags)
	}
	if yt.Description != "Official video" || yt.PublishDate != "2009-10-24T23:57:33-07:00" || yt.ViewCount != 1832600234 {
		t.Errorf("metadata: %+v", yt)
	}
	if yt.FamilySafe == nil || !*yt.FamilySafe || yt.Live {
		t.Errorf("flags: %+v", yt)
	}
	if !strings.HasSuffix(yt.Thumbnail, "/max.jpg") {
		t.Errorf("thumbnail = %q, want the largest", yt.Thumbnail)
	}
	if yt.WatchURL != "https://www.youtube.com/watch?v=dQw4w9WgXcQ" {
		t.Errorf("watch URL = %q", yt.WatchURL)
	}
}

func TestParseYouTubeWatchPage(t *testing.T) {
	body := []byte(`<html><head>
<meta itemprop="name" content="A &amp; B">
<meta itemprop="description" content="Desc">
<meta itemprop="genre" content="Gaming">
<meta name="keywords" content="minecraft, gaming, lets play">
<meta itemprop="datePublished" content="2020-01-02">
<meta itemprop="duration" content="PT1H2M3S">
<meta itemprop="isFamilyFriendly" content="true">
<meta itemprop="identifier" content="watchid123">
<meta property="og:image" content="https://i.ytimg.com/vi/watchid123/maxresdefault.jpg">
</head><body><script>var ytInitialPlayerResponse = {"videoDetails":{"videoId":"watchid123","title":"A } B","author":"Chan","lengthSeconds":"3723"},"microformat":{"playerMicroformatRenderer":{"category":"Gaming","publishDate":"2020-01-02","isFamilySafe":true}}};</script></body></html>`)
	yt := parseYouTube("https://www.youtube.com/watch?v=watchid123", nil, body)
	if yt == nil {
		t.Fatal("parseYouTube returned nil")
	}
	if yt.VideoID != "watchid123" || yt.Title != "A } B" || yt.Channel != "Chan" {
		t.Errorf("embedded player metadata: %+v", yt)
	}
	if yt.Category != "Gaming" || yt.Seconds != 3723 {
		t.Errorf("details: %+v", yt)
	}
	if len(yt.Tags) != 3 || yt.Tags[2] != "lets play" {
		t.Errorf("tags: %+v", yt.Tags)
	}
	if yt.Description != "Desc" || yt.PublishDate != "2020-01-02" {
		t.Errorf("meta fallback: %+v", yt)
	}
	if yt.FamilySafe == nil || !*yt.FamilySafe {
		t.Errorf("family safe: %+v", yt.FamilySafe)
	}
	if !strings.HasSuffix(yt.Thumbnail, "maxresdefault.jpg") {
		t.Errorf("thumbnail = %q", yt.Thumbnail)
	}
}

func TestParseISODuration(t *testing.T) {
	for in, want := range map[string]int{
		"PT45S":    45,
		"PT3M34S":  214,
		"PT1H2M3S": 3723,
		"P1DT2H":   93600,
		"nonsense": 0,
		"PT0S":     0,
	} {
		if got := parseISODuration(in); got != want {
			t.Errorf("parseISODuration(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestWantsYouTubeMetadata(t *testing.T) {
	for _, c := range []struct {
		host, path string
		want       bool
	}{
		{"www.youtube.com", "/watch", true},
		{"m.youtube.com", "/shorts/abc", true},
		{"www.youtube.com", "/youtubei/v1/player", true},
		{"www.youtube.com", "/feed/subscriptions", false},
		{"example.com", "/watch", false},
	} {
		if got := wantsYouTubeMetadata(c.host, c.path); got != c.want {
			t.Errorf("wantsYouTubeMetadata(%q, %q) = %v, want %v", c.host, c.path, got, c.want)
		}
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

func TestConfigLateNightTimezone(t *testing.T) {
	c := defaultConfig()
	c.Alert.LateNightFrom, c.Alert.LateNightTo = 23, 6
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	c.loc = loc
	// 07:00 UTC on 2026-10-30 is midnight PDT: late night locally.
	if !c.isLateNight(time.Date(2026, 10, 30, 7, 0, 0, 0, time.UTC)) {
		t.Error("midnight local time not treated as late night")
	}
	// 23:30 UTC is 16:30 PDT: afternoon locally.
	if c.isLateNight(time.Date(2026, 10, 30, 23, 30, 0, 0, time.UTC)) {
		t.Error("afternoon local time treated as late night")
	}
}

func TestParseSince(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"24h": 24 * time.Hour, "10m": 10 * time.Minute, "7d": 7 * 24 * time.Hour, "0d": 0,
	} {
		got, err := parseSince(in)
		if err != nil || got != want {
			t.Errorf("parseSince(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "7days", "d", "-1d", "soon"} {
		if _, err := parseSince(in); err == nil {
			t.Errorf("parseSince(%q) succeeded, want error", in)
		}
	}
}

func TestDenyPrivate(t *testing.T) {
	for _, addr := range []string{
		"127.0.0.1:8080", "192.168.1.1:80", "10.0.0.5:443", "172.16.3.4:80",
		"169.254.1.1:80", "0.0.0.0:80", "[::1]:443", "[fd00::1]:443", "[fe80::1]:80",
	} {
		if err := denyPrivate("tcp", addr, nil); err == nil {
			t.Errorf("denyPrivate(%q) allowed a private/local address", addr)
		}
	}
	for _, addr := range []string{"93.184.215.14:443", "[2606:2800:21f:cb07:6820:80da:af6b:8b2c]:443"} {
		if err := denyPrivate("tcp", addr, nil); err != nil {
			t.Errorf("denyPrivate(%q) = %v, want nil", addr, err)
		}
	}
}

func TestWSDialAddr(t *testing.T) {
	for _, c := range []struct{ host, scheme, want string }{
		{"example.com", "https", "example.com:443"},
		{"example.com", "http", "example.com:80"},
		{"example.com:8443", "https", "example.com:8443"},
		{"[2001:db8::1]", "https", "[2001:db8::1]:443"},
	} {
		if got := wsDialAddr(c.host, c.scheme); got != c.want {
			t.Errorf("wsDialAddr(%q, %q) = %q, want %q", c.host, c.scheme, got, c.want)
		}
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

// TestBootstrapNamesTheLoadedCA: the install page and the iOS profile must show
// the CA's own name, because the router can sign with the Pi Home CA (which
// also signs the dashboard cert) instead of the generated "Kids Filter CA".
func TestBootstrapNamesTheLoadedCA(t *testing.T) {
	cert := &x509.Certificate{
		SerialNumber: big.NewInt(0x541b3904),
		Subject:      pkix.Name{CommonName: "Pi Home CA"},
	}
	ca := &CA{cert: cert}

	page := bootstrapPage(ca, "setup.kids")
	if !strings.Contains(page, "<b>Pi Home CA</b>") {
		t.Error("bootstrap page does not name the loaded CA")
	}
	if strings.Contains(page, "Kids Filter CA") {
		t.Error("bootstrap page still says Kids Filter CA")
	}
	if strings.Contains(page, "%!") {
		t.Error("bootstrap page has a formatting error")
	}
	if !strings.Contains(page, "<code>541B3904</code>") {
		t.Error("bootstrap page does not show the CA serial")
	}

	mc := string(mobileconfig(ca))
	if !strings.Contains(mc, "<string>Pi Home CA</string>") {
		t.Error("mobileconfig does not name the loaded CA")
	}
	if strings.Contains(mc, "%!") {
		t.Error("mobileconfig has a formatting error")
	}
}

// TestCARSAKeySignsLeaf covers loading an existing RSA CA (the Pi Home CA that
// also signs the dashboard cert) instead of the ECDSA CA initCA generates.
func TestCARSAKeySignsLeaf(t *testing.T) {
	for _, tc := range []struct {
		name string
		enc  func(*rsa.PrivateKey) ([]byte, error)
	}{
		{"pkcs1", func(k *rsa.PrivateKey) ([]byte, error) {
			return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}), nil
		}},
		{"pkcs8", func(k *rsa.PrivateKey) ([]byte, error) {
			b, err := x509.MarshalPKCS8PrivateKey(k)
			return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: b}), err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			key, err := rsa.GenerateKey(rand.Reader, 2048)
			if err != nil {
				t.Fatalf("generate RSA key: %v", err)
			}
			now := time.Now()
			tmpl := &x509.Certificate{
				SerialNumber:          big.NewInt(1),
				Subject:               pkix.Name{CommonName: "Pi Home CA"},
				NotBefore:             now.Add(-time.Hour),
				NotAfter:              now.AddDate(10, 0, 0),
				IsCA:                  true,
				BasicConstraintsValid: true,
				KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
			}
			der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
			if err != nil {
				t.Fatalf("create CA cert: %v", err)
			}
			certPath := filepath.Join(dir, "ca.crt")
			keyPath := filepath.Join(dir, "ca.key")
			if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
				t.Fatal(err)
			}
			kb, err := tc.enc(key)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(keyPath, kb, 0o600); err != nil {
				t.Fatal(err)
			}

			ca, err := loadCA(certPath, keyPath, 7)
			if err != nil {
				t.Fatalf("loadCA: %v", err)
			}
			leaf, err := ca.leaf("example.com")
			if err != nil {
				t.Fatalf("leaf: %v", err)
			}
			parsed, err := x509.ParseCertificate(leaf.Certificate[0])
			if err != nil {
				t.Fatalf("parse leaf: %v", err)
			}
			roots := x509.NewCertPool()
			roots.AddCert(ca.cert)
			if _, err := parsed.Verify(x509.VerifyOptions{DNSName: "example.com", Roots: roots}); err != nil {
				t.Errorf("RSA-signed leaf does not verify: %v", err)
			}
			if got := parsed.Issuer.CommonName; got != "Pi Home CA" {
				t.Errorf("leaf issuer = %q, want Pi Home CA", got)
			}
		})
	}
}

// TestCALoadRejectsMismatchedKey: a CA key that does not match the cert must be
// rejected at load, not fail every handshake later.
func TestCALoadRejectsMismatchedKey(t *testing.T) {
	dir := t.TempDir()
	if err := initCA(dir); err != nil {
		t.Fatalf("initCA: %v", err)
	}
	other := t.TempDir()
	if err := initCA(other); err != nil {
		t.Fatalf("initCA: %v", err)
	}
	if _, err := loadCA(filepath.Join(dir, "ca.crt"), filepath.Join(other, "ca.key"), 7); err == nil {
		t.Fatal("loadCA accepted a key that does not match the certificate")
	}
}

// TestCALeafRenewsBeforeExpiry: leaves last leaf_days, but the proxy runs for
// weeks. A cached leaf past its renewal point must be re-minted, not served.
func TestCALeafRenewsBeforeExpiry(t *testing.T) {
	dir := t.TempDir()
	if err := initCA(dir); err != nil {
		t.Fatalf("initCA: %v", err)
	}
	ca, err := loadCA(filepath.Join(dir, "ca.crt"), filepath.Join(dir, "ca.key"), 7)
	if err != nil {
		t.Fatalf("loadCA: %v", err)
	}
	first, err := ca.leaf("example.com")
	if err != nil {
		t.Fatalf("leaf: %v", err)
	}
	if again, _ := ca.leaf("example.com"); again != first {
		t.Error("fresh leaf was not served from the cache")
	}
	ca.mu.Lock()
	e := ca.cache["example.com"]
	e.renewAt = time.Now().Add(-time.Minute)
	ca.cache["example.com"] = e
	ca.mu.Unlock()
	renewed, err := ca.leaf("example.com")
	if err != nil {
		t.Fatalf("leaf: %v", err)
	}
	if renewed == first {
		t.Error("leaf past its renewal point was served from the cache")
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
	p := newProxy(cfg, ca, nil, nil, nil)
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

func TestMigrateAddsYouTubeColumns(t *testing.T) {
	db, err := writableDB(filepath.Join(t.TempDir(), "old.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE youtube (
		id INTEGER PRIMARY KEY, ts INTEGER, device TEXT, video_id TEXT,
		title TEXT, channel TEXT, seconds INTEGER, query TEXT, url TEXT)`); err != nil {
		t.Fatalf("old schema: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO youtube (ts,device,video_id,title,channel,seconds,query,url)
		VALUES (1,'aa:bb','vid','Old title','Old channel',5,'','https://www.youtube.com/youtubei/v1/player')`); err != nil {
		t.Fatalf("old row: %v", err)
	}
	if err := migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	rows, err := db.Query(`PRAGMA table_info(youtube)`)
	if err != nil {
		t.Fatalf("table_info: %v", err)
	}
	have := map[string]bool{}
	for rows.Next() {
		var (
			cid, notnull, pk int
			name, ctype      string
			dflt             any
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("scan: %v", err)
		}
		have[name] = true
	}
	rows.Close()
	for _, c := range youtubeAdded {
		if !have[c.name] {
			t.Errorf("migrate did not add column %q", c.name)
		}
	}
	var title string
	if err := db.QueryRow(`SELECT title FROM youtube WHERE video_id='vid'`).Scan(&title); err != nil {
		t.Fatalf("old row lost: %v", err)
	}
	if title != "Old title" {
		t.Errorf("title = %q", title)
	}
}

func TestYouTubeMetadataUpsert(t *testing.T) {
	cfg := defaultConfig()
	cfg.DataDir = t.TempDir()
	cfg.Capture.SnippetBytes = 4096
	s, err := openStore(cfg)
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	defer s.Close()

	family := true
	ev := func(v *YouTube) *Event {
		return &Event{TS: time.Now(), SrcIP: "192.168.30.50", Method: "GET",
			Scheme: "https", Host: "www.youtube.com", Status: 200, Action: "bump",
			URL: "https://www.youtube.com/watch?v=vid42", Video: v}
	}
	if err := s.writeEvent(ev(&YouTube{
		VideoID: "vid42", Title: "A Video", Channel: "A Channel", ChannelID: "UC1",
		Seconds: 213, Category: "Music", Tags: []string{"one", "two"},
		Description: "Description", PublishDate: "2020-01-01", ViewCount: 1234,
		FamilySafe: &family, Thumbnail: "https://i.ytimg.com/vi/vid42/hq.jpg",
		WatchURL: "https://www.youtube.com/watch?v=vid42",
	})); err != nil {
		t.Fatalf("writeEvent: %v", err)
	}
	// A later hit often carries only the ID; it must not blank the metadata.
	if err := s.writeEvent(ev(&YouTube{VideoID: "vid42"})); err != nil {
		t.Fatalf("second writeEvent: %v", err)
	}
	var (
		title, channel, category, tags, desc, watch, thumb string
		view, familySafe                                   int64
		seconds                                            int
	)
	if err := s.db.QueryRow(`SELECT title,channel,category,tags,description,watch_url,
		thumbnail,view_count,family_safe,seconds FROM youtube WHERE video_id='vid42'`).
		Scan(&title, &channel, &category, &tags, &desc, &watch, &thumb, &view, &familySafe, &seconds); err != nil {
		t.Fatalf("query: %v", err)
	}
	if title != "A Video" || channel != "A Channel" || category != "Music" || seconds != 213 {
		t.Errorf("identity lost: title=%q channel=%q category=%q seconds=%d", title, channel, category, seconds)
	}
	if tags != `["one","two"]` || desc != "Description" || watch != "https://www.youtube.com/watch?v=vid42" {
		t.Errorf("metadata lost: tags=%q desc=%q watch=%q", tags, desc, watch)
	}
	if !strings.HasSuffix(thumb, "hq.jpg") || view != 1234 || familySafe != 1 {
		t.Errorf("extra metadata lost: thumb=%q view=%d family=%d", thumb, view, familySafe)
	}
}

func TestWriteExportTSV(t *testing.T) {
	family := false
	videos := []exportVideo{{
		Date: "2026-10-31", Weekday: "Sat", Hour: 22, Device: "aa:bb:cc:dd:ee:ff",
		VideoID: "vid42", Title: "A\tVideo", Channel: "A Channel", Category: "Music",
		Tags: []string{"one", "two"}, Seconds: 213, PublishDate: "2020-01-01",
		ViewCount: 42, FamilySafe: &family, URL: "https://www.youtube.com/watch?v=vid42",
	}}
	var b bytes.Buffer
	writeExportTSV(&b, videos)
	lines := strings.Split(strings.TrimSpace(b.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines = %d, want 2:\n%s", len(lines), b.String())
	}
	fields := strings.Split(lines[1], "\t")
	if len(fields) != 14 {
		t.Fatalf("fields = %d, want 14: %q", len(fields), fields)
	}
	if fields[0] != "2026-10-31" || fields[1] != "Sat" || fields[2] != "22" {
		t.Errorf("date fields = %q", fields[:3])
	}
	if fields[5] != "A Video" {
		t.Errorf("title not flattened: %q", fields[5])
	}
	if fields[8] != `["one","two"]` || fields[12] != "false" {
		t.Errorf("tags/family = %q / %q", fields[8], fields[12])
	}
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
