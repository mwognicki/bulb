package firewall

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	defaultConfigNamespace = "bulb-system"
	defaultConfigMapName   = "bulb-firewall-agent"
	defaultZone            = "public"
	defaultStateFile       = "/var/lib/bulb/firewall-agent-state.json"
)

type AgentConfig struct {
	Backend     string
	Zone        string
	StateFile   string
	DeniedPorts []int32
	DryRun      bool
}

func DefaultConfig() AgentConfig {
	return AgentConfig{
		Backend:     "firewalld",
		Zone:        defaultZone,
		StateFile:   defaultStateFile,
		DeniedPorts: []int32{22, 80, 443},
		DryRun:      false,
	}
}

func LoadConfig(ctx context.Context, c client.Reader, namespace, name string) (AgentConfig, error) {
	cfg := DefaultConfig()
	if namespace == "" {
		namespace = defaultConfigNamespace
	}
	if name == "" {
		name = defaultConfigMapName
	}

	var cm corev1.ConfigMap
	if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &cm); err != nil {
		return cfg, fmt.Errorf("get firewall-agent configmap %s/%s: %w", namespace, name, err)
	}

	if v := strings.TrimSpace(cm.Data["backend"]); v != "" {
		cfg.Backend = v
	}
	if v := strings.TrimSpace(cm.Data["zone"]); v != "" {
		cfg.Zone = v
	}
	if v := strings.TrimSpace(cm.Data["stateFile"]); v != "" {
		cfg.StateFile = v
	}
	if v := strings.TrimSpace(cm.Data["deniedPorts"]); v != "" {
		ports, err := parseDeniedPorts(v)
		if err != nil {
			return cfg, fmt.Errorf("parse deniedPorts: %w", err)
		}
		cfg.DeniedPorts = ports
	}
	if v := strings.TrimSpace(cm.Data["dryRun"]); v != "" {
		dryRun, err := strconv.ParseBool(v)
		if err != nil {
			return cfg, fmt.Errorf("parse dryRun: %w", err)
		}
		cfg.DryRun = dryRun
	}
	return cfg, nil
}

func parseDeniedPorts(raw string) ([]int32, error) {
	parts := strings.Split(raw, ",")
	ports := make([]int32, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		n, err := strconv.ParseInt(part, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("invalid port %q", part)
		}
		if n < 1 || n > 65535 {
			return nil, fmt.Errorf("port %d out of range", n)
		}
		ports = append(ports, int32(n))
	}
	return ports, nil
}
