package firewall

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type StateStore interface {
	Load() ([]PortKey, error)
	Save([]PortKey) error
}

type FileStateStore struct {
	Path string
}

type PortKey struct {
	Port     int32  `json:"port"`
	Protocol string `json:"protocol"`
}

func portKeys(ports []PortSpec) []PortKey {
	out := make([]PortKey, 0, len(ports))
	for _, port := range ports {
		out = append(out, port.key())
	}
	return out
}

func specsFromKeys(keys []PortKey) []PortSpec {
	out := make([]PortSpec, 0, len(keys))
	for _, key := range keys {
		out = append(out, PortSpec{
			Port:     key.Port,
			Protocol: protocolFromString(key.Protocol),
		})
	}
	return out
}

func (s FileStateStore) Load() ([]PortKey, error) {
	data, err := os.ReadFile(s.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read state file: %w", err)
	}
	var keys []PortKey
	if err := json.Unmarshal(data, &keys); err != nil {
		return nil, fmt.Errorf("decode state file: %w", err)
	}
	return keys, nil
}

func (s FileStateStore) Save(keys []PortKey) error {
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o755); err != nil {
		return fmt.Errorf("mkdir state dir: %w", err)
	}
	data, err := json.Marshal(keys)
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	tmp := s.Path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write temp state file: %w", err)
	}
	if err := os.Rename(tmp, s.Path); err != nil {
		return fmt.Errorf("replace state file: %w", err)
	}
	return nil
}
