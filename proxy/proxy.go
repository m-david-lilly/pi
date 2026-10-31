package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/net/http2"
)

type ctxKey int

const metaKey ctxKey = 1

// originalDstFunc is a seam for tests: the real implementation reads
// SO_ORIGINAL_DST, which only returns anything for a NATed socket.
var originalDstFunc = originalDst

type connMeta struct {
	srcIP  string
	host   string
	scheme string
	port   int
}

// connQueue adapts accepted connections into an http.Server listener so one
// server instance (and one HTTP/2 config) handles every connection.
type connQueue struct {
	ch   chan net.Conn
	done chan struct{}
	once sync.Once
}

func newConnQueue() *connQueue {
	return &connQueue{ch: make(chan net.Conn, 64), done: make(chan struct{})}
}

func (q *connQueue) push(c net.Conn) {
	select {
	case q.ch <- c:
	case <-q.done:
		_ = c.Close()
	}
}

func (q *connQueue) Accept() (net.Conn, error) {
	select {
	case c := <-q.ch:
		return c, nil
	case <-q.done:
		return nil, net.ErrClosed
	}
}

func (q *connQueue) Close() error {
	q.once.Do(func() { close(q.done) })
	return nil
}

func (q *connQueue) Addr() net.Addr { return &net.TCPAddr{} }

type Proxy struct {
	cfg       *Config
	ca        *CA
	store     *Store
	rules     *Rules
	transport *http.Transport
	srv       *http.Server
	queue     *connQueue
	conns     sync.Map // net.Conn -> *connMeta
}

func newProxy(cfg *Config, ca *CA, store *Store, rules *Rules) *Proxy {
	p := &Proxy{cfg: cfg, ca: ca, store: store, rules: rules, queue: newConnQueue()}
	p.transport = &http.Transport{
		Proxy:                 nil,
		DialContext:           upstreamDialer().DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          128,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: 45 * time.Second,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
	}
	p.srv = &http.Server{
		Handler:           http.HandlerFunc(p.handle),
		ReadHeaderTimeout: 20 * time.Second,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    256 << 10,
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			if m, ok := p.conns.Load(c); ok {
				return context.WithValue(ctx, metaKey, m)
			}
			return ctx
		},
		ConnState: func(c net.Conn, st http.ConnState) {
			if st == http.StateClosed || st == http.StateHijacked {
				p.conns.Delete(c)
			}
		},
	}
	_ = http2.ConfigureServer(p.srv, &http2.Server{MaxConcurrentStreams: 100})
	return p
}

func (p *Proxy) Serve() error {
	go func() {
		if err := p.srv.Serve(p.queue); err != nil && !errors.Is(err, net.ErrClosed) {
			log.Printf("http server: %v", err)
		}
	}()
	if p.cfg.Listen.Transparent != "" {
		ln, err := net.Listen("tcp", p.cfg.Listen.Transparent)
		if err != nil {
			return fmt.Errorf("transparent listen: %w", err)
		}
		log.Printf("transparent listener on %s", p.cfg.Listen.Transparent)
		go p.acceptLoop(ln, true)
	}
	if p.cfg.Listen.Explicit != "" {
		ln, err := net.Listen("tcp", p.cfg.Listen.Explicit)
		if err != nil {
			return fmt.Errorf("explicit listen: %w", err)
		}
		log.Printf("explicit proxy on %s", p.cfg.Listen.Explicit)
		go p.acceptLoop(ln, false)
	}
	select {}
}

func (p *Proxy) acceptLoop(ln net.Listener, transparent bool) {
	for {
		c, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			log.Printf("accept: %v", err)
			time.Sleep(50 * time.Millisecond)
			continue
		}
		go p.handleConn(c, transparent)
	}
}

