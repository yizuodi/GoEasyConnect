package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/pelletier/go-toml/v2"
)

var tomlAssignmentPattern = regexp.MustCompile(`^\s*([A-Za-z0-9_.-]+)\s*=\s*("(?:\\.|[^"\\])*"|'[^']*')\s*(?:#.*)?$`)

type codexRuntime struct {
	APIKey         string
	BaseURL        string
	Model          string
	WireAPI        string
	Custom         bool
	ProfileContent string
}

func parseCodexRuntime(content string) (codexRuntime, error) {
	if strings.IndexByte(content, 0) >= 0 {
		return codexRuntime{}, errors.New("Codex TOML cannot contain null bytes")
	}
	var parsed map[string]any
	if err := toml.Unmarshal([]byte(content), &parsed); err != nil {
		return codexRuntime{}, err
	}
	apiKey, err := readCodexShorthand(parsed, content, "api_key")
	if err != nil {
		return codexRuntime{}, err
	}
	baseURL, err := readCodexShorthand(parsed, content, "base_url")
	if err != nil {
		return codexRuntime{}, err
	}
	model, err := topLevelTOMLString(parsed, content, "model")
	if err != nil {
		return codexRuntime{}, err
	}
	wireAPI, err := readCodexShorthand(parsed, content, "wire_api")
	if err != nil {
		return codexRuntime{}, err
	}
	runtime := codexRuntime{
		APIKey:  apiKey,
		BaseURL: baseURL,
		Model:   model,
		WireAPI: wireAPI,
	}
	if runtime.WireAPI == "" {
		runtime.WireAPI = "responses"
	}
	runtime.Custom = runtime.APIKey != "" || runtime.BaseURL != ""
	if runtime.Custom && (runtime.APIKey == "" || runtime.BaseURL == "" || runtime.Model == "") {
		return codexRuntime{}, errors.New("Custom Codex profiles require api_key, base_url, and model")
	}
	if runtime.Custom {
		parsedURL, err := url.ParseRequestURI(runtime.BaseURL)
		if err != nil || parsedURL.Host == "" || (parsedURL.Scheme != "http" && parsedURL.Scheme != "https") {
			return codexRuntime{}, errors.New("base_url must be an http or https URL")
		}
		if runtime.WireAPI != "responses" && runtime.WireAPI != "chat" {
			return codexRuntime{}, errors.New("wire_api must be responses or chat")
		}
	}
	runtime.ProfileContent = removeTopLevelCodexShorthand(content)
	return runtime, nil
}

func topLevelTOMLString(parsed map[string]any, content, key string) (string, error) {
	value, exists := parsed[key]
	if !exists {
		return "", nil
	}
	text, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("%s must be a string", key)
	}
	// EasyConnect removes credential shorthand before writing the Codex
	// profile. Keep that transformation deliberately narrow and predictable.
	if key != "model" && !hasTopLevelSimpleAssignment(content, key) {
		return "", fmt.Errorf("%s must use a single-line quoted value", key)
	}
	return text, nil
}

func readCodexShorthand(parsed map[string]any, content, key string) (string, error) {
	if _, exists := parsed[key]; exists {
		return topLevelTOMLString(parsed, content, key)
	}
	table := ""
	for _, line := range strings.Split(content, "\n") {
		if name, ok := tomlTableName(line); ok {
			table = name
			continue
		}
		match := tomlAssignmentPattern.FindStringSubmatch(line)
		if len(match) != 3 || match[1] != key || !isLegacyCodexShorthandTable(table) {
			continue
		}
		return unquoteTOMLString(match[2])
	}
	return "", nil
}

func unquoteTOMLString(quoted string) (string, error) {
	if strings.HasPrefix(quoted, "'") {
		return strings.TrimSuffix(strings.TrimPrefix(quoted, "'"), "'"), nil
	}
	value, err := strconv.Unquote(quoted)
	if err != nil {
		return "", err
	}
	return value, nil
}

func tomlTableName(line string) (string, bool) {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "[") {
		return "", false
	}
	trimmed = strings.Trim(trimmed, "[] ")
	if trimmed == "" {
		return "", false
	}
	return trimmed, true
}

func isLegacyCodexShorthandTable(table string) bool {
	return table == "" || table == "chat_model" || table == "provider" || strings.HasPrefix(table, "provider.")
}

func hasTopLevelSimpleAssignment(content, key string) bool {
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") {
			return false
		}
		match := tomlAssignmentPattern.FindStringSubmatch(line)
		if len(match) == 3 && match[1] == key {
			return true
		}
	}
	return false
}

func removeTopLevelCodexShorthand(content string) string {
	lines := strings.Split(content, "\n")
	filtered := make([]string, 0, len(lines))
	table := ""
	for _, line := range lines {
		if name, ok := tomlTableName(line); ok {
			table = name
		}
		match := tomlAssignmentPattern.FindStringSubmatch(line)
		if isLegacyCodexShorthandTable(table) && len(match) == 3 && (match[1] == "api_key" || match[1] == "base_url" || match[1] == "wire_api") {
			continue
		}
		filtered = append(filtered, line)
	}
	return strings.Join(filtered, "\n")
}

