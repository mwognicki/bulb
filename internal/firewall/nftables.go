package firewall

import (
	"context"
	"fmt"
	"strings"
)

const (
	nftTableName  = "bulb_firewall_agent"
	nftChainName  = "input"
	nftTCPSetName = "tcp_ports"
	nftUDPSetName = "udp_ports"
)

const nftRulesetTemplate = `table inet ` + nftTableName + ` {
	set ` + nftTCPSetName + ` {
		type inet_service
		elements = { %s }
	}

	set ` + nftUDPSetName + ` {
		type inet_service
		elements = { %s }
	}

	chain ` + nftChainName + ` {
		type filter hook input priority 0;
		tcp dport @` + nftTCPSetName + ` accept
		udp dport @` + nftUDPSetName + ` accept
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

func (b *NFTablesBackend) Validate(ctx context.Context) error {
	if b.runner == nil {
		return fmt.Errorf("command runner is required")
	}
	if _, err := b.runner.Output(ctx, "nft", "list", "tables"); err != nil {
		return fmt.Errorf("list nftables tables: %w", err)
	}
	return nil
}

func (b *NFTablesBackend) Apply(ctx context.Context, desired []PortSpec) error {
	out, err := b.runner.Output(ctx, "nft", "list", "table", "inet", nftTableName)
	if err != nil && !isNFTNoSuchTable(err) {
		return fmt.Errorf("inspect nftables table: %w", err)
	}
	if strings.TrimSpace(out) != "" && !strings.Contains(out, "table inet "+nftTableName+" {") {
		return fmt.Errorf("unexpected nftables table output for %s", nftTableName)
	}

	tcpPorts, udpPorts := splitPortsByProtocol(desired)
	ruleset := fmt.Sprintf(nftRulesetTemplate, joinPorts(tcpPorts), joinPorts(udpPorts))
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
