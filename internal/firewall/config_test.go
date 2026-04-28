package firewall

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestLoadConfig_UsesDefaultsAndOverrides(t *testing.T) {
	scheme := newScheme(t)
	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "bulb-system",
				Name:      "bulb-firewall-agent",
			},
			Data: map[string]string{
				"backend":     "nftables",
				"zone":        "edge",
				"stateFile":   "/var/lib/bulb/custom.json",
				"deniedPorts": "22, 80, 443, 8443",
			},
		}).
		Build()

	cfg, err := LoadConfig(context.Background(), client, "bulb-system", "bulb-firewall-agent")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.Backend != "nftables" || cfg.Zone != "edge" || cfg.StateFile != "/var/lib/bulb/custom.json" {
		t.Fatalf("unexpected config: %+v", cfg)
	}
	if len(cfg.DeniedPorts) != 4 || cfg.DeniedPorts[3] != 8443 {
		t.Fatalf("unexpected denied ports: %+v", cfg.DeniedPorts)
	}
}

func TestParseDeniedPorts_Invalid(t *testing.T) {
	if _, err := parseDeniedPorts("22, nope"); err == nil {
		t.Fatal("expected parse error")
	}
}
