package firewall

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/godbus/dbus/v5"
	corev1 "k8s.io/api/core/v1"
)

const (
	firewalldBusName   = "org.fedoraproject.FirewallD1"
	firewalldObject    = "/org/fedoraproject/FirewallD1"
	firewalldZoneIface = "org.fedoraproject.FirewallD1.zone"
)

type FirewalldBackendOptions struct {
	Zone      string
	StateFile string
}

type FirewalldBackend struct {
	zone  string
	store StateStore
	dbus  firewalldZoneClient
}

type firewalldZoneClient interface {
	GetPorts(ctx context.Context, zone string) ([]PortSpec, error)
	AddPort(ctx context.Context, zone string, port PortSpec) error
	RemovePort(ctx context.Context, zone string, port PortSpec) error
}

func NewFirewalldBackend(opts FirewalldBackendOptions) (*FirewalldBackend, error) {
	if opts.Zone == "" {
		return nil, fmt.Errorf("firewalld zone is required")
	}
	conn, err := dbus.ConnectSystemBus()
	if err != nil {
		return nil, fmt.Errorf("connect system bus: %w", err)
	}
	return &FirewalldBackend{
		zone:  opts.Zone,
		store: FileStateStore{Path: opts.StateFile},
		dbus:  firewalldDBusClient{obj: conn.Object(firewalldBusName, dbus.ObjectPath(firewalldObject))},
	}, nil
}

func (b *FirewalldBackend) Name() string { return "firewalld" }

func (b *FirewalldBackend) Validate(ctx context.Context) error {
	if b.zone == "" {
		return fmt.Errorf("firewalld zone is required")
	}
	if b.store == nil {
		return fmt.Errorf("state store is required")
	}
	if b.dbus == nil {
		return fmt.Errorf("firewalld D-Bus client is required")
	}
	if _, err := b.dbus.GetPorts(ctx, b.zone); err != nil {
		return fmt.Errorf("query zone %s ports: %w", b.zone, err)
	}
	return nil
}

func (b *FirewalldBackend) Apply(ctx context.Context, desired []PortSpec) error {
	managedKeys, err := b.store.Load()
	if err != nil {
		return err
	}
	managed := specsFromKeys(managedKeys)
	current, err := b.dbus.GetPorts(ctx, b.zone)
	if err != nil {
		return err
	}

	toAdd := diffPorts(desired, current)
	trackedCurrent := intersectPorts(managed, current)
	toRemove := diffPorts(trackedCurrent, desired)

	for _, port := range toAdd {
		if err := b.dbus.AddPort(ctx, b.zone, port); err != nil {
			return fmt.Errorf("add %s to zone %s: %w", port, b.zone, err)
		}
	}
	for _, port := range toRemove {
		if err := b.dbus.RemovePort(ctx, b.zone, port); err != nil {
			return fmt.Errorf("remove %s from zone %s: %w", port, b.zone, err)
		}
	}

	nextManaged := nextManagedPorts(managed, desired, current, toAdd)
	if err := b.store.Save(portKeys(nextManaged)); err != nil {
		return err
	}
	return nil
}

func diffPorts(left, right []PortSpec) []PortSpec {
	rightSet := make(map[PortKey]struct{}, len(right))
	for _, port := range right {
		rightSet[port.key()] = struct{}{}
	}
	out := make([]PortSpec, 0, len(left))
	for _, port := range left {
		if _, ok := rightSet[port.key()]; !ok {
			out = append(out, PortSpec{Port: port.Port, Protocol: port.Protocol})
		}
	}
	return stableUniquePorts(out)
}

func intersectPorts(left, right []PortSpec) []PortSpec {
	rightSet := make(map[PortKey]struct{}, len(right))
	for _, port := range right {
		rightSet[port.key()] = struct{}{}
	}
	out := make([]PortSpec, 0, len(left))
	for _, port := range left {
		if _, ok := rightSet[port.key()]; ok {
			out = append(out, PortSpec{Port: port.Port, Protocol: port.Protocol})
		}
	}
	return stableUniquePorts(out)
}

func nextManagedPorts(previousManaged, desired, current, added []PortSpec) []PortSpec {
	desiredSet := make(map[PortKey]struct{}, len(desired))
	for _, port := range desired {
		desiredSet[port.key()] = struct{}{}
	}
	currentSet := make(map[PortKey]struct{}, len(current))
	for _, port := range current {
		currentSet[port.key()] = struct{}{}
	}

	next := make([]PortSpec, 0, len(previousManaged)+len(added))
	for _, port := range previousManaged {
		if _, desiredNow := desiredSet[port.key()]; !desiredNow {
			continue
		}
		if _, stillPresent := currentSet[port.key()]; stillPresent {
			next = append(next, PortSpec{Port: port.Port, Protocol: port.Protocol})
		}
	}
	for _, port := range added {
		next = append(next, PortSpec{Port: port.Port, Protocol: port.Protocol})
	}
	return stableUniquePorts(next)
}

func stableUniquePorts(ports []PortSpec) []PortSpec {
	seen := make(map[PortKey]struct{}, len(ports))
	out := make([]PortSpec, 0, len(ports))
	for _, port := range ports {
		base := PortSpec{Port: port.Port, Protocol: port.Protocol}
		if _, ok := seen[base.key()]; ok {
			continue
		}
		seen[base.key()] = struct{}{}
		out = append(out, base)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Port != out[j].Port {
			return out[i].Port < out[j].Port
		}
		return out[i].Protocol < out[j].Protocol
	})
	return out
}

type firewalldDBusClient struct {
	obj dbus.BusObject
}

func (c firewalldDBusClient) GetPorts(ctx context.Context, zone string) ([]PortSpec, error) {
	var raw [][]string
	call := c.obj.CallWithContext(ctx, firewalldZoneIface+".getPorts", 0, zone)
	if call.Err != nil {
		return nil, call.Err
	}
	if err := call.Store(&raw); err != nil {
		return nil, fmt.Errorf("store getPorts result: %w", err)
	}
	ports := make([]PortSpec, 0, len(raw))
	for _, entry := range raw {
		if len(entry) != 2 {
			continue
		}
		ports = append(ports, PortSpec{
			Port:     parsePort(entry[0]),
			Protocol: protocolFromString(entry[1]),
		})
	}
	return stableUniquePorts(ports), nil
}

func (c firewalldDBusClient) AddPort(ctx context.Context, zone string, port PortSpec) error {
	call := c.obj.CallWithContext(ctx, firewalldZoneIface+".addPort", 0, zone, fmt.Sprintf("%d", port.Port), protocolString(port.Protocol), int32(0))
	return call.Err
}

func (c firewalldDBusClient) RemovePort(ctx context.Context, zone string, port PortSpec) error {
	call := c.obj.CallWithContext(ctx, firewalldZoneIface+".removePort", 0, zone, fmt.Sprintf("%d", port.Port), protocolString(port.Protocol))
	return call.Err
}

func protocolString(protocol corev1.Protocol) string {
	if protocol == "" {
		return "tcp"
	}
	return strings.ToLower(string(protocol))
}

func protocolFromString(protocol string) corev1.Protocol {
	switch strings.ToUpper(protocol) {
	case "UDP":
		return corev1.ProtocolUDP
	default:
		return corev1.ProtocolTCP
	}
}

func parsePort(raw string) int32 {
	var port int32
	fmt.Sscanf(raw, "%d", &port)
	return port
}
