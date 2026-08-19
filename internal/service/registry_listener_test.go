package service

import (
	"errors"
	"testing"
)

func TestRegistryListenerSettingsValidate(t *testing.T) {
	rs := &RegistrySettings{Namespaces: []RegistryNamespace{{
		Name:         "default",
		Repositories: []RegistryRepository{{Name: "docker", Type: RegistryTypeDocker, Kind: RegistryKindLocal}},
	}}}

	tests := []struct {
		name string
		cfg  RegistryListenerSettings
		ok   bool
	}{
		{
			name: "shared plain port with different hostnames",
			cfg: RegistryListenerSettings{Listeners: []RegistryListener{
				{ID: "one", Enabled: true, Port: 5000, Hostname: "docker.example.com", Namespace: "default", Repo: "docker"},
				{ID: "two", Enabled: true, Port: 5000, Hostname: "npm.example.com", Namespace: "default", Repo: "docker"},
			}},
			ok: true,
		},
		{
			name: "reverse proxy catch-all without TLS",
			cfg: RegistryListenerSettings{Listeners: []RegistryListener{
				{ID: "one", Enabled: true, Port: 5000, Namespace: "default", Repo: "docker"},
			}},
			ok: true,
		},
		{
			name: "duplicate hostname",
			cfg: RegistryListenerSettings{Listeners: []RegistryListener{
				{ID: "one", Enabled: true, Port: 5000, Hostname: "docker.example.com", Namespace: "default", Repo: "docker"},
				{ID: "two", Enabled: true, Port: 5000, Hostname: "docker.example.com", Namespace: "default", Repo: "docker"},
			}},
		},
		{
			name: "unknown repo",
			cfg: RegistryListenerSettings{Listeners: []RegistryListener{
				{ID: "one", Enabled: true, Port: 5000, Namespace: "default", Repo: "missing"},
			}},
		},
		{
			name: "invalid hostname",
			cfg: RegistryListenerSettings{Listeners: []RegistryListener{
				{ID: "one", Enabled: true, Port: 5000, Hostname: "Docker Example", Namespace: "default", Repo: "docker"},
			}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate(rs)
			if tt.ok && err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if !tt.ok && !errors.Is(err, ErrBadRequest) {
				t.Fatalf("Validate error = %v, want ErrBadRequest", err)
			}
		})
	}
}