func (p *Proxy) handleConn(c net.Conn, transparent bool) {
	setDeadline(c, 15*time.Second)

	var dstHost string
	var dstPort int
	if transparent {
		h, port, err := originalDstFunc(c)
		if err != nil {
			log.Printf("transparent: %v", err)
			_ = c.Close()
			return
		}
		dstHost, dstPort = h, port
	}

	br := bufio.NewReaderSize(c, 16<<10)
	first, err := br.Peek(1)
	if err != nil {
		_ = c.Close()
		return
	}

	if !transparent {
		p.handleExplicit(c, br)
		return
	}

	if first[0] != 0x16 {
		// Plain HTTP: hand the connection to the HTTP server. Peek pulled the
		// first request out of the socket and into br, so replay br -- pushing
		// the raw conn drops those bytes and the server waits forever for a
		// request that already arrived.
		pc := &prefixConn{Conn: c, r: br}
		meta := &connMeta{srcIP: remoteIP(c), host: dstHost, scheme: "http", port: dstPort}
		p.conns.Store(pc, meta)
		_ = c.SetReadDeadline(time.Time{})
		p.queue.push(pc)
		return
	}

	sni, consumed, err := readClientHello(br)
	if err != nil {
		// Advertised TLS but unparsable: fail closed rather than leak.
		p.logEvent(&Event{SrcIP: remoteIP(c), Action: "error", Reason: "clienthello: " + err.Error(),
			Host: dstHost, URL: fmt.Sprintf("tcp://%s:%d", dstHost, dstPort)})
		_ = c.Close()
		return
	}
	host := sni
	if host == "" {
		host = dstHost
	}
	p.bump(c, br, consumed, host, dstPort)
}

// bump terminates TLS with a CA-minted certificate and serves the request.
func (p *Proxy) bump(c net.Conn, br *bufio.Reader, consumed []byte, host string, port int) {
	if ok, note := p.rules.blocked(host); ok {
		p.serveBlockedTLS(c, br, consumed, host, port, note)
		return
	}
	if p.isBypass(host) {
		p.tunnel(c, br, consumed, host, port, "bypass")
		return
	}
	leaf, err := p.ca.leaf(host)
	if err != nil {
		log.Printf("mint %s: %v", host, err)
		_ = c.Close()
		return
	}
	replay := &prefixConn{Conn: c, r: io.MultiReader(bytes.NewReader(consumed), br)}
	tlsConn := tls.Server(replay, &tls.Config{
		Certificates: []tls.Certificate{*leaf},
		NextProtos:   []string{"h2", "http/1.1"},
		MinVersion:   tls.VersionTLS12,
	})
	_ = c.SetReadDeadline(time.Time{})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		p.logEvent(&Event{SrcIP: remoteIP(c), Host: host, Action: "error",
			Reason: "handshake: " + err.Error(), URL: fmt.Sprintf("https://%s:%d/", host, port)})
		_ = c.Close()
		return
	}
	meta := &connMeta{srcIP: remoteIP(c), host: host, scheme: "https", port: port}
	p.conns.Store(tlsConn, meta)
	p.queue.push(tlsConn)
}

// tunnel relays raw bytes (pinned apps, allowlisted hosts) without decrypting.
func (p *Proxy) tunnel(c net.Conn, br *bufio.Reader, consumed []byte, host string, port int, why string) {
	addr := net.JoinHostPort(host, fmt.Sprint(port))
	up, err := upstreamDialer().Dial("tcp", addr)
	if err != nil {
		_ = c.Close()
		return
	}
	p.logEvent(&Event{SrcIP: remoteIP(c), Host: host, Action: "tunnel", Reason: why,
		URL: fmt.Sprintf("tcp://%s:%d", host, port), Scheme: "tls"})
	if len(consumed) > 0 {
		if _, err := up.Write(consumed); err != nil {
			_ = up.Close()
			_ = c.Close()
			return
		}
	}
	_ = c.SetReadDeadline(time.Time{})
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(up, br)
		if tc, ok := up.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
		done <- struct{}{}
	}()
	go func() { _, _ = io.Copy(c, up); done <- struct{}{} }()
	<-done
	_ = up.Close()
	_ = c.Close()
}

// handleExplicit implements CONNECT tunnelling for the optional test listener.
func (p *Proxy) handleExplicit(c net.Conn, br *bufio.Reader) {
	_ = c.SetReadDeadline(time.Now().Add(15 * time.Second))
	req, err := http.ReadRequest(br)
	if err != nil {
		_ = c.Close()
		return
	}
	if req.Method != http.MethodConnect {
		_, _ = io.WriteString(c, "HTTP/1.1 405 Method Not Allowed\r\nConnection: close\r\n\r\n")
		_ = c.Close()
		return
	}
	host, portStr, err := net.SplitHostPort(req.Host)
	if err != nil {
		host, portStr = req.Host, "443"
	}
	port := 443
	fmt.Sscanf(portStr, "%d", &port)
	_, _ = io.WriteString(c, "HTTP/1.1 200 Connection Established\r\n\r\n")

	sni, consumed, err := readClientHello(br)
	if err != nil {
		_ = c.Close()
		return
	}
	if sni == "" {
		sni = host
	}
	_ = c.SetReadDeadline(time.Time{})
	p.bump(c, br, consumed, sni, port)
}

