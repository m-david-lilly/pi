package main

import (
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
)

const bootstrapHTML = `<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Kids network setup</title>
<style>
:root{color-scheme:light dark}
body{font-family:system-ui,-apple-system,sans-serif;max-width:38rem;margin:0 auto;padding:2rem 1.25rem;line-height:1.55}
h1{font-size:1.5rem;margin-bottom:.25rem}h2{font-size:1.05rem;margin-top:2rem}
a.btn{display:inline-block;background:#2563eb;color:#fff;text-decoration:none;padding:.7rem 1.1rem;border-radius:.5rem;margin:.4rem 0}
code,pre{background:rgba(127,127,127,.15);padding:.15rem .35rem;border-radius:.25rem}
ol,ul{padding-left:1.3rem}li{margin:.3rem 0}
.note{border-left:3px solid #d97706;padding:.6rem .9rem;background:rgba(217,119,6,.08);border-radius:.3rem;margin:1.2rem 0}
.ok{border-left:3px solid #16a34a;padding:.6rem .9rem;background:rgba(22,163,74,.08);border-radius:.3rem;margin:1.2rem 0}
</style></head><body>
<h1>Kids network setup</h1>
<p>This network filters and logs web traffic so a parent can keep an eye on things.
To make that work, this device has to trust the router's certificate authority.</p>

<p id="trust" class="note">Checking whether this device trusts the certificate…</p>

<p><a class="btn" href="/ca.crt">Download the certificate</a>
<a class="btn" href="/ca.mobileconfig" style="background:#334155">iPhone / iPad profile</a></p>

<h2>iPhone / iPad</h2>
<ol>
<li>Tap <b>iPhone / iPad profile</b> above, then <b>Allow</b>, then close the warning.</li>
<li>Open <b>Settings</b> → <b>Profile Downloaded</b> → <b>Install</b> (top right) → enter the passcode.</li>
<li><b>Required:</b> Settings → General → About → <b>Certificate Trust Settings</b> → turn on <b>%[1]s</b>.</li>
</ol>

<h2>Android</h2>
<ol>
<li>Tap <b>Download the certificate</b> above.</li>
<li>Settings → Security → Encryption &amp; credentials → <b>Install a certificate</b> → <b>CA certificate</b> → install anyway.</li>
<li>Chrome and other browsers will now be filtered. Many apps ignore user certificates by design and may fail to load.</li>
</ol>

<h2>Windows</h2>
<ol>
<li>Download the certificate, then double-click it → <b>Install Certificate</b> → <b>Local Machine</b>.</li>
<li>Choose <b>Place all certificates in the following store</b> → <b>Trusted Root Certification Authorities</b>.</li>
</ol>

<h2>macOS</h2>
<ol>
<li>Download the certificate and open it — Keychain Access installs it in <b>login</b>.</li>
<li>Open Keychain Access → <b>System</b> → find <b>%[1]s</b> → double-click → <b>Trust</b> → <b>Always Trust</b>.</li>
</ol>

<div class="note"><b>Apps that won't work:</b> some apps pin their certificates (Snapchat, TikTok, banking, games).
They will show connection errors on this network. That is expected — ask a parent if an app needs to be allowed.</div>
<p style="margin-top:2rem;color:#777;font-size:.85rem">Certificate fingerprint (serial): <code>%[2]s</code></p>
<script>
(function () {
  var el = document.getElementById('trust');
  if (!el) return;
  // Probe the exact host the page was reached on: it is known to resolve and
  // is covered by the leaf the proxy mints (DNS name or IP SAN).
  var host = location.hostname || %[3]s;

  function report(ok) {
    el.textContent = ok
      ? 'This device trusts the certificate. Filtering and content logging are fully active.'
      : 'This device does not trust the certificate yet. Install it below, then reload this page.';
    el.className = ok ? 'ok' : 'note';
    // Same-origin POST over plain HTTP: the proxy records the answer per MAC
    // so the parent dashboard can show whether the CA is trusted.
    fetch('/ca-trust?ok=' + (ok ? '1' : '0'), {method: 'POST', cache: 'no-store'}).catch(function () {});
  }

  // Over HTTPS the handshake itself already proved trust.
  if (location.protocol === 'https:') { report(true); return; }
  if (!host) { el.textContent = ''; return; }
  fetch('https://' + host + '/ca-probe', {mode: 'no-cors', cache: 'no-store'})
    .then(function () { report(true); })
    .catch(function () { report(false); });
})();
</script>
</body></html>`

