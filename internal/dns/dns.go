// Package dns implements the bulb dns-agent. It reconciles DNSRecord
// CRs against a configurable provider (Cloudflare to start) and runs
// active TCP health checks against published targets.
package dns

import "errors"

func Run(args []string) error {
	return errors.New("not yet implemented (Phase 3)")
}