func (p *Proxy) isBypass(host string) bool {
	for _, b := range p.cfg.Bypass {
		if hostMatches(host, b) {
			return true
		}
	}
	return false
}

func (p *Proxy) isBootstrapHost(host string) bool {
	host = strings.ToLower(hostOnly(host))
	if host == strings.ToLower(p.cfg.Network.Gateway) {
		return true
	}
	for _, n := range p.cfg.Network.Names {
		if host == strings.ToLower(n) {
			return true
		}
	}
	return false
}

func (p *Proxy) handle(w http.ResponseWriter, r *http.Request) {
	meta, _ := r.Context().Value(metaKey).(*connMeta)
	if meta == nil {
		meta = &connMeta{srcIP: remoteIPFromRequest(r), scheme: "https"}
	}
	host := hostOnly(r.Host)
	if host == "" {
		host = meta.host
	}
	if p.isBootstrapHost(host) {
		p.serveBootstrap(w, r)
		return
	}
	if ok, note := p.rules.blocked(host); ok {
		p.writeBlockPage(w, r, note)
		p.logEvent(&Event{SrcIP: meta.srcIP, Method: r.Method, Scheme: meta.scheme, Host: host,
			URL: absoluteURL(meta.scheme, r), Action: "block", Reason: note})
		return
	}
	if isWebsocket(r) {
		p.handleWS(w, r, meta, host)
		return
	}
	p.forward(w, r, meta, host)
}

func (p *Proxy) forward(w http.ResponseWriter, r *http.Request, meta *connMeta, host string) {
	scheme := meta.scheme
	if scheme == "" {
		scheme = "https"
	}
	if r.URL.Scheme != "" {
		scheme = r.URL.Scheme
	}
	target := scheme + "://" + r.Host + r.URL.RequestURI()

	var reqCap *cappedBuffer
	var body io.Reader = r.Body
	if r.Body != nil && r.ContentLength != 0 {
		reqCap = newCapped(64 << 10)
		body = io.TeeReader(r.Body, reqCap)
	}
	outReq, err := http.NewRequestWithContext(r.Context(), r.Method, target, body)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	outReq.ContentLength = r.ContentLength
	outReq.Host = r.Host
	outReq.Header = r.Header.Clone()
	stripHopByHop(outReq.Header)
	outReq.Header.Del("Accept-Encoding") // let Go add gzip and decompress for us

	resp, err := p.transport.RoundTrip(outReq)
	if err != nil {
		http.Error(w, "upstream error: "+err.Error(), http.StatusBadGateway)
		p.logEvent(&Event{SrcIP: meta.srcIP, Method: r.Method, Scheme: scheme, Host: host,
			URL: target, Action: "error", Reason: err.Error()})
		return
	}
	defer resp.Body.Close()

	ct := resp.Header.Get("Content-Type")
	capture := p.shouldCapture(host, ct, resp.ContentLength)

	h := w.Header()
	for k, vv := range resp.Header {
		for _, v := range vv {
			h.Add(k, v)
		}
	}
	stripHopByHop(h)
	w.WriteHeader(resp.StatusCode)

	var buf *cappedBuffer
	var dst io.Writer = w
	if capture {
		buf = newCapped(p.cfg.Capture.MaxBodyBytes)
		dst = io.MultiWriter(w, buf)
	}
	if f, ok := w.(http.Flusher); ok {
		dst = &flushWriter{w: dst, f: f}
	}
	n, copyErr := io.Copy(dst, resp.Body)

	ev := &Event{
		TS: time.Now(), SrcIP: meta.srcIP, Method: r.Method, Scheme: scheme,
		Host: host, URL: target, Status: resp.StatusCode, MIME: ct,
		RespSize: n, Action: "bump",
	}
	if reqCap != nil {
		ev.ReqBytes = int64(reqCap.Len())
	}
	if buf != nil {
		ev.Snippet = extractText(ct, buf.Bytes(), p.cfg.Capture.SnippetBytes)
	}
	// Search queries and video IDs come from the URL and request body, so log
	// them even when the response is not captured (store_snippets off,
	// skip_hosts, non-text types); only player titles need the response.
	ev.Video = parseYouTube(target, capBytes(reqCap), capBytes(buf))
	ev.Search = parseSearch(target, capBytes(reqCap))
	if copyErr != nil && !errors.Is(copyErr, io.ErrUnexpectedEOF) {
		ev.Reason = "copy: " + copyErr.Error()
	}
	p.logEvent(ev)
}