func normalizeProfileContent(agent string, content any) (string, error) {
	if agent == "claude" {
		var raw []byte
		var err error
		if text, ok := content.(string); ok {
			raw = []byte(text)
		} else {
			raw, err = json.Marshal(content)
			if err != nil {
				return "", err
			}
		}
		var value any
		if err := json.Unmarshal(raw, &value); err != nil {
			return "", err
		}
		return string(raw), nil
	}
	text, ok := content.(string)
	if !ok {
		if content == nil {
			text = ""
		} else {
			text = fmt.Sprint(content)
		}
	}
	if _, err := parseCodexRuntime(text); err != nil {
		return "", err
	}
	return text, nil
}

func customProviderArgs(runtime codexRuntime) []string {
	if !runtime.Custom {
		return nil
	}
	return []string{
		"-c", `model=` + tomlQuote(runtime.Model),
		"-c", `model_provider="easyconnect"`,
		"-c", `model_providers.easyconnect.name="EasyConnect custom provider"`,
		"-c", `model_providers.easyconnect.base_url=` + tomlQuote(runtime.BaseURL),
		"-c", `model_providers.easyconnect.env_key="OPENAI_API_KEY"`,
		"-c", `model_providers.easyconnect.wire_api=` + tomlQuote(runtime.WireAPI),
		"-c", `shell_environment_policy.exclude=["OPENAI_API_KEY"]`,
		"--disable", "shell_snapshot",
	}
}

func tomlQuote(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func (a *App) profileFilePath(profile Profile) string {
	if profile.Agent == "codex" {
		return filepath.Join(a.cfg.CodexHome, profile.ID+".config.toml")
	}
	return filepath.Join(a.cfg.ClaudeSettingsDir, profile.ID+".json")
}

func (a *App) writeProfileFile(profile Profile, content string) error {
	fileContent := content
	runAsUser := a.cfg.Claude.RunAsUser
	if profile.Agent == "codex" {
		runtime, err := parseCodexRuntime(content)
		if err != nil {
			return err
		}
		fileContent = runtime.ProfileContent
		runAsUser = a.cfg.Codex.RunAsUser
	}
	return writeAgentFile(a.profileFilePath(profile), []byte(fileContent), runAsUser)
}

func writeAgentFile(path string, content []byte, runAsUser string) error {
	if err := ensureDirectory(filepath.Dir(path), 0o700, runAsUser); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".easyconnect-profile-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(content); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := chownPath(temporaryPath, runAsUser); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

func ensureAgentDirectory(path, runAsUser string) error {
	return ensureDirectory(path, 0o700, runAsUser)
}

func ensureDirectory(path string, mode os.FileMode, runAsUser string) error {
	cleanPath := filepath.Clean(path)
	missing := make([]string, 0)
	for current := cleanPath; ; current = filepath.Dir(current) {
		_, err := os.Stat(current)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		missing = append(missing, current)
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
	}
	if err := os.MkdirAll(cleanPath, mode); err != nil {
		return err
	}
	for index := len(missing) - 1; index >= 0; index-- {
		if err := chownPath(missing[index], runAsUser); err != nil {
			return err
		}
	}
	return nil
}

func chownPath(path, username string) error {
	if username == "" {
		return nil
	}
	account, err := user.Lookup(username)
	if err != nil {
		return fmt.Errorf("lookup user %s: %w", username, err)
	}
	uid, err := strconv.Atoi(account.Uid)
	if err != nil {
		return err
	}
	gid, err := strconv.Atoi(account.Gid)
	if err != nil {
		return err
	}
	return os.Chown(path, uid, gid)
}

func (a *App) validateCodexProfile(content string) error {
	runtime, err := parseCodexRuntime(content)
	if err != nil {
		return err
	}
	validationID := "easyconnect-validation-" + uuid.NewString()
	validationPath := filepath.Join(a.cfg.CodexHome, validationID+".config.toml")
	if err := writeAgentFile(validationPath, []byte(runtime.ProfileContent), a.cfg.Codex.RunAsUser); err != nil {
		return err
	}
	defer os.Remove(validationPath)
	args := append([]string{"--profile", validationID}, customProviderArgs(runtime)...)
	args = append(args, "mcp", "list")
	binary := a.cfg.Codex.Binary
	if a.cfg.Codex.RunAsUser != "" {
		args = append([]string{"-u", a.cfg.Codex.RunAsUser, "-E", binary}, args...)
		binary = "sudo"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, binary, args...)
	overrides := map[string]string{"CODEX_HOME": a.cfg.CodexHome, "HOME": a.cfg.CodexUserHome}
	if runtime.APIKey != "" {
		overrides["OPENAI_API_KEY"] = runtime.APIKey
	}
	command.Env = mergedEnv(overrides)
	output, commandErr := command.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return errors.New("Codex configuration validation timed out")
	}
	if commandErr != nil {
		return errors.New(safeCodexError(output, runtime.APIKey))
	}
	return nil
}

func safeCodexError(output []byte, secrets ...string) string {
	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Error:") {
			for _, secret := range secrets {
				if secret != "" {
					line = strings.ReplaceAll(line, secret, "[REDACTED]")
				}
			}
			return line
		}
	}
	return "Codex rejected this configuration"
}

func mergedEnv(overrides map[string]string) []string {
	values := make(map[string]string)
	order := make([]string, 0)
	for _, entry := range os.Environ() {
		key, _, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if _, exists := values[key]; !exists {
			order = append(order, key)
		}
		values[key] = entry
	}
	for key, value := range overrides {
		entry := key + "=" + value
		if _, exists := values[key]; !exists {
			order = append(order, key)
		}
		values[key] = entry
	}
	result := make([]string, 0, len(values))
	for _, key := range order {
		result = append(result, values[key])
	}
	return result
}
