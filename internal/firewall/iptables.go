package firewall

import (
	"context"
	"fmt"
	"strings"
)

const (
	iptablesChain = "BULB-INPUT"
)

type IPTablesBackendOptions struct{}

type IPTablesBackend struct {
	runner CommandRunner
}

func NewIPTablesBackend(_ IPTablesBackendOptions) (*IPTablesBackend, error) {
	return &IPTablesBackend{runner: ExecRunner{}}, nil
}

func (b *IPTablesBackend) Name() string { return "iptables" }

func (b *IPTablesBackend) Apply(ctx context.Context, desired []PortSpec) error {
	if err := b.ensureChain(ctx, "iptables"); err != nil {
		return err
	}
	if err := b.ensureChain(ctx, "ip6tables"); err != nil {
		return err
	}
	for _, binary := range []string{"iptables", "ip6tables"} {
		if err := b.runner.Run(ctx, binary, "-w", "-F", iptablesChain); err != nil {
			return fmt.Errorf("flush %s chain: %w", binary, err)
		}
		for _, port := range stableUniquePorts(desired) {
			if err := b.runner.Run(ctx, binary, "-w", "-A", iptablesChain, "-p", protocolString(port.Protocol), "--dport", fmt.Sprintf("%d", port.Port), "-j", "ACCEPT"); err != nil {
				return fmt.Errorf("append %s rule for %s: %w", binary, port, err)
			}
		}
	}
	return nil
}

func (b *IPTablesBackend) ensureChain(ctx context.Context, binary string) error {
	if err := b.runner.Run(ctx, binary, "-w", "-N", iptablesChain); err != nil && !isAlreadyExistsError(err) {
		return fmt.Errorf("create %s chain: %w", binary, err)
	}
	checkArgs := []string{"-w", "-C", "INPUT", "-j", iptablesChain}
	if err := b.runner.Run(ctx, binary, checkArgs...); err == nil {
		return nil
	}
	insertArgs := []string{"-w", "-I", "INPUT", "1", "-j", iptablesChain}
	if err := b.runner.Run(ctx, binary, insertArgs...); err != nil {
		return fmt.Errorf("insert %s jump: %w", binary, err)
	}
	return nil
}

func isAlreadyExistsError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "Chain already exists") || strings.Contains(msg, "already exists")
}