func (p *Proxy) shouldCapture(host, ct string, size int64) bool {
	if !p.cfg.Capture.StoreSnippets || p.cfg.skipHost(host) {
		return false
	}
	if !p.cfg.isCapturableType(ct) {
		return false
	}
	if size > 8<<20 {
		return false
	}
	return true
}

func (p *Proxy) handleWS(w http.ResponseWriter, r *http.Request, meta *connMeta, host string) {
	scheme := meta.scheme
	addr := wsDialAddr(r.Host, scheme)
	var up net.Conn
	var err error
	d := upstreamDialer()
	if scheme == "http" {
		up, err = d.Dial("tcp", addr)
	} else {
		up, err = tls.DialWithDialer(d, "tcp", addr, &tls.Config{ServerName: hostOnly(addr), MinVersion: tls.VersionTLS12})
	}
	if err != nil {
		http.Error(w, "upstream error", http.StatusBadGateway)
		p.logEvent(&Event{SrcIP: meta.srcIP, Method: r.Method, Scheme: scheme, Host: host,
			URL: absoluteURL(scheme, r), Action: "error", Reason: "ws dial: " + err.Error()})
		return
	}
	r.Header.Del("Proxy-Connection")
	if err := r.Write(up); err != nil {
		_ = up.Close()
		http.Error(w, "upstream write failed", http.StatusBadGateway)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		_ = up.Close()
		http.Error(w, "hijack unsupported", http.StatusNotImplemented)
		return
	}
	client, rw, err := hj.Hijack()
	if err != nil {
		_ = up.Close()
		return
	}
	p.logEvent(&Event{SrcIP: meta.srcIP, Method: r.Method, Scheme: scheme, Host: host,
		URL: absoluteURL(scheme, r), Status: http.StatusSwitchingProtocols, Action: "tunnel", Reason: "websocket"})
	go func() { _, _ = io.Copy(up, rw); _ = up.Close() }()
	_, _ = io.Copy(client, up)
	_ = client.Close()
	_ = up.Close()
}

func (p *Proxy) writeBlockPage(w http.ResponseWriter, r *http.Request, note string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusForbidden)
	fmt.Fprintf(w, `<!doctype html><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Blocked</title>
<style>body{font-family:system-ui,sans-serif;max-width:34rem;margin:15vh auto;padding:0 1.5rem;color:#222}
h1{font-size:1.4rem}p{line-height:1.5;color:#555}code{background:#f2f2f2;padding:.1rem .3rem;border-radius:4px}</style>
<h1>This site is blocked on the kids network</h1>
<p><code>%s</code></p>
<p>Reason: %s</p>
<p>If this is a mistake, ask a parent to allow it from the router dashboard.</p>`,
		templateEscape(r.Host), templateEscape(note))
}

