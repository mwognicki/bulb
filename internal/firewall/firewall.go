// Package firewall implements the bulb firewall-agent. It reconciles
// firewalld public-zone rules from LBPort CRs via the firewalld D-Bus
// API (org.fedoraproject.FirewallD1).
package firewall

import "errors"

func Run(args []string) error {
	return errors.New("not yet implemented (Phase 2)")
}