// bootstrapPage renders the CA install instructions. The CA's own name is used
// so the device's trust toggle matches what the user sees (the router may sign
// with the Pi Home CA, not the "Kids Filter CA" this program generates).
func bootstrapPage(ca *CA, probeHost string) string {
	return fmt.Sprintf(bootstrapHTML, ca.name(), ca.fingerprint(), strconv.Quote(probeHost))
}

// probeHost is the fallback the bootstrap page uses to test whether the device
// trusts the CA when the browser reports no hostname. Normally the page probes
// the host it was loaded from, so both DNS names and the gateway IP work.
func (p *Proxy) probeHost() string {
	for _, n := range p.cfg.Network.Names {
		if n = strings.TrimSpace(n); n != "" {
			return n
		}
	}
	return p.cfg.Network.Gateway
}

func (p *Proxy) serveBootstrap(w http.ResponseWriter, r *http.Request) {
	switch strings.TrimSuffix(r.URL.Path, "/") {
	case "/ca.crt":
		w.Header().Set("Content-Type", "application/x-x509-ca-cert")
		w.Header().Set("Content-Disposition", `attachment; filename="kids-filter-ca.crt"`)
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(p.ca.pem())
	case "/ca.mobileconfig":
		w.Header().Set("Content-Type", "application/x-apple-aspen-config")
		w.Header().Set("Content-Disposition", `attachment; filename="kids-filter-ca.mobileconfig"`)
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(mobileconfig(p.ca))
	case "/ca-probe":
		// The page's JS fetches this over HTTPS; if the handshake succeeds the
		// device accepted a certificate minted by our CA.
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
	case "/ca-trust":
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		p.recordCATrust(r, r.URL.Query().Get("ok") == "1")
		w.WriteHeader(http.StatusNoContent)
	default:
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = io.WriteString(w, bootstrapPage(p.ca, p.probeHost()))
	}
}

// recordCATrust stores the probe answer for the requesting device. It is a
// dashboard hint, not a security control: the parent still decides whether to
// turn inspection off.
func (p *Proxy) recordCATrust(r *http.Request, ok bool) {
	if p.store == nil || p.store.db == nil {
		return
	}
	mac := p.store.deviceMAC(remoteIPFromRequest(r))
	if mac == "" {
		return
	}
	if _, err := setCATrustIfChanged(p.store.db, mac, ok); err != nil {
		log.Printf("ca-trust: %v", err)
	}
}

// mobileconfig wraps the CA in an unsigned iOS profile. The CA itself is what
// iOS trusts; the profile just makes installation one tap.
func mobileconfig(ca *CA) []byte {
	der := ca.cert.Raw
	b64 := base64.StdEncoding.EncodeToString(der)
	return []byte(fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>PayloadContent</key>
  <array>
    <dict>
      <key>PayloadCertificateFileName</key><string>KidsFilterCA.crt</string>
      <key>PayloadContent</key><data>%[1]s</data>
      <key>PayloadDescription</key><string>Lets the kids network inspect web traffic.</string>
      <key>PayloadDisplayName</key><string>%[2]s</string>
      <key>PayloadIdentifier</key><string>lan.kids.ca.%[3]s</string>
      <key>PayloadType</key><string>com.apple.security.root</string>
      <key>PayloadUUID</key><string>7E1B2C44-0F1A-4C21-9A5E-3B7D0C1A2F01</string>
      <key>PayloadVersion</key><integer>1</integer>
    </dict>
  </array>
  <key>PayloadDisplayName</key><string>%[2]s</string>
  <key>PayloadIdentifier</key><string>lan.kids.profile</string>
  <key>PayloadOrganization</key><string>Home</string>
  <key>PayloadRemovalDisallowed</key><false/>
  <key>PayloadType</key><string>Configuration</string>
  <key>PayloadUUID</key><string>9C2D3E55-1A2B-4D32-8B6F-4C8E1D2B3A02</string>
  <key>PayloadVersion</key><integer>1</integer>
</dict>
</plist>
`, b64, ca.name(), ca.fingerprint()))
}
