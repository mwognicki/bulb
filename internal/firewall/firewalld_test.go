package firewall

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestFirewalldBackend_Apply_AddsAndRemovesTrackedPorts(t *testing.T) {
	client := &fakeFirewalldClient{
		current: []PortSpec{
			{Port: 8080, Protocol: corev1.ProtocolTCP},
			{Port: 9999, Protocol: corev1.ProtocolTCP},
		},
	}
	store := &memoryStateStore{
		keys: []PortKey{
			{Port: 8080, Protocol: "tcp"},
			{Port: 9999, Protocol: "tcp"},
		},
	}
	backend := &FirewalldBackend{
		zone:  "public",
		store: store,
		dbus:  client,
	}

	err := backend.Apply(context.Background(), []PortSpec{
		{Port: 8080, Protocol: corev1.ProtocolTCP},
		{Port: 8443, Protocol: corev1.ProtocolTCP},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}

	if len(client.added) != 1 || client.added[0].Port != 8443 {
		t.Fatalf("added: %+v", client.added)
	}
	if len(client.removed) != 1 || client.removed[0].Port != 9999 {
		t.Fatalf("removed: %+v", client.removed)
	}
	if got, want := specsFromKeys(store.saved), []PortSpec{
		{Port: 8080, Protocol: corev1.ProtocolTCP},
		{Port: 8443, Protocol: corev1.ProtocolTCP},
	}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("saved state: got %+v want %+v", got, want)
	}
}

func TestFirewalldBackend_Apply_DoesNotClaimPreExistingOperatorRule(t *testing.T) {
	client := &fakeFirewalldClient{
		current: []PortSpec{{Port: 8443, Protocol: corev1.ProtocolTCP}},
	}
	store := &memoryStateStore{}
	backend := &FirewalldBackend{
		zone:  "public",
		store: store,
		dbus:  client,
	}

	if err := backend.Apply(context.Background(), []PortSpec{{Port: 8443, Protocol: corev1.ProtocolTCP}}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(store.saved) != 0 {
		t.Fatalf("expected empty managed state, got %+v", store.saved)
	}
}

func TestFirewalldBackend_Apply_PropagatesClientError(t *testing.T) {
	backend := &FirewalldBackend{
		zone:  "public",
		store: &memoryStateStore{},
		dbus:  &fakeFirewalldClient{getErr: errors.New("dbus down")},
	}
	if err := backend.Apply(context.Background(), []PortSpec{{Port: 8443, Protocol: corev1.ProtocolTCP}}); err == nil {
		t.Fatal("expected error")
	}
}

func TestFirewalldBackend_Apply_PrunesStateWhenTrackedRuleDriftedAway(t *testing.T) {
	client := &fakeFirewalldClient{
		current: []PortSpec{{Port: 8080, Protocol: corev1.ProtocolTCP}},
	}
	store := &memoryStateStore{
		keys: []PortKey{
			{Port: 8080, Protocol: "tcp"},
			{Port: 9999, Protocol: "tcp"},
		},
	}
	backend := &FirewalldBackend{
		zone:  "public",
		store: store,
		dbus:  client,
	}

	if err := backend.Apply(context.Background(), []PortSpec{{Port: 8080, Protocol: corev1.ProtocolTCP}}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(client.removed) != 0 {
		t.Fatalf("expected no remove call for already-drifted port, got %+v", client.removed)
	}
	if got, want := specsFromKeys(store.saved), []PortSpec{{Port: 8080, Protocol: corev1.ProtocolTCP}}; len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("saved state: got %+v want %+v", got, want)
	}
}

type fakeFirewalldClient struct {
	current []PortSpec
	added   []PortSpec
	removed []PortSpec
	getErr  error
	addErr  error
	delErr  error
}

func (c *fakeFirewalldClient) GetPorts(_ context.Context, _ string) ([]PortSpec, error) {
	if c.getErr != nil {
		return nil, c.getErr
	}
	return append([]PortSpec(nil), c.current...), nil
}

func (c *fakeFirewalldClient) AddPort(_ context.Context, _ string, port PortSpec) error {
	if c.addErr != nil {
		return c.addErr
	}
	c.added = append(c.added, PortSpec{Port: port.Port, Protocol: port.Protocol})
	c.current = append(c.current, PortSpec{Port: port.Port, Protocol: port.Protocol})
	return nil
}

func (c *fakeFirewalldClient) RemovePort(_ context.Context, _ string, port PortSpec) error {
	if c.delErr != nil {
		return c.delErr
	}
	c.removed = append(c.removed, PortSpec{Port: port.Port, Protocol: port.Protocol})
	next := make([]PortSpec, 0, len(c.current))
	for _, current := range c.current {
		if current.key() == port.key() {
			continue
		}
		next = append(next, current)
	}
	c.current = next
	return nil
}

type memoryStateStore struct {
	keys  []PortKey
	saved []PortKey
	err   error
}

func (s *memoryStateStore) Load() ([]PortKey, error) {
	if s.err != nil {
		return nil, s.err
	}
	return append([]PortKey(nil), s.keys...), nil
}

func (s *memoryStateStore) Save(keys []PortKey) error {
	if s.err != nil {
		return s.err
	}
	s.saved = append([]PortKey(nil), keys...)
	s.keys = append([]PortKey(nil), keys...)
	return nil
}
