package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the on-Pi configuration. Secrets (SMTP password) never live here;
// msmtp reads /etc/msmtprc which is 0600 and gitignored.
type Config struct {
	DataDir   string `yaml:"data_dir"`
	LogLevel  string `yaml:"log_level"`
	Retention int    `yaml:"retention_days"`

	Listen struct {
		Transparent string `yaml:"transparent"`
		Explicit    string `yaml:"explicit"`
	} `yaml:"listen"`

	CA struct {
		Cert     string `yaml:"cert"`
		Key      string `yaml:"key"`
		LeafDays int    `yaml:"leaf_days"`
	} `yaml:"ca"`

	Network struct {
		KidsSubnet string   `yaml:"kids_subnet"`
		Gateway    string   `yaml:"gateway"`
		Names      []string `yaml:"gateway_names"`
	} `yaml:"network"`

	Capture struct {
		MaxBodyBytes  int      `yaml:"max_body_bytes"`
		SnippetBytes  int      `yaml:"snippet_bytes"`
		StoreSnippets bool     `yaml:"store_snippets"`
		TextTypes     []string `yaml:"text_types"`
		SkipHosts     []string `yaml:"skip_hosts"`
	} `yaml:"capture"`

	// Bypass hosts are blind-tunnelled (not decrypted). Use for pinned apps
	// that break under inspection; they still show up as host-level events.
	Bypass []string `yaml:"bypass"`

	Block struct {
		Domains  []string `yaml:"domains"`
		Keywords []string `yaml:"keywords"`
	} `yaml:"block"`

	Alert struct {
		FirstSeen     bool `yaml:"first_seen"`
		LateNight     bool `yaml:"late_night"`
		LateNightFrom int  `yaml:"late_night_from"` // hour 0-23
		LateNightTo   int  `yaml:"late_night_to"`   // hour 0-23
	} `yaml:"alert"`

	Notify struct {
		Email struct {
			Enabled         bool     `yaml:"enabled"`
			Command         string   `yaml:"command"`
			To              []string `yaml:"to"`
			From            string   `yaml:"from"`
			MinSeverity     string   `yaml:"min_severity"`
			CooldownSeconds int      `yaml:"cooldown_seconds"`
			MaxPerHour      int      `yaml:"max_per_hour"`
		} `yaml:"email"`
	} `yaml:"notify"`

	// Devices maps a MAC (lowercase) to a friendly name so logs read
	// "kid-phone" instead of 192.168.30.104.
	Devices map[string]string `yaml:"devices"`

	path string
}

func defaultConfig() *Config {
	c := &Config{DataDir: "/var/lib/kidproxy", LogLevel: "info", Retention: 30}
	c.Listen.Transparent = ":8080"
	c.CA.Cert = "/etc/kidproxy/ca.crt"
	c.CA.Key = "/etc/kidproxy/ca.key"
	c.CA.LeafDays = 7
	c.Network.KidsSubnet = "192.168.30.0/24"
	c.Network.Gateway = "192.168.30.1"
	c.Network.Names = []string{"setup.kids", "kidproxy.lan"}
	c.Capture.MaxBodyBytes = 262144
	c.Capture.SnippetBytes = 4096
	c.Capture.StoreSnippets = true
	c.Capture.TextTypes = []string{
		"text/html", "application/xhtml+xml", "text/plain",
		"application/json", "application/x-json", "text/json",
	}
	c.Capture.SkipHosts = []string{
		"googlevideo.com", "ytimg.com", "ggpht.com", "gstatic.com",
		"doubleclick.net", "google-analytics.com",
	}
	c.Alert.FirstSeen = true
	c.Alert.LateNight = true
	c.Alert.LateNightFrom = 23
	c.Alert.LateNightTo = 6
	c.Notify.Email.Command = "/usr/bin/msmtp"
	c.Notify.Email.MinSeverity = "warn"
	c.Notify.Email.CooldownSeconds = 900
	c.Notify.Email.MaxPerHour = 20
	return c
}

func loadConfig(path string) (*Config, error) {
	c := defaultConfig()
	c.path = path
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if err := yaml.Unmarshal(b, c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if c.Capture.SnippetBytes > c.Capture.MaxBodyBytes {
		c.Capture.SnippetBytes = c.Capture.MaxBodyBytes
	}
	if c.Listen.Transparent == "" && c.Listen.Explicit == "" {
		return nil, fmt.Errorf("no listener configured")
	}
	return c, nil
}

func (c *Config) severityAtLeast(s string) bool {
	order := map[string]int{"info": 0, "warn": 1, "high": 2}
	min, ok := order[strings.ToLower(c.Notify.Email.MinSeverity)]
	if !ok {
		min = 1
	}
	v, ok := order[strings.ToLower(s)]
	if !ok {
		v = 0
	}
	return v >= min
}

func (c *Config) isLateNight(t time.Time) bool {
	if !c.Alert.LateNight {
		return false
	}
	h := t.Hour()
	from, to := c.Alert.LateNightFrom, c.Alert.LateNightTo
	if from == to {
		return false
	}
	if from < to {
		return h >= from && h < to
	}
	return h >= from || h < to
}

func (c *Config) isCapturableType(ct string) bool {
	ct = strings.ToLower(strings.TrimSpace(strings.SplitN(ct, ";", 2)[0]))
	for _, t := range c.Capture.TextTypes {
		if ct == strings.ToLower(t) {
			return true
		}
	}
	return false
}

func (c *Config) skipHost(host string) bool {
	host = strings.ToLower(host)
	for _, h := range c.Capture.SkipHosts {
		if host == h || strings.HasSuffix(host, "."+h) {
			return true
		}
	}
	return false
}
