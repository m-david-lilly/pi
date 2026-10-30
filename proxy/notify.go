package main

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Notifier sends alert email through msmtp (configured in /etc/msmtprc, 0600).
// Keeping SMTP credentials in msmtp means they are not duplicated in YAML.
type Notifier struct {
	cfg *Config

	mu        sync.Mutex
	last      map[string]time.Time
	hourStart time.Time
	hourCount int
}

func newNotifier(cfg *Config) *Notifier {
	return &Notifier{cfg: cfg, last: map[string]time.Time{}}
}

func (n *Notifier) Run(ch <-chan *Alert) {
	for a := range ch {
		if !n.cfg.Notify.Email.Enabled {
			continue
		}
		if !n.cfg.severityAtLeast(a.Severity) {
			continue
		}
		key := a.Kind + "|" + a.Host + "|" + a.Device
		if !n.allow(key) {
			continue
		}
		if err := n.send(a); err != nil {
			log.Printf("notify: %v", err)
		}
	}
}

func (n *Notifier) allow(key string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	now := time.Now()
	cd := time.Duration(n.cfg.Notify.Email.CooldownSeconds) * time.Second
	if t, ok := n.last[key]; ok && now.Sub(t) < cd {
		return false
	}
	if now.Sub(n.hourStart) > time.Hour {
		n.hourStart = now
		n.hourCount = 0
	}
	if n.cfg.Notify.Email.MaxPerHour > 0 && n.hourCount >= n.cfg.Notify.Email.MaxPerHour {
		return false
	}
	n.last[key] = now
	n.hourCount++
	return true
}

func (n *Notifier) send(a *Alert) error {
	cmd := n.cfg.Notify.Email.Command
	if cmd == "" {
		cmd = "/usr/bin/msmtp"
	}
	to := strings.Join(n.cfg.Notify.Email.To, ", ")
	if to == "" {
		return fmt.Errorf("no recipients configured")
	}
	from := n.cfg.Notify.Email.From
	if from == "" {
		from = "kidproxy@localhost"
	}
	clean := func(s string) string {
		return strings.NewReplacer("\r", " ", "\n", " ").Replace(s)
	}
	subject := fmt.Sprintf("[kidproxy] %s: %s", a.Severity, a.Kind)
	if a.Device != "" {
		subject += " — " + clean(a.Device)
	}
	var body bytes.Buffer
	fmt.Fprintf(&body, "From: %s\r\nTo: %s\r\nSubject: %s\r\nDate: %s\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n",
		from, to, clean(subject), a.TS.Format(time.RFC1123Z))
	fmt.Fprintf(&body, "Time:   %s\nDevice: %s\nHost:   %s\nKind:   %s\nDetail: %s\nURL:    %s\n",
		a.TS.Format("2006-01-02 15:04:05"), a.Device, a.Host, a.Kind, a.Detail, a.URL)
	fmt.Fprintf(&body, "\n-- kidproxy on %s\n", hostname())

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, cmd, "-t")
	c.Stdin = &body
	var stderr bytes.Buffer
	c.Stderr = &stderr
	if err := c.Run(); err != nil {
		return fmt.Errorf("msmtp: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	log.Printf("notify: emailed %q to %s", subject, to)
	return nil
}
