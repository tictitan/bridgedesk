package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"syscall"
)

func main() {
	platformInit()

	// CLI subcommands (print to the parent console on Windows).
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "key", "info", "status", "--status":
			attachParentConsole()
			cfg, err := loadConfig()
			if err != nil {
				fmt.Println("error:", err)
				return
			}
			_, fp, _ := loadOrCreateCert()
			fmt.Printf("BridgeDesk %s on %s\n", appVersion, cfg.Name)
			fmt.Printf("Access key:  %s\n", cfg.AccessKey)
			fmt.Printf("Fingerprint: %s\n", prettyFP(fp))
			fmt.Printf("LAN port:    %d   Local port: %d\n", cfg.LANPort, cfg.LocalPort)
			if runtime.GOOS == "windows" {
				admin := "no"
				if isElevated() {
					admin = "yes"
				}
				fmt.Printf("Administrator: %s\n", admin)
			}
			for _, ip := range localIPs() {
				fmt.Printf("  phone URL: https://%s:%d/\n", ip, cfg.LANPort)
			}
			return
		case "elevate", "admin":
			attachParentConsole()
			if isElevated() {
				fmt.Println("already running as administrator")
			} else if relaunchElevated() {
				fmt.Println("launching an elevated instance…")
			} else {
				fmt.Println("elevation was declined")
			}
			return
		case "newkey":
			attachParentConsole()
			cfg, _ := loadConfig()
			cfg.AccessKey = randomKey(16)
			cfg.Save()
			fmt.Println("new access key:", cfg.AccessKey)
			return
		case "google":
			// bridgedesk google on <email>[,<email2>...]   |   google off   |   google status
			attachParentConsole()
			cfg, _ := loadConfig()
			sub := ""
			if len(os.Args) > 2 {
				sub = os.Args[2]
			}
			switch sub {
			case "on":
				if len(os.Args) > 3 {
					if list, ok := strList(os.Args[3]); ok {
						cfg.GoogleEmails = list
					}
				}
				if len(cfg.GoogleEmails) == 0 {
					fmt.Println("usage: bridgedesk google on you@gmail.com[,other@domain]")
					return
				}
				cfg.RequireGoogle = true
				cfg.Save()
				fmt.Printf("Google sign-in REQUIRED. Allowed: %s\n", strings.Join(cfg.GoogleEmails, ", "))
			case "off":
				cfg.RequireGoogle = false
				cfg.Save()
				fmt.Println("Google sign-in requirement OFF (access key + TLS pin still apply).")
			default:
				fmt.Printf("Google sign-in: %v\nAllowed emails: %s\nAccepted client IDs: %s\n",
					cfg.RequireGoogle, strings.Join(cfg.GoogleEmails, ", "), strings.Join(cfg.GoogleClients, ", "))
			}
			return
		case "-h", "--help", "help":
			attachParentConsole()
			fmt.Println(helpText)
			return
		}
	}

	cfg, err := loadConfig()
	if err != nil {
		fatal(err)
	}
	cert, fp, err := loadOrCreateCert()
	if err != nil {
		fatal(err)
	}
	local := NewLocalDevice(cfg)
	hub := NewHub(local)
	srv := NewServer(cfg, hub, cert, fp)

	log.SetFlags(log.Ltime)
	log.Printf("BridgeDesk %s — %s (%s)", appVersion, cfg.Name, runtime.GOOS)
	log.Printf("access key: %s", cfg.AccessKey)
	log.Printf("fingerprint: %s", prettyFP(fp))
	if err := srv.Run(); err != nil {
		fatal(err)
	}

	background := len(os.Args) > 1 && os.Args[1] == "--background"

	done := make(chan struct{})
	var once sync.Once
	quit := func() { once.Do(func() { close(done) }) }

	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() { <-ch; quit() }()

	if runtime.GOOS == "windows" {
		ensureFirewall(cfg.LANPort) // let other PCs/phones reach this hub
		// Native tray icon is the management entry point; open the dashboard on first launch.
		go runTray(srv, quit)
		if !background {
			openTarget(fmt.Sprintf("http://127.0.0.1:%d/?key=%s", cfg.LocalPort, cfg.AccessKey))
		}
	}

	<-done
	log.Println("shutting down")
}

func fatal(err error) {
	log.Printf("fatal: %v", err)
	if runtime.GOOS == "windows" {
		showMessage("BridgeDesk error", err.Error())
	}
	os.Exit(1)
}

const helpText = `BridgeDesk ` + appVersion + ` — remote shell / files / desktop between your PC and phone.

Usage:
  bridgedesk              start the hub (dashboard + LAN server)
  bridgedesk --background start without the info dialog (used by autostart)
  bridgedesk status       print access key, fingerprint and connection URLs
  bridgedesk newkey       generate a fresh access key
  bridgedesk help         show this help

Config & certificate live in the BridgeDesk app-data folder.`
