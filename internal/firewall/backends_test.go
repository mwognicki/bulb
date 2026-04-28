package firewall

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestNewBackend_SupportsAdditionalBackends(t *testing.T) {
	for _, name := range []string{"iptables", "nftables"} {
		t.Run(name, func(t *testing.T) {
			backend, err := NewBackend(name, BackendOptions{
				Zone:      "public",
				StateFile: "/tmp/state.json",
			})
			if err != nil {
				t.Fatalf("new backend: %v", err)
			}
			if backend == nil {
				t.Fatal("expected backend")
			}
		})
	}
}

func TestIPTablesBackend_ApplyBuildsExpectedCommands(t *testing.T) {
	runner := &recordingRunner{
		failures: map[string]error{
			"iptables -w -N BULB-INPUT":           errors.New("already exists"),
			"ip6tables -w -N BULB-INPUT":          errors.New("already exists"),
			"iptables -w -C INPUT -j BULB-INPUT":  errors.New("missing"),
			"ip6tables -w -C INPUT -j BULB-INPUT": errors.New("missing"),
		},
	}
	backend := &IPTablesBackend{runner: runner}

	err := backend.Apply(context.Background(), []PortSpec{
		{Port: 8443, Protocol: corev1.ProtocolTCP},
		{Port: 5353, Protocol: corev1.ProtocolUDP},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}

	assertContainsCommand(t, runner.commands, "iptables -w -I INPUT 1 -j BULB-INPUT")
	assertContainsCommand(t, runner.commands, "ip6tables -w -I INPUT 1 -j BULB-INPUT")
	assertContainsCommand(t, runner.commands, "iptables -w -A BULB-INPUT -p tcp --dport 8443 -j ACCEPT")
	assertContainsCommand(t, runner.commands, "iptables -w -A BULB-INPUT -p udp --dport 5353 -j ACCEPT")
	assertContainsCommand(t, runner.commands, "ip6tables -w -A BULB-INPUT -p tcp --dport 8443 -j ACCEPT")
	assertContainsCommand(t, runner.commands, "ip6tables -w -A BULB-INPUT -p udp --dport 5353 -j ACCEPT")
}

func TestNFTablesBackend_ApplyRendersRuleset(t *testing.T) {
	runner := &recordingRunner{
		failures: map[string]error{
			"nft delete table inet bulb": errors.New("No such table"),
		},
	}
	backend := &NFTablesBackend{runner: runner}

	err := backend.Apply(context.Background(), []PortSpec{
		{Port: 8443, Protocol: corev1.ProtocolTCP},
		{Port: 5353, Protocol: corev1.ProtocolUDP},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(runner.inputs) != 1 {
		t.Fatalf("expected 1 input script, got %d", len(runner.inputs))
	}
	script := runner.inputs[0]
	for _, want := range []string{
		"table inet bulb",
		"elements = { 8443 }",
		"elements = { 5353 }",
		"tcp dport @tcp_ports accept",
		"udp dport @udp_ports accept",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("ruleset missing %q:\n%s", want, script)
		}
	}
	assertContainsCommand(t, runner.commands, "nft delete table inet bulb")
	assertContainsCommand(t, runner.commands, "nft -f -")
}

type recordingRunner struct {
	commands []string
	inputs   []string
	failures map[string]error
}

func (r *recordingRunner) Run(_ context.Context, name string, args ...string) error {
	cmd := strings.TrimSpace(name + " " + strings.Join(args, " "))
	r.commands = append(r.commands, cmd)
	if r.failures != nil {
		if err, ok := r.failures[cmd]; ok {
			return err
		}
	}
	return nil
}

func (r *recordingRunner) RunInput(_ context.Context, input string, name string, args ...string) error {
	cmd := strings.TrimSpace(name + " " + strings.Join(args, " "))
	r.commands = append(r.commands, cmd)
	r.inputs = append(r.inputs, input)
	if r.failures != nil {
		if err, ok := r.failures[cmd]; ok {
			return err
		}
	}
	return nil
}

func assertContainsCommand(t *testing.T, commands []string, want string) {
	t.Helper()
	for _, got := range commands {
		if got == want {
			return
		}
	}
	t.Fatalf("missing command %q in %+v", want, commands)
}
