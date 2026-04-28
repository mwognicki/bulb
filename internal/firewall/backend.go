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

type DryRunBackend struct {
	inner Backend
}

func NewDryRunBackend(inner Backend) (*DryRunBackend, error) {
	if inner == nil {
		return nil, fmt.Errorf("inner backend is required")
	}
	return &DryRunBackend{inner: inner}, nil
}

func (b *DryRunBackend) Name() string {
	return b.inner.Name()
}

func (b *DryRunBackend) Validate(ctx context.Context) error {
	return b.inner.Validate(ctx)
}

func (b *DryRunBackend) Apply(_ context.Context, _ []PortSpec) error {
	return nil
}
