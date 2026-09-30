package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestLoadConfigReusesLegacyDatabase(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	configPath := filepath.Join(root, "config.json")
	if err := os.WriteFile(configPath, []byte(`{"server":{"port":12345},"auth":{"password":"test-password"},"branding":{"name":"EasyClaude"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	legacyDB := filepath.Join(root, "easyclaude.db")
	if err := os.WriteFile(legacyDB, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := loadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if config.DBPath != legacyDB || config.Branding.Name != "EasyConnect" || config.Server.Host != defaultServerHost {
		t.Fatalf("unexpected config: %#v", config)
	}
	if mode := fileMode(t, configPath); mode != 0o600 {
		t.Fatalf("config mode=%o", mode)
	}
}

func TestLoadConfigRejectsUnsafePassword(t *testing.T) {
	t.Parallel()
	for _, password := range []string{"", "   ", examplePassword} {
		password := password
		t.Run(password, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			configPath := filepath.Join(root, "config.json")
			content := []byte(`{"auth":{"password":` + strconv.Quote(password) + `}}`)
			if err := os.WriteFile(configPath, content, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := loadConfig(configPath); err == nil || !strings.Contains(err.Error(), "auth.password") {
				t.Fatalf("loadConfig password=%q error=%v", password, err)
			}
		})
	}
}

func TestLoadConfigUpdateDefaults(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		updates string
		enabled bool
	}{
		{name: "missing", updates: "", enabled: true},
		{name: "enabled", updates: `,"updates":{"enabled":true}`, enabled: true},
		{name: "disabled", updates: `,"updates":{"enabled":false}`, enabled: false},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			configPath := filepath.Join(root, "config.json")
			content := []byte(`{"auth":{"password":"test-password"}` + test.updates + `}`)
			if err := os.WriteFile(configPath, content, 0o600); err != nil {
				t.Fatal(err)
			}
			config, err := loadConfig(configPath)
			if err != nil {
				t.Fatal(err)
			}
			if config.Updates.Enabled != test.enabled {
				t.Fatalf("updates enabled=%v, want %v", config.Updates.Enabled, test.enabled)
			}
		})
	}
}

func TestServiceUserHomeWithoutHOME(t *testing.T) {
	t.Setenv("HOME", "")
	home, err := serviceUserHome()
	if err != nil {
		t.Fatal(err)
	}
	if home == "" || !filepath.IsAbs(home) {
		t.Fatalf("service home=%q", home)
	}
}
