package main

import (
	"io"
	"log"
	"path/filepath"
	"testing"
)

func testConfig(t *testing.T) *Config {
	t.Helper()
	root := t.TempDir()
	return &Config{
		Server: ServerConfig{Host: "127.0.0.1", Port: 26890},
		Auth:   AuthConfig{Password: "test-password"},
		Claude: AgentConfig{Binary: "/bin/true"},
		Codex:  AgentConfig{Binary: "/bin/true"},
		Session: SessionConfig{
			DefaultAgent: "claude", PollIntervalMS: 300, PollBufferSize: 50,
			OutputBufferBytes: 64 * 1024, PollBufferBytes: 32 * 1024,
		},
		Defaults:          DefaultsConfig{CodexProfileContent: defaultCodexProfile},
		Branding:          BrandingConfig{Name: "EasyConnect", Emoji: "🔗", DocumentTitle: "EasyConnect", WelcomeSubtitle: "Claude Code & Codex Web Management Panel"},
		BaseDir:           root,
		ConfigPath:        filepath.Join(root, "config.json"),
		DBPath:            filepath.Join(root, "easyconnect.db"),
		LogPath:           filepath.Join(root, "error.log"),
		DefaultWorkingDir: filepath.Join(root, "work"),
		ClaudeSettingsDir: filepath.Join(root, ".claude", "settings"),
		ClaudeProjectsDir: filepath.Join(root, ".claude", "projects"),
		CodexHome:         filepath.Join(root, ".codex"),
		CodexSessionsDir:  filepath.Join(root, ".codex", "sessions"),
		CodexUserHome:     root,
		Logging:           LoggingConfig{Path: filepath.Join(root, "error.log"), MaxSizeMB: 1},
	}
}

func testApp(t *testing.T, cfg *Config) *App {
	t.Helper()
	app, err := newApp(cfg, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("newApp: %v", err)
	}
	t.Cleanup(func() {
		app.sessions.stopAll()
		_ = app.store.Close()
	})
	return app
}
