package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		valid      bool
	}{
		{"valid", "cache_size = '1.5GB'\ncache = ['chip-google-toolchain', 'git:https://github.com/a/b.git']", true},
		{"unknown key", "cache_size = '1GB'\nworkers = 2", false},
		{"internal field", "cache_size = '1GB'\nLimit = 9", false},
		{"binary units", "cache_size = '1GiB'", false},
		{"negative", "cache_size = '-1GB'", false},
		{"overflow", "cache_size = '999999999999999TB'", false},
		{"private", "cache_size = '1GB'\ncache = ['git:https://u:p@github.com/a/b']", false},
		{"ssh", "cache_size = '1GB'\ncache = ['git:ssh://github.com/a/b']", false},
		{"unknown entry", "cache_size = '1GB'\ncache = ['pip']", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte(tc.text), 0o600); err != nil {
				t.Fatal(err)
			}
			c, err := Load(path)
			if (err == nil) != tc.valid {
				t.Fatalf("Load: %v", err)
			}
			if tc.valid && (c.Limit != 1500000000 || !c.Toolchain || len(c.Repositories) != 1) {
				t.Fatalf("config: %+v", c)
			}
		})
	}
}

func TestPaths(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(base, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(base, "state"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(base, "cache"))
	t.Setenv("STATE_DIRECTORY", filepath.Join(base, "systemd-state"))
	t.Setenv("CACHE_DIRECTORY", filepath.Join(base, "systemd-cache"))
	p, err := ResolvePaths()
	if err != nil {
		t.Fatal(err)
	}
	if p.State != filepath.Join(base, "systemd-state") || p.Cache != filepath.Join(base, "systemd-cache") {
		t.Fatalf("paths: %+v", p)
	}
	t.Setenv("CACHE_DIRECTORY", p.State)
	if _, err := ResolvePaths(); err == nil {
		t.Fatal("accepted overlapping state and cache")
	}
}

func TestRepositoryURL(t *testing.T) {
	for _, value := range []string{"https://github.com/a/../b", "https://github.com/a/%2e%2e/b", "https://127.0.0.1/a/b", "https://localhost/a/b", "https://github.com/a/b?token=x", "https://github.com/a/b\n", "https://github.com:8443/a/b"} {
		if _, err := RepositoryURL(value); err == nil {
			t.Errorf("accepted %q", value)
		}
	}
}
