// Command pi-msg bridges the Pi coding agent (`pi --mode rpc`) to XMPP, so the
// agent can be driven from a chat client. See README.md.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

func main() {
	if hasArg("--discover") {
		if err := runDiscover(); err != nil {
			fmt.Fprintf(os.Stderr, "[pi-msg] %v\n", err)
			os.Exit(1)
		}
		return
	}
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "[pi-msg] %v\n", err)
		os.Exit(1)
	}
}

// hasArg reports whether any command-line argument equals want.
func hasArg(want string) bool {
	for _, a := range os.Args[1:] {
		if a == want {
			return true
		}
	}
	return false
}

// runDiscover discovers the local Bonjour XMPP service, lists every user
// currently online, and prints the account fields (jid/owner) to use, so config
// can be written without guessing. It honours the default account's
// bonjourService/bonjourName/discoverTimeout when a config exists, else the
// built-in defaults.
func runDiscover() error {
	service, name, timeout := defaultBonjourService, "", defaultDiscoverTimeout
	if cfg, err := loadConfig(configPath()); err == nil {
		if acct, ok := cfg.Accounts[defaultAccount]; ok {
			if acct.BonjourService != "" {
				service = acct.BonjourService
			}
			name = strings.TrimSpace(acct.BonjourName)
			if acct.DiscoverTimeout != "" {
				if d, err := time.ParseDuration(acct.DiscoverTimeout); err == nil {
					timeout = d
				}
			}
		}
	}

	instances, err := discoverBonjourInstances(context.Background(), service, name, timeout)
	if err != nil {
		return err
	}
	fmt.Printf("discovered %d %s service(s) on the local network:\n", len(instances), service)
	for _, inst := range instances {
		host := strings.TrimSuffix(inst.Host, ".")
		fmt.Printf("  %-20s %s:%d\n", unescapeInstance(inst.Instance), host, inst.Port)
	}
	fmt.Println("suggested account config:")
	first := instances[0]
	user, domain, _ := strings.Cut(unescapeInstance(first.Instance), "@")
	if domain == "" {
		domain = strings.TrimSuffix(first.Host, ".")
	}
	fmt.Printf("  \"jid\":   \"pi@%s\"\n", domain)
	if user != "" {
		fmt.Printf("  \"owner\": \"%s@%s\"\n", user, domain)
	} else {
		fmt.Printf("  \"owner\": \"<you>@%s\"\n", domain)
	}
	return nil
}

// unescapeInstance undoes DNS-SD's escaping of special characters in service
// instance names, so "crn\\@mbpro" reads as the JID "crn@mbpro".
func unescapeInstance(name string) string {
	return strings.ReplaceAll(name, `\@`, "@")
}

// escapeInstance escapes the special characters DNS-SD forbids in a service
// instance name, so the JID "pi@mbpro" is advertised as "pi\@mbpro" and
// round-trips through a peer's mDNS parser.
func escapeInstance(name string) string {
	r := strings.NewReplacer(`@`, `\@`, `.`, `\.`, `,`, `\,`, `"`, `\"`, `\`, `\\`)
	return r.Replace(name)
}

func run() error {
	cfg, err := loadConfig(configPath())
	if err != nil {
		if errors.Is(err, errNoConfig) {
			return fmt.Errorf("%w — nothing to do. See README for setup", err)
		}
		return err
	}
	acct, err := resolveAccount(cfg, os.Getenv("PI_MSG_ACCOUNT"))
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	debug := os.Getenv("PI_MSG_DEBUG") != ""
	return NewBridge(acct, debug).Run(ctx)
}
