package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"
)

const (
	defaultCodexProfile = "# Custom provider; credentials are injected only into this session\napi_key = \"\"\nbase_url = \"\"\nmodel = \"\"\nwire_api = \"responses\"\n"
	examplePassword     = "change-this-password"
	defaultServerHost   = "127.0.0.1"
)

type Config struct {
	Server      ServerConfig      `json:"server"`
	Auth        AuthConfig        `json:"auth"`
	Database    DatabaseConfig    `json:"database"`
	Claude      AgentConfig       `json:"claude"`
	Codex       AgentConfig       `json:"codex"`
	Session     SessionConfig     `json:"session"`
	Defaults    DefaultsConfig    `json:"defaults"`
	Branding    BrandingConfig    `json:"branding"`
	FileBrowser FileBrowserConfig `json:"fileBrowser"`
	Logging     LoggingConfig     `json:"logging"`

	BaseDir           string `json:"-"`
	ConfigPath        string `json:"-"`
	DBPath            string `json:"-"`
	LogPath           string `json:"-"`
	DefaultWorkingDir string `json:"-"`
	ClaudeSettingsDir string `json:"-"`
	ClaudeProjectsDir string `json:"-"`
	CodexHome         string `json:"-"`
	CodexSessionsDir  string `json:"-"`
	ClaudeUserHome    string `json:"-"`
	CodexUserHome     string `json:"-"`
}

type ServerConfig struct {
	Port int    `json:"port"`
	Host string `json:"host"`
}

type AuthConfig struct {
	Password string `json:"password"`
}

type DatabaseConfig struct {
	Path string `json:"path"`
}

type AgentConfig struct {
	Binary            string `json:"binary"`
	SettingsDir       string `json:"settingsDir"`
	ProjectsDir       string `json:"projectsDir"`
	DefaultWorkingDir string `json:"defaultWorkingDir"`
	Home              string `json:"home"`
	SessionsDir       string `json:"sessionsDir"`
	RunAsUser         string `json:"runAsUser"`
}

type SessionConfig struct {
	DefaultAgent      string `json:"defaultAgent"`
	DefaultWorkingDir string `json:"defaultWorkingDir"`
	PollIntervalMS    int    `json:"pollIntervalMs"`
	PollBufferSize    int    `json:"pollBufferSize"`
	OutputBufferBytes int    `json:"outputBufferBytes"`
	PollBufferBytes   int    `json:"pollBufferBytes"`
}

type DefaultsConfig struct {
	ProfileContent      any    `json:"profileContent"`
	CodexProfileContent string `json:"codexProfileContent"`
}

type BrandingConfig struct {
	Name            string `json:"name"`
	Emoji           string `json:"emoji"`
	DocumentTitle   string `json:"documentTitle"`
	WelcomeSubtitle string `json:"welcomeSubtitle"`
}

type FileBrowserConfig struct {
	URL string `json:"url"`
}

type LoggingConfig struct {
	Path      string `json:"path"`
	MaxSizeMB int64  `json:"maxSizeMB"`
}

