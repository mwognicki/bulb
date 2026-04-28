package firewall

import (
	"context"
	"fmt"
)

type Backend interface {
	Name() string
	Validate(ctx context.Context) error
	Apply(ctx context.Context, desired []PortSpec) error
}

type BackendOptions struct {
	Zone      string
	StateFile string
}

func NewBackend(name string, opts BackendOptions) (Backend, error) {
	switch name {
	case "firewalld":
		return NewFirewalldBackend(FirewalldBackendOptions{
			Zone:      opts.Zone,
			StateFile: opts.StateFile,
		})
	case "iptables":
		return NewIPTablesBackend(IPTablesBackendOptions{})
	case "nftables":
		return NewNFTablesBackend(NFTablesBackendOptions{})
	default:
		return nil, fmt.Errorf("unsupported firewall backend %q", name)
	}
}
