package main

import (
	"context"
	"crypto/sha256"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

const version = "0.1.0"

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "run":
		runCmd(os.Args[2:])
	case "check":
		checkCmd(os.Args[2:])
	case "ca":
		caCmd(os.Args[2:])
	case "query":
		queryCmd(os.Args[2:])
	case "notify-test":
		notifyTestCmd(os.Args[2:])
	case "version", "-v", "--version":
		fmt.Println("kidproxy", version)
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `kidproxy — TLS-intercepting filter for the kids network

  kidproxy run   [--config /etc/kidproxy/config.yaml]
  kidproxy check [--config /etc/kidproxy/config.yaml]
  kidproxy ca init [--dir /etc/kidproxy]
  kidproxy ca show [--cert /etc/kidproxy/ca.crt]
  kidproxy ca mobileconfig [--cert ...] [--out file]
  kidproxy notify-test [--config /etc/kidproxy/config.yaml]
  kidproxy query [--kind events] [--since 24h] [--host x] [--q text] [--limit 100]`)
	os.Exit(2)
}

func runCmd(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	cfgPath := fs.String("config", "/etc/kidproxy/config.yaml", "config file")
	_ = fs.Parse(args)

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		fail(err)
	}
	log.SetFlags(log.LstdFlags)
	log.Printf("kidproxy %s starting", version)

	ca, err := loadCA(cfg.CA.Cert, cfg.CA.Key, cfg.CA.LeafDays)
	if err != nil {
		fail(err)
	}
	store, err := openStore(cfg)
	if err != nil {
		fail(err)
	}
	defer store.Close()

	notifier := newNotifier(cfg)
	go notifier.Run(store.Alerts())

	p := newProxy(cfg, ca, store, store.rules)

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		log.Printf("shutting down")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = p.srv.Shutdown(ctx)
		store.Close()
		os.Exit(0)
	}()

	if err := p.Serve(); err != nil {
		fail(err)
	}
}

func checkCmd(args []string) {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	cfgPath := fs.String("config", "/etc/kidproxy/config.yaml", "config file")
	_ = fs.Parse(args)
	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		fail(err)
	}
	ca, err := loadCA(cfg.CA.Cert, cfg.CA.Key, cfg.CA.LeafDays)
	if err != nil {
		fail(err)
	}
	fmt.Printf("config:   %s\n", *cfgPath)
	fmt.Printf("data:     %s\n", cfg.DataDir)
	fmt.Printf("listen:   transparent=%q explicit=%q\n", cfg.Listen.Transparent, cfg.Listen.Explicit)
	fmt.Printf("ca:       %s (serial %s, expires %s)\n", cfg.CA.Cert, ca.fingerprint(), ca.cert.NotAfter.Format("2006-01-02"))
	fmt.Printf("bypass:   %d hosts\n", len(cfg.Bypass))
	fmt.Printf("blocked:  %d domains\n", len(cfg.Block.Domains))
	fmt.Printf("keywords: %d\n", len(cfg.Block.Keywords))
	fmt.Printf("email:    enabled=%v to=%v\n", cfg.Notify.Email.Enabled, cfg.Notify.Email.To)
}

func notifyTestCmd(args []string) {
	fs := flag.NewFlagSet("notify-test", flag.ExitOnError)
	cfgPath := fs.String("config", "/etc/kidproxy/config.yaml", "config file")
	kind := fs.String("kind", "test", "alert kind")
	detail := fs.String("detail", "test alert from kidproxy — email delivery works", "alert detail")
	_ = fs.Parse(args)
	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		fail(err)
	}
	if len(cfg.Notify.Email.To) == 0 {
		fail(fmt.Errorf("notify.email.to is empty in %s", *cfgPath))
	}
	n := newNotifier(cfg)
	a := &Alert{
		TS: time.Now(), Severity: "warn", Kind: *kind, Device: hostname(),
		Host: "router", Detail: *detail,
	}
	if err := n.send(a); err != nil {
		fail(err)
	}
	fmt.Println("test email sent to", cfg.Notify.Email.To)
}

func caCmd(args []string) {
	if len(args) == 0 {
		usage()
	}
	switch args[0] {
	case "init":
		fs := flag.NewFlagSet("ca init", flag.ExitOnError)
		dir := fs.String("dir", "/etc/kidproxy", "CA directory")
		_ = fs.Parse(args[1:])
		if err := initCA(*dir); err != nil {
			fail(err)
		}
		fmt.Printf("created %s and %s (key is 0600)\n", filepath.Join(*dir, "ca.crt"), filepath.Join(*dir, "ca.key"))
		fmt.Println("Back up the key offline; install ca.crt on the kids' devices.")
	case "show":
		fs := flag.NewFlagSet("ca show", flag.ExitOnError)
		cert := fs.String("cert", "/etc/kidproxy/ca.crt", "CA cert")
		_ = fs.Parse(args[1:])
		c, err := readCACert(*cert)
		if err != nil {
			fail(err)
		}
		fmt.Printf("subject:     %s\n", c.Subject.String())
		fmt.Printf("serial:      %X\n", c.SerialNumber)
		fmt.Printf("not before:  %s\n", c.NotBefore.Format(time.RFC3339))
		fmt.Printf("not after:   %s\n", c.NotAfter.Format(time.RFC3339))
		fmt.Printf("sha256:      %X\n", sha256.Sum256(c.Raw))
	case "mobileconfig":
		fs := flag.NewFlagSet("ca mobileconfig", flag.ExitOnError)
		cert := fs.String("cert", "/etc/kidproxy/ca.crt", "CA cert")
		key := fs.String("key", "/etc/kidproxy/ca.key", "CA key")
		out := fs.String("out", "", "output file (default stdout)")
		_ = fs.Parse(args[1:])
		ca, err := loadCA(*cert, *key, 7)
		if err != nil {
			fail(err)
		}
		b := mobileconfig(ca)
		if *out == "" {
			_, _ = os.Stdout.Write(b)
			return
		}
		if err := os.WriteFile(*out, b, 0o644); err != nil {
			fail(err)
		}
		fmt.Println("wrote", *out)
	default:
		usage()
	}
}
