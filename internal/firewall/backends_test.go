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
		outputs: map[string]string{
			"iptables -w -S INPUT":       "-P INPUT ACCEPT",
			"ip6tables -w -S INPUT":      "-P INPUT ACCEPT",
			"iptables -w -S BULB-INPUT":  "-N BULB-INPUT",
			"ip6tables -w -S BULB-INPUT": "-N BULB-INPUT",
		},
		failures: map[string]error{
			"iptables -w -N BULB-INPUT":  errors.New("already exists"),
			"ip6tables -w -N BULB-INPUT": errors.New("already exists"),
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

func TestIPTablesBackend_Apply_RejectsMultipleOwnedJumps(t *testing.T) {
	runner := &recordingRunner{
		outputs: map[string]string{
			"iptables -w -S INPUT":      "-A INPUT -j BULB-INPUT\n-A INPUT -j BULB-INPUT",
			"iptables -w -S BULB-INPUT": "-N BULB-INPUT",
		},
		failures: map[string]error{
			"iptables -w -N BULB-INPUT": errors.New("already exists"),
		},
	}
	backend := &IPTablesBackend{runner: runner}
	if err := backend.Apply(context.Background(), []PortSpec{{Port: 8443, Protocol: corev1.ProtocolTCP}}); err == nil {
		t.Fatal("expected multiple-jump error")
	}
}

func TestIPTablesBackend_Apply_DeletesOnlyStaleOwnedRules(t *testing.T) {
	runner := &recordingRunner{
		outputs: map[string]string{
			"iptables -w -S INPUT":       "-A INPUT -j BULB-INPUT",
			"ip6tables -w -S INPUT":      "-A INPUT -j BULB-INPUT",
			"iptables -w -S BULB-INPUT":  "-N BULB-INPUT\n-A BULB-INPUT -p tcp --dport 8080 -j ACCEPT\n-A BULB-INPUT -p tcp --dport 9999 -j ACCEPT",
			"ip6tables -w -S BULB-INPUT": "-N BULB-INPUT\n-A BULB-INPUT -p tcp --dport 8080 -j ACCEPT\n-A BULB-INPUT -p tcp --dport 9999 -j ACCEPT",
		},
		failures: map[string]error{
			"iptables -w -N BULB-INPUT":  errors.New("already exists"),
			"ip6tables -w -N BULB-INPUT": errors.New("already exists"),
		},
	}
	backend := &IPTablesBackend{runner: runner}
	if err := backend.Apply(context.Background(), []PortSpec{{Port: 8080, Protocol: corev1.ProtocolTCP}}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	assertContainsCommand(t, runner.commands, "iptables -w -D BULB-INPUT -p tcp --dport 9999 -j ACCEPT")
	assertContainsCommand(t, runner.commands, "ip6tables -w -D BULB-INPUT -p tcp --dport 9999 -j ACCEPT")
}

func TestNFTablesBackend_ApplyRendersRuleset(t *testing.T) {
	runner := &recordingRunner{
		outputs: map[string]string{
			"nft list table inet bulb_firewall_agent": "",
		},
		failures: map[string]error{
			"nft list table inet bulb_firewall_agent": errors.New("No such table"),
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
		"table inet bulb_firewall_agent",
		"elements = { 8443 }",
		"elements = { 5353 }",
		"tcp dport @tcp_ports accept",
		"udp dport @udp_ports accept",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("ruleset missing %q:\n%s", want, script)
		}
	}
	assertContainsCommand(t, runner.commands, "nft list table inet bulb_firewall_agent")
	assertContainsCommand(t, runner.commands, "nft -f -")
}

func TestNFTablesBackend_Apply_RejectsUnexpectedOwnedTable(t *testing.T) {
	runner := &recordingRunner{
		outputs: map[string]string{
			"nft list table inet bulb_firewall_agent": "table inet something_else {}",
		},
	}
	backend := &NFTablesBackend{runner: runner}
	if err := backend.Apply(context.Background(), []PortSpec{{Port: 8443, Protocol: corev1.ProtocolTCP}}); err == nil {
		t.Fatal("expected unexpected-table error")
	}
}

type recordingRunner struct {
	commands []string
	inputs   []string
	failures map[string]error
	outputs  map[string]string
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

func (r *recordingRunner) Output(_ context.Context, name string, args ...string) (string, error) {
	cmd := strings.TrimSpace(name + " " + strings.Join(args, " "))
	r.commands = append(r.commands, cmd)
	if r.outputs != nil {
		if out, ok := r.outputs[cmd]; ok {
			if r.failures != nil {
				if err, failed := r.failures[cmd]; failed {
					return out, err
				}
			}
			return out, nil
		}
	}
	if r.failures != nil {
		if err, ok := r.failures[cmd]; ok {
			return "", err
		}
	}
	return "", nil
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
