// Command bulb is the single-binary entrypoint for all bulb roles.
// Subcommands dispatch into internal packages: controller, proxy,
// firewall-agent, dns-agent. See CLAUDE.md for the architecture.
package main

import (
	"fmt"
	"os"

	"github.com/mwognicki/bulb/internal/controller"
	"github.com/mwognicki/bulb/internal/dns"
	"github.com/mwognicki/bulb/internal/firewall"
	"github.com/mwognicki/bulb/internal/proxy"
)

func main() {
	if len(os.Args) < 2 {
		usage(os.Stderr)
		os.Exit(2)
	}

	cmd, args := os.Args[1], os.Args[2:]

	var err error
	switch cmd {
	case "controller":
		err = controller.Run(args)
	case "proxy":
		err = proxy.Run(args)
	case "firewall-agent":
		err = firewall.Run(args)
	case "dns-agent":
		err = dns.Run(args)
	case "-h", "--help", "help":
		usage(os.Stdout)
		return
	default:
		fmt.Fprintf(os.Stderr, "bulb: unknown subcommand %q\n\n", cmd)
		usage(os.Stderr)
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "bulb %s: %v\n", cmd, err)
		os.Exit(1)
	}
}

func usage(w *os.File) {
	fmt.Fprint(w, `bulb - LoadBalancer controller for clusters without cloud LB

Usage:
  bulb <subcommand> [flags]

Subcommands:
  controller       Watch Services, reconcile per-Service proxy DaemonSets,
                   emit LBPort and DNSRecord CRs.
  proxy            TCP forwarder dataplane (runs in per-Service pods).
  firewall-agent   Reconcile the desired per-node exposure set from
                   LBPort CRs into a concrete firewall backend.
  dns-agent        Publish DNSRecord CRs to a DNS provider (Cloudflare).
`)
}