func loadConfig(configPath string) (*Config, error) {
	absPath, err := filepath.Abs(configPath)
	if err != nil {
		return nil, fmt.Errorf("resolve config path: %w", err)
	}
	raw, err := os.ReadFile(absPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("config file not found: %s", absPath)
		}
		return nil, fmt.Errorf("read config: %w", err)
	}
	if err := os.Chmod(absPath, 0o600); err != nil {
		return nil, fmt.Errorf("secure config permissions: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	cfg.ConfigPath = absPath
	cfg.BaseDir = filepath.Dir(absPath)
	if cfg.Server.Port == 0 {
		cfg.Server.Port = 26890
	}
	if cfg.Server.Port < 1 || cfg.Server.Port > 65535 {
		return nil, fmt.Errorf("server.port must be between 1 and 65535")
	}
	if cfg.Server.Host == "" {
		cfg.Server.Host = defaultServerHost
	}
	if strings.TrimSpace(cfg.Auth.Password) == "" {
		return nil, errors.New("auth.password must not be empty")
	}
	if cfg.Auth.Password == examplePassword {
		return nil, errors.New("auth.password must be changed from the example value")
	}
	if cfg.Claude.Binary == "" {
		cfg.Claude.Binary = "claude"
	}
	if cfg.Codex.Binary == "" {
		cfg.Codex.Binary = "codex"
	}
	if cfg.Session.DefaultAgent != "codex" {
		cfg.Session.DefaultAgent = "claude"
	}
	if cfg.Session.PollIntervalMS <= 0 {
		cfg.Session.PollIntervalMS = 300
	}
	if cfg.Session.PollBufferSize <= 0 {
		cfg.Session.PollBufferSize = 500
	}
	if cfg.Session.OutputBufferBytes <= 0 {
		cfg.Session.OutputBufferBytes = 2 * 1024 * 1024
	}
	if cfg.Session.PollBufferBytes <= 0 {
		cfg.Session.PollBufferBytes = 1024 * 1024
	}
	if cfg.Defaults.CodexProfileContent == "" {
		cfg.Defaults.CodexProfileContent = defaultCodexProfile
	}
	applyBrandingDefaults(&cfg.Branding)

	home, err := serviceUserHome()
	if err != nil {
		return nil, fmt.Errorf("resolve service home: %w", err)
	}
	cfg.ClaudeUserHome = homeForRunAsUser(cfg.Claude.RunAsUser, home)
	cfg.CodexUserHome = homeForRunAsUser(cfg.Codex.RunAsUser, home)
	cfg.ClaudeSettingsDir = resolveOptionalPath(cfg.Claude.SettingsDir, filepath.Join(cfg.ClaudeUserHome, ".claude", "settings"), cfg.BaseDir)
	cfg.ClaudeProjectsDir = resolveOptionalPath(cfg.Claude.ProjectsDir, filepath.Join(cfg.ClaudeUserHome, ".claude", "projects"), cfg.BaseDir)
	cfg.CodexHome = resolveOptionalPath(cfg.Codex.Home, filepath.Join(cfg.CodexUserHome, ".codex"), cfg.BaseDir)
	cfg.CodexSessionsDir = resolveOptionalPath(cfg.Codex.SessionsDir, filepath.Join(cfg.CodexHome, "sessions"), cfg.BaseDir)
	defaultAgentHome := cfg.ClaudeUserHome
	if cfg.Session.DefaultAgent == "codex" {
		defaultAgentHome = cfg.CodexUserHome
	}
	workingDir := firstNonEmpty(cfg.Session.DefaultWorkingDir, cfg.Claude.DefaultWorkingDir, defaultAgentHome)
	cfg.DefaultWorkingDir = resolveOptionalPath(workingDir, defaultAgentHome, cfg.BaseDir)

	dbPath := cfg.Database.Path
	if dbPath == "" {
		legacy := filepath.Join(cfg.BaseDir, "easyclaude.db")
		if _, err := os.Stat(legacy); err == nil {
			dbPath = legacy
		} else {
			dbPath = filepath.Join(cfg.BaseDir, "easyconnect.db")
		}
	}
	cfg.DBPath = resolveOptionalPath(dbPath, filepath.Join(cfg.BaseDir, "easyconnect.db"), cfg.BaseDir)
	if cfg.Logging.Path == "" {
		cfg.Logging.Path = "error.log"
	}
	if cfg.Logging.MaxSizeMB <= 0 {
		cfg.Logging.MaxSizeMB = 10
	}
	cfg.LogPath = resolveOptionalPath(cfg.Logging.Path, filepath.Join(cfg.BaseDir, "error.log"), cfg.BaseDir)
	return &cfg, nil
}

func serviceUserHome() (string, error) {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return home, nil
	}
	account, err := user.Current()
	if err != nil {
		return "", err
	}
	if account.HomeDir == "" {
		return "", errors.New("current user has no home directory")
	}
	return account.HomeDir, nil
}

func applyBrandingDefaults(brand *BrandingConfig) {
	if brand.Name == "" || brand.Name == "EasyClaude" {
		brand.Name = "EasyConnect"
	}
	if brand.Emoji == "" || brand.Emoji == "🤖" {
		brand.Emoji = "🔗"
	}
	if brand.DocumentTitle == "" || brand.DocumentTitle == "EasyClaude" {
		brand.DocumentTitle = "EasyConnect"
	}
	if brand.WelcomeSubtitle == "" || brand.WelcomeSubtitle == "Claude Code Web Management Panel" {
		brand.WelcomeSubtitle = "Claude Code & Codex Web Management Panel"
	}
}

func resolveOptionalPath(value, fallback, base string) string {
	if strings.TrimSpace(value) == "" {
		value = fallback
	}
	if !filepath.IsAbs(value) {
		value = filepath.Join(base, value)
	}
	abs, err := filepath.Abs(value)
	if err != nil {
		return filepath.Clean(value)
	}
	return abs
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func homeForRunAsUser(username, fallback string) string {
	if username == "" {
		return fallback
	}
	if account, err := user.Lookup(username); err == nil && account.HomeDir != "" {
		return account.HomeDir
	}
	return filepath.Join("/home", username)
}
