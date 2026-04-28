package firewall

import (
	"context"
	"fmt"
	"strings"
)

const nftRulesetTemplate = `table inet bulb {
	set tcp_ports {
		type inet_service
		elements = { %s }
	}

	set udp_ports {
		type inet_service
		elements = { %s }
	}

	chain input {
		type filter hook input priority 0;
		tcp dport @tcp_ports accept
		udp dport @udp_ports accept
	}
}
`

type NFTablesBackendOptions struct{}

type NFTablesBackend struct {
	runner CommandRunner
}

func NewNFTablesBackend(_ NFTablesBackendOptions) (*NFTablesBackend, error) {
	return &NFTablesBackend{runner: ExecRunner{}}, nil
}

func (b *NFTablesBackend) Name() string { return "nftables" }

func (b *NFTablesBackend) Apply(ctx context.Context, desired []PortSpec) error {
	tcpPorts, udpPorts := splitPortsByProtocol(desired)
	ruleset := fmt.Sprintf(nftRulesetTemplate, joinPorts(tcpPorts), joinPorts(udpPorts))
	if err := b.runner.Run(ctx, "nft", "delete", "table", "inet", "bulb"); err != nil && !isNFTNoSuchTable(err) {
		return fmt.Errorf("delete existing nftables table: %w", err)
	}
	if err := b.runner.RunInput(ctx, ruleset, "nft", "-f", "-"); err != nil {
		return fmt.Errorf("apply nftables ruleset: %w", err)
	}
	return nil
}

func splitPortsByProtocol(desired []PortSpec) (tcp []int32, udp []int32) {
	for _, port := range stableUniquePorts(desired) {
		switch port.Protocol {
		case "UDP":
			udp = append(udp, port.Port)
		default:
			tcp = append(tcp, port.Port)
		}
	}
	return tcp, udp
}

func joinPorts(ports []int32) string {
	if len(ports) == 0 {
		return ""
	}
	parts := make([]string, 0, len(ports))
	for _, port := range ports {
		parts = append(parts, fmt.Sprintf("%d", port))
	}
	return strings.Join(parts, ", ")
}

func isNFTNoSuchTable(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "No such file or directory") || strings.Contains(msg, "No such table")
}