// serveBlockedTLS completes a handshake just to show the block page.
func (p *Proxy) serveBlockedTLS(c net.Conn, br *bufio.Reader, consumed []byte, host string, port int, note string) {
	leaf, err := p.ca.leaf(host)
	if err != nil {
		_ = c.Close()
		return
	}
	replay := &prefixConn{Conn: c, r: io.MultiReader(bytes.NewReader(consumed), br)}
	tlsConn := tls.Server(replay, &tls.Config{
		Certificates: []tls.Certificate{*leaf},
		NextProtos:   []string{"http/1.1"},
		MinVersion:   tls.VersionTLS12,
	})
	_ = c.SetReadDeadline(time.Now().Add(15 * time.Second))
	if err := tlsConn.Handshake(); err != nil {
		_ = c.Close()
		return
	}
	body := fmt.Sprintf("<!doctype html><meta name=viewport content='width=device-width,initial-scale=1'>"+
		"<title>Blocked</title><body style='font-family:system-ui;max-width:34rem;margin:15vh auto;padding:0 1.5rem'>"+
		"<h1 style='font-size:1.3rem'>This site is blocked on the kids network</h1><p><code>%s</code></p><p>Reason: %s</p>",
		templateEscape(host), templateEscape(note))
	fmt.Fprintf(tlsConn, "HTTP/1.1 403 Forbidden\r\nContent-Type: text/html; charset=utf-8\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		len(body), body)
	p.logEvent(&Event{SrcIP: remoteIP(c), Host: host, Scheme: "https",
		URL: fmt.Sprintf("https://%s:%d/", host, port), Action: "block", Reason: note})
	_ = tlsConn.Close()
}

func (p *Proxy) logEvent(ev *Event) {
	if ev.TS.IsZero() {
		ev.TS = time.Now()
	}
	p.store.Ingest(ev)
}

// ---- small helpers ----

// upstreamDialer is the only way the proxy opens connections on a kid's
// behalf (HTTP transport, websockets, blind tunnels).
func upstreamDialer() *net.Dialer {
	return &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second, Control: denyPrivate}
}

// denyPrivate is a net.Dialer Control hook. The proxy dials from the router
// itself and the target comes from the client (Host header or SNI), so without
// it a kids device could reach the adult LAN, the admin dashboard, or the
// router's loopback services through the proxy -- including the proxy's own
// listener (Host: 127.0.0.1:8080), which loops until it runs out of file
// descriptors. It runs after DNS resolution, so a public name that resolves to
// a private address is refused too.
func denyPrivate(network, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("refusing to dial %s: not an IP address", address)
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() || ip.IsMulticast() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return fmt.Errorf("refusing to dial %s: private or local address", address)
	}
	return nil
}

// wsDialAddr adds the scheme's default port: browsers omit it from the Host
// header, and net.Dial rejects an address without one.
func wsDialAddr(host, scheme string) string {
	if _, _, err := net.SplitHostPort(host); err == nil {
		return host
	}
	port := "443"
	if scheme == "http" {
		port = "80"
	}
	return net.JoinHostPort(strings.Trim(host, "[]"), port)
}

type cappedBuffer struct {
	buf []byte
	max int
}

func newCapped(max int) *cappedBuffer { return &cappedBuffer{max: max} }

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if len(c.buf) < c.max {
		room := c.max - len(c.buf)
		if len(p) > room {
			c.buf = append(c.buf, p[:room]...)
		} else {
			c.buf = append(c.buf, p...)
		}
	}
	return len(p), nil
}

func (c *cappedBuffer) Bytes() []byte { return c.buf }
func (c *cappedBuffer) Len() int      { return len(c.buf) }

func capBytes(c *cappedBuffer) []byte {
	if c == nil {
		return nil
	}
	return c.buf
}

type flushWriter struct {
	w io.Writer
	f http.Flusher
}

func (fw *flushWriter) Write(p []byte) (int, error) {
	n, err := fw.w.Write(p)
	fw.f.Flush()
	return n, err
}

func remoteIP(c net.Conn) string {
	if c == nil {
		return ""
	}
	h, _, err := net.SplitHostPort(c.RemoteAddr().String())
	if err != nil {
		return c.RemoteAddr().String()
	}
	return h
}

func remoteIPFromRequest(r *http.Request) string {
	h, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return h
}

func hostOnly(h string) string {
	if h == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(h); err == nil {
		return strings.ToLower(host)
	}
	return strings.ToLower(strings.Trim(h, "[]"))
}

func absoluteURL(scheme string, r *http.Request) string {
	if r.URL.IsAbs() {
		return r.URL.String()
	}
	return scheme + "://" + r.Host + r.URL.RequestURI()
}

func isWebsocket(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket") &&
		strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade")
}

var hopHeaders = []string{
	"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

func stripHopByHop(h http.Header) {
	for _, k := range hopHeaders {
		h.Del(k)
	}
}

func templateEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&#34;", "'", "&#39;")
	return r.Replace(s)
}
