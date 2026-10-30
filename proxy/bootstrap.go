package main

import (
	"encoding/base64"
	"fmt"
	"net/http"
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
</style></head><body>
<h1>Kids network setup</h1>
<p>This network filters and logs web traffic so a parent can keep an eye on things.
To make that work, this device has to trust the router's certificate authority.</p>

<p><a class="btn" href="/ca.crt">Download the certificate</a>
<a class="btn" href="/ca.mobileconfig" style="background:#334155">iPhone / iPad profile</a></p>

<h2>iPhone / iPad</h2>
<ol>
<li>Tap <b>iPhone / iPad profile</b> above, then <b>Allow</b>, then close the warning.</li>
<li>Open <b>Settings</b> → <b>Profile Downloaded</b> → <b>Install</b> (top right) → enter the passcode.</li>
<li><b>Required:</b> Settings → General → About → <b>Certificate Trust Settings</b> → turn on <b>Kids Filter CA</b>.</li>
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
<li>Open Keychain Access → <b>System</b> → find <b>Kids Filter CA</b> → double-click → <b>Trust</b> → <b>Always Trust</b>.</li>
</ol>

<div class="note"><b>Apps that won't work:</b> some apps pin their certificates (Snapchat, TikTok, banking, games).
They will show connection errors on this network. That is expected — ask a parent if an app needs to be allowed.</div>
<p style="margin-top:2rem;color:#777;font-size:.85rem">Certificate fingerprint (serial): <code>%s</code></p>
</body></html>`

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
	default:
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		fmt.Fprintf(w, bootstrapHTML, p.ca.fingerprint())
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
      <key>PayloadContent</key><data>%s</data>
      <key>PayloadDescription</key><string>Lets the kids network inspect web traffic.</string>
      <key>PayloadDisplayName</key><string>Kids Filter CA</string>
      <key>PayloadIdentifier</key><string>lan.kids.ca.%s</string>
      <key>PayloadType</key><string>com.apple.security.root</string>
      <key>PayloadUUID</key><string>7E1B2C44-0F1A-4C21-9A5E-3B7D0C1A2F01</string>
      <key>PayloadVersion</key><integer>1</integer>
    </dict>
  </array>
  <key>PayloadDisplayName</key><string>Kids Filter CA</string>
  <key>PayloadIdentifier</key><string>lan.kids.profile</string>
  <key>PayloadOrganization</key><string>Home</string>
  <key>PayloadRemovalDisallowed</key><false/>
  <key>PayloadType</key><string>Configuration</string>
  <key>PayloadUUID</key><string>9C2D3E55-1A2B-4D32-8B6F-4C8E1D2B3A02</string>
  <key>PayloadVersion</key><integer>1</integer>
</dict>
</plist>
`, b64, ca.fingerprint()))
}
