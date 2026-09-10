package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHTTPCompatibilityAndAuthentication(t *testing.T) {
	cfg := testConfig(t)
	app := testApp(t, cfg)
	server := httptest.NewServer(app.routes())
	defer server.Close()

	response, err := http.Get(server.URL + "/api/sessions")
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%d", response.StatusCode)
	}
	if response.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("authenticated API cache policy=%q", response.Header.Get("Cache-Control"))
	}
	response.Body.Close()

	response = requestJSON(t, server.URL, cfg.Auth.Password, http.MethodGet, "/", nil)
	body := readBody(t, response)
	if response.StatusCode != http.StatusOK ||
		!strings.Contains(body, "EasyConnect") ||
		!strings.Contains(body, `id="sidebarToggle"`) ||
		!strings.Contains(body, `id="fullscreenToggle"`) {
		t.Fatalf("embedded index status=%d body=%q", response.StatusCode, body)
	}
	response = requestJSON(t, server.URL, cfg.Auth.Password, http.MethodGet, "/app.js", nil)
	body = readBody(t, response)
	if response.StatusCode != http.StatusOK ||
		!strings.Contains(body, "ec_sidebar_collapsed") ||
		!strings.Contains(body, "requestFullscreen") ||
		strings.Contains(body, "ws?token=") {
		t.Fatalf("embedded app status=%d body=%q", response.StatusCode, body)
	}
	response = requestJSON(t, server.URL, cfg.Auth.Password, http.MethodGet, "/xterm.js", nil)
	if response.StatusCode != http.StatusOK || response.Header.Get("Cache-Control") != "no-cache" {
		t.Fatalf("embedded xterm status=%d cache=%q", response.StatusCode, response.Header.Get("Cache-Control"))
	}
	response.Body.Close()

	profileResponse := requestJSON(t, server.URL, cfg.Auth.Password, http.MethodPost, "/api/profiles", map[string]any{
		"name": "test-profile", "agent": "claude", "description": "test", "content": `{"hasCompletedOnboarding":true}`,
	})
	if profileResponse.StatusCode != http.StatusOK {
		t.Fatalf("create profile status=%d body=%s", profileResponse.StatusCode, readBody(t, profileResponse))
	}
	var profile struct {
		ID string `json:"id"`
	}
	decodeBody(t, profileResponse, &profile)
	profilePath := filepath.Join(cfg.ClaudeSettingsDir, profile.ID+".json")
	if mode := fileMode(t, profilePath); mode != 0o600 {
		t.Fatalf("profile mode=%o", mode)
	}

	sessionResponse := requestJSON(t, server.URL, cfg.Auth.Password, http.MethodPost, "/api/sessions", map[string]any{
		"name": "test-session", "agent": "claude", "profile_id": profile.ID, "working_dir": cfg.DefaultWorkingDir,
	})
	if sessionResponse.StatusCode != http.StatusOK {
		t.Fatalf("create session status=%d body=%s", sessionResponse.StatusCode, readBody(t, sessionResponse))
	}
	var session struct {
		ID string `json:"id"`
	}
	decodeBody(t, sessionResponse, &session)

	response = requestJSON(t, server.URL, cfg.Auth.Password, http.MethodGet, "/api/sessions", nil)
	var sessions []Session
	decodeBody(t, response, &sessions)
	if len(sessions) != 1 || sessions[0].ID != session.ID || sessions[0].ProfileName == nil || *sessions[0].ProfileName != "test-profile" {
		t.Fatalf("sessions=%#v", sessions)
	}
	response = requestJSON(t, server.URL, cfg.Auth.Password, http.MethodDelete, "/api/sessions/"+session.ID, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("delete session status=%d", response.StatusCode)
	}
	response.Body.Close()
	response = requestJSON(t, server.URL, cfg.Auth.Password, http.MethodDelete, "/api/profiles/"+profile.ID, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("delete profile status=%d", response.StatusCode)
	}
	response.Body.Close()
}

func TestPTYAndPollingLifecycle(t *testing.T) {
	cfg := testConfig(t)
	cliPath := filepath.Join(cfg.BaseDir, "fake-cli")
	script := "#!/bin/sh\nprintf 'READY\\n'\nwhile IFS= read -r line; do\n  printf 'ECHO:%s\\n' \"$line\"\ndone\n"
	if err := os.WriteFile(cliPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg.Claude.Binary = cliPath
	app := testApp(t, cfg)
	if err := os.MkdirAll(cfg.DefaultWorkingDir, 0o700); err != nil {
		t.Fatal(err)
	}
	session := Session{ID: "pty-session", Name: "pty", Agent: "claude", WorkingDir: cfg.DefaultWorkingDir}
	if err := app.store.createSession(session); err != nil {
		t.Fatal(err)
	}
	if err := app.sessions.Start(session); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "permission denied") {
			t.Skipf("PTY devices are unavailable in this test environment: %v", err)
		}
		t.Fatalf("start PTY: %v", err)
	}
	terminal := app.sessions.get(session.ID)
	waitForOutput(t, terminal, "READY")
	if _, err := terminal.Write([]byte("hello\r")); err != nil {
		t.Fatal(err)
	}
	waitForOutput(t, terminal, "ECHO:hello")
	start := time.Now()
	app.sessions.Stop(session.ID)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("graceful stop took %s", elapsed)
	}
	if app.sessions.IsRunning(session.ID) {
		t.Fatal("session still marked running")
	}
	stored, err := app.store.getSession(session.ID)
	if err != nil || stored.Status != "stopped" {
		t.Fatalf("stored session=%#v err=%v", stored, err)
	}
}

