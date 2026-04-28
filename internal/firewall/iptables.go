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

func (b *IPTablesBackend) Validate(ctx context.Context) error {
	if b.runner == nil {
		return fmt.Errorf("command runner is required")
	}
	for _, binary := range []string{"iptables", "ip6tables"} {
		if _, err := b.runner.Output(ctx, binary, "-w", "-S", "INPUT"); err != nil {
			return fmt.Errorf("inspect %s INPUT chain: %w", binary, err)
		}
	}
	return nil
}

func (b *IPTablesBackend) Apply(ctx context.Context, desired []PortSpec) error {
	for _, binary := range []string{"iptables", "ip6tables"} {
		if err := b.ensureChain(ctx, binary); err != nil {
			return err
		}
		if err := b.ensureJump(ctx, binary); err != nil {
			return err
		}
		if err := b.reconcileOwnedRules(ctx, binary, desired); err != nil {
			return err
		}
	}
	return nil
}

func (b *IPTablesBackend) ensureChain(ctx context.Context, binary string) error {
	if err := b.runner.Run(ctx, binary, "-w", "-N", iptablesChain); err != nil && !isAlreadyExistsError(err) {
		return fmt.Errorf("create %s chain: %w", binary, err)
	}
	return nil
}

func (b *IPTablesBackend) ensureJump(ctx context.Context, binary string) error {
	out, err := b.runner.Output(ctx, binary, "-w", "-S", "INPUT")
	if err != nil {
		return fmt.Errorf("list %s INPUT chain: %w", binary, err)
	}
	jumpRule := "-A INPUT -j " + iptablesChain
	if countLine(out, jumpRule) == 1 {
		return nil
	}
	if countLine(out, jumpRule) > 1 {
		return fmt.Errorf("%s INPUT chain has multiple %s jumps", binary, iptablesChain)
	}
	if err := b.runner.Run(ctx, binary, "-w", "-I", "INPUT", "1", "-j", iptablesChain); err != nil {
		return fmt.Errorf("insert %s jump: %w", binary, err)
	}
	return nil
}

func (b *IPTablesBackend) reconcileOwnedRules(ctx context.Context, binary string, desired []PortSpec) error {
	out, err := b.runner.Output(ctx, binary, "-w", "-S", iptablesChain)
	if err != nil {
		return fmt.Errorf("list %s owned chain: %w", binary, err)
	}
	current := parseIPTablesChainRules(out)
	want := stableUniquePorts(desired)

	for _, port := range diffPorts(current, want) {
		if err := b.runner.Run(ctx, binary, "-w", "-D", iptablesChain, "-p", protocolString(port.Protocol), "--dport", fmt.Sprintf("%d", port.Port), "-j", "ACCEPT"); err != nil {
			return fmt.Errorf("delete %s rule for %s: %w", binary, port, err)
		}
	}
	for _, port := range diffPorts(want, current) {
		if err := b.runner.Run(ctx, binary, "-w", "-A", iptablesChain, "-p", protocolString(port.Protocol), "--dport", fmt.Sprintf("%d", port.Port), "-j", "ACCEPT"); err != nil {
			return fmt.Errorf("append %s rule for %s: %w", binary, port, err)
		}
	}
	return nil
}

func parseIPTablesChainRules(raw string) []PortSpec {
	lines := strings.Split(strings.TrimSpace(raw), "\n")
	out := make([]PortSpec, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || line == ("-N "+iptablesChain) {
			continue
		}
		fields := strings.Fields(line)
		var port int32
		var protocol string
		for i := 0; i < len(fields); i++ {
			if fields[i] == "-p" && i+1 < len(fields) {
				protocol = fields[i+1]
			}
			if fields[i] == "--dport" && i+1 < len(fields) {
				port = parsePort(fields[i+1])
			}
		}
		if port == 0 {
			continue
		}
		out = append(out, PortSpec{
			Port:     port,
			Protocol: protocolFromString(protocol),
		})
	}
	return stableUniquePorts(out)
}

func countLine(raw, want string) int {
	count := 0
	for _, line := range strings.Split(raw, "\n") {
		if strings.TrimSpace(line) == want {
			count++
		}
	}
	return count
}

func isAlreadyExistsError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "Chain already exists") || strings.Contains(msg, "already exists")
}