func TestCustomCodexProfileDoesNotWriteSecretToDiskOrArgs(t *testing.T) {
	cfg := testConfig(t)
	app := testApp(t, cfg)
	server := httptest.NewServer(app.routes())
	defer server.Close()
	secret := "custom-provider-secret"
	response := requestJSON(t, server.URL, cfg.Auth.Password, http.MethodPost, "/api/profiles", map[string]any{
		"name": "custom-codex", "agent": "codex",
		"content": "api_key = \"" + secret + "\"\nbase_url = \"https://provider.example/v1\"\nmodel = \"provider-model\"\nwire_api = \"responses\"\n",
	})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("create Codex profile status=%d body=%s", response.StatusCode, readBody(t, response))
	}
	var created struct {
		ID string `json:"id"`
	}
	decodeBody(t, response, &created)
	diskContent, err := os.ReadFile(filepath.Join(cfg.CodexHome, created.ID+".config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(diskContent), secret) || strings.Contains(string(diskContent), "api_key") || strings.Contains(string(diskContent), "base_url") {
		t.Fatalf("secret shorthand leaked to disk: %s", diskContent)
	}
	profile, err := app.store.getProfile(created.ID)
	if err != nil || !strings.Contains(profile.Content, secret) {
		t.Fatalf("protected DB profile missing source credentials: profile=%#v err=%v", profile, err)
	}
	profileID := created.ID
	runtime, err := app.sessionRuntime(Session{ID: "runtime", Agent: "codex", ProfileID: &profileID, WorkingDir: cfg.DefaultWorkingDir})
	if err != nil {
		t.Fatal(err)
	}
	if runtime.env["OPENAI_API_KEY"] != secret {
		t.Fatal("custom provider key not injected into session environment")
	}
	if strings.Contains(strings.Join(runtime.args, " "), secret) {
		t.Fatal("custom provider key leaked to command arguments")
	}
	if !strings.Contains(strings.Join(runtime.args, " "), "check_for_update_on_startup=false") {
		t.Fatal("Codex startup update prompt was not disabled")
	}
}

func TestShortAssistantOutputIsPersistedWhileRunning(t *testing.T) {
	cfg := testConfig(t)
	app := testApp(t, cfg)
	session := Session{ID: "short-output-session", Name: "short", Agent: "codex", WorkingDir: cfg.DefaultWorkingDir}
	if err := app.store.createSession(session); err != nil {
		t.Fatal(err)
	}
	messageID := "short-output-message"
	if err := app.store.createMessage(Message{ID: messageID, SessionID: session.ID, Role: "assistant"}); err != nil {
		t.Fatal(err)
	}
	terminal := &TerminalSession{
		manager: app.sessions, dbID: session.ID, agent: "codex", onboardingDone: true,
		output: byteRing{max: 1024}, poll: pollBuffer{maxBytes: 1024, maxItems: 10},
		messageOutput: byteRing{max: 1024}, clients: make(map[*wsClient]struct{}),
	}
	terminal.BeginMessage(messageID)
	terminal.consumeOutput([]byte("short final response"))
	t.Cleanup(func() { terminal.finalize(0) })

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		messages, err := app.store.listMessages(session.ID, 10, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(messages) == 1 && messages[0].Content == "short final response" {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("short assistant response was not persisted before session exit")
}

func TestCodexStartupScreenDetection(t *testing.T) {
	t.Parallel()
	if !codexNeedsStartupConfirmation("Welcome to Codex\n› 1. Yes, continue\n2. No, quit") {
		t.Fatal("Codex trust prompt was not recognized")
	}
	if !codexNeedsStartupConfirmation("Wel\x1b[1mcome\x1b[0m to Co\x1b[2mdex\n› 1. Y\x1b[1mes, con\x1b[0mtinue") {
		t.Fatal("ANSI-formatted Codex trust prompt was not recognized")
	}
	if codexNeedsStartupConfirmation("ordinary output mentioning trust") {
		t.Fatal("ordinary output was mistaken for the Codex trust prompt")
	}
	if !codexMainScreenReady("OpenAI Codex\nmodel: provider-model\ndirectory: /workspace") {
		t.Fatal("Codex main screen was not recognized")
	}
}

func waitForOutput(t *testing.T, terminal *TerminalSession, expected string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		output, _ := terminal.Poll(0)
		if strings.Contains(string(output), expected) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	output, _ := terminal.Poll(0)
	t.Fatalf("timed out waiting for %q in %q", expected, output)
}

func requestJSON(t *testing.T, baseURL, password, method, path string, body any) *http.Response {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, baseURL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+password)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func readBody(t *testing.T, response *http.Response) string {
	t.Helper()
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func decodeBody(t *testing.T, response *http.Response, destination any) {
	t.Helper()
	defer response.Body.Close()
	if err := json.NewDecoder(response.Body).Decode(destination); err != nil {
		t.Fatal(err)
	}
}
