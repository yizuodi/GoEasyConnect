package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
)

var (
	codexSessionSuffix = regexp.MustCompile(`([0-9a-fA-F]{8}-[0-9a-fA-F-]{27,})$`)
	terminalCSI        = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)
)

type agentRuntime struct {
	agent      string
	binary     string
	args       []string
	runAsUser  string
	env        map[string]string
	sessionDir string
	sessionID  string
	workDir    string
}

type SessionManager struct {
	app     *App
	mu      sync.RWMutex
	running map[string]*TerminalSession
}

type TerminalSession struct {
	manager *SessionManager
	dbID    string
	pty     *os.File
	cmd     *exec.Cmd

	mu                 sync.Mutex
	agent              string
	agentSessionID     string
	sessionDir         string
	workDir            string
	existingFiles      map[string]struct{}
	lastDetection      time.Time
	output             byteRing
	poll               pollBuffer
	messageOutput      byteRing
	currentAssistantID string
	lastMessageSave    time.Time
	messageSaveTimer   *time.Timer
	finalized          bool
	onboardingDone     bool
	onboarding         byteRing
	trustSent          bool
	themeSent          bool
	codexStartupSent   bool
	clients            map[*wsClient]struct{}
	stopping           bool
	exited             chan struct{}
	readDone           chan struct{}
	stopDone           chan struct{}
	stopOnce           sync.Once
	closeOnce          onceError
}

func newSessionManager(app *App) *SessionManager {
	return &SessionManager{app: app, running: make(map[string]*TerminalSession)}
}

func (m *SessionManager) IsRunning(id string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.running[id]
	return ok
}

func (m *SessionManager) get(id string) *TerminalSession {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.running[id]
}

func (m *SessionManager) Start(session Session) error {
	runtime, err := m.app.sessionRuntime(session)
	if err != nil {
		return err
	}
	existingFiles := listSessionFiles(runtime.sessionDir)

	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.running[session.ID]; exists {
		return errors.New("Session already running")
	}
	spawnBinary := runtime.binary
	spawnArgs := append([]string(nil), runtime.args...)
	if runtime.runAsUser != "" {
		spawnArgs = append([]string{"-u", runtime.runAsUser, "-E", runtime.binary}, spawnArgs...)
		spawnBinary = "sudo"
	}
	command := exec.Command(spawnBinary, spawnArgs...)
	command.Dir = runtime.workDir
	overrides := make(map[string]string, len(runtime.env)+2)
	for key, value := range runtime.env {
		overrides[key] = value
	}
	overrides["TERM"] = "xterm-256color"
	if runtime.runAsUser != "" {
		overrides["HOME"] = homeForRunAsUser(runtime.runAsUser, "")
	}
	command.Env = mergedEnv(overrides)
	ptmx, err := pty.StartWithSize(command, &pty.Winsize{Cols: 120, Rows: 40})
	if err != nil {
		return fmt.Errorf("start PTY: %w", err)
	}
	terminal := &TerminalSession{
		manager:        m,
		dbID:           session.ID,
		pty:            ptmx,
		cmd:            command,
		agent:          runtime.agent,
		agentSessionID: runtime.sessionID,
		sessionDir:     runtime.sessionDir,
		workDir:        runtime.workDir,
		existingFiles:  existingFiles,
		output:         byteRing{max: m.app.cfg.Session.OutputBufferBytes},
		poll: pollBuffer{
			maxBytes: m.app.cfg.Session.PollBufferBytes,
			maxItems: m.app.cfg.Session.PollBufferSize,
		},
		messageOutput:  byteRing{max: m.app.cfg.Session.OutputBufferBytes},
		onboardingDone: runtime.sessionID != "",
		onboarding:     byteRing{max: 64 * 1024},
		clients:        make(map[*wsClient]struct{}),
		exited:         make(chan struct{}),
		readDone:       make(chan struct{}),
		stopDone:       make(chan struct{}),
	}
	m.running[session.ID] = terminal
	if err := m.app.store.setSessionStatus(session.ID, "running"); err != nil {
		delete(m.running, session.ID)
		_ = ptmx.Close()
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		_ = command.Process.Kill()
		_ = command.Wait()
		return err
	}
	go terminal.readLoop()
	go terminal.waitLoop()
	return nil
}

func (m *SessionManager) Stop(id string) {
	terminal := m.get(id)
	if terminal == nil {
		return
	}
	terminal.stop()
}

func (m *SessionManager) stopAll() {
	m.mu.RLock()
	items := make([]*TerminalSession, 0, len(m.running))
	for _, terminal := range m.running {
		items = append(items, terminal)
	}
	m.mu.RUnlock()
	var wait sync.WaitGroup
	wait.Add(len(items))
	for _, terminal := range items {
		go func() {
			defer wait.Done()
			terminal.stop()
		}()
	}
	wait.Wait()
}

func (m *SessionManager) finish(id string, terminal *TerminalSession) {
	m.mu.Lock()
	if m.running[id] == terminal {
		delete(m.running, id)
	}
	m.mu.Unlock()
}

func (a *App) sessionRuntime(session Session) (agentRuntime, error) {
	agent := normalizeAgent(session.Agent)
	workDir, err := filepath.Abs(firstNonEmpty(session.WorkingDir, a.cfg.DefaultWorkingDir))
	if err != nil {
		return agentRuntime{}, err
	}
	if agent == "codex" {
		args := []string{"--no-alt-screen", "-c", "check_for_update_on_startup=false"}
		runtime := codexRuntime{}
		if session.ProfileID != nil && *session.ProfileID != "" {
			profile, err := a.store.getAgentProfile(*session.ProfileID, "codex")
			if err != nil {
				return agentRuntime{}, errors.New("Selected Codex profile was not found")
			}
			runtime, err = parseCodexRuntime(profile.Content)
			if err != nil {
				return agentRuntime{}, err
			}
			args = append(args, "--profile", profile.ID)
			args = append(args, customProviderArgs(runtime)...)
		}
		if session.SkipPermissions != 0 {
			args = append(args, "--dangerously-bypass-approvals-and-sandbox")
		}
		if session.CodexSessionID != "" {
			args = append(args, "resume", session.CodexSessionID)
		}
		env := map[string]string{"CODEX_HOME": a.cfg.CodexHome}
		if runtime.APIKey != "" {
			env["OPENAI_API_KEY"] = runtime.APIKey
		}
		return agentRuntime{agent: agent, binary: a.cfg.Codex.Binary, args: args, runAsUser: a.cfg.Codex.RunAsUser, env: env, sessionDir: a.cfg.CodexSessionsDir, sessionID: session.CodexSessionID, workDir: workDir}, nil
	}
	args := []string{"--verbose"}
	if session.ProfileID != nil && *session.ProfileID != "" {
		settings := filepath.Join(a.cfg.ClaudeSettingsDir, *session.ProfileID+".json")
		if fileExists(settings) {
			args = append(args, "--settings", settings)
		}
	}
	if session.ClaudeSessionID != "" {
		args = append(args, "--resume", session.ClaudeSessionID)
	}
	if session.SkipPermissions != 0 && a.cfg.Claude.RunAsUser != "" {
		args = append(args, "--dangerously-skip-permissions")
	}
	projectDir := strings.ReplaceAll(workDir, "/", "-")
	return agentRuntime{agent: agent, binary: a.cfg.Claude.Binary, args: args, runAsUser: a.cfg.Claude.RunAsUser, env: map[string]string{}, sessionDir: filepath.Join(a.cfg.ClaudeProjectsDir, projectDir), sessionID: session.ClaudeSessionID, workDir: workDir}, nil
}

func (t *TerminalSession) readLoop() {
	defer close(t.readDone)
	buffer := make([]byte, 32*1024)
	for {
		count, err := t.pty.Read(buffer)
		if count > 0 {
			t.consumeOutput(buffer[:count])
		}
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, os.ErrClosed) && !strings.Contains(strings.ToLower(err.Error()), "input/output error") {
				t.manager.app.logError("read PTY", err)
			}
			return
		}
	}
}

func (t *TerminalSession) consumeOutput(data []byte) {
	copyData := append([]byte(nil), data...)
	var clients []*wsClient
	var assistantID string
	var messageContent []byte
	var shouldSave bool
	var scheduleSave time.Duration
	var trust, theme bool

	t.mu.Lock()
	t.output.Append(copyData)
	t.poll.Append(copyData)
	if t.currentAssistantID != "" {
		t.messageOutput.Append(copyData)
		if time.Since(t.lastMessageSave) >= 2*time.Second {
			if t.messageSaveTimer != nil {
				t.messageSaveTimer.Stop()
				t.messageSaveTimer = nil
			}
			t.lastMessageSave = time.Now()
			assistantID = t.currentAssistantID
			messageContent = t.messageOutput.Bytes()
			shouldSave = true
		} else if t.messageSaveTimer == nil {
			scheduleSave = 2*time.Second - time.Since(t.lastMessageSave)
		}
	}
	if !t.onboardingDone {
		t.onboarding.Append(copyData)
		text := string(t.onboarding.data)
		if t.agent == "codex" {
			if !t.codexStartupSent && codexNeedsStartupConfirmation(text) {
				t.codexStartupSent = true
				trust = true
			}
			if t.codexStartupSent || codexMainScreenReady(text) {
				t.onboardingDone = true
			}
		} else {
			if !t.trustSent && strings.Contains(strings.ToLower(text), "trust") {
				t.trustSent = true
				trust = true
			}
			if !t.themeSent && strings.Contains(strings.ToLower(text), "text style") {
				t.themeSent = true
				theme = true
			}
			if strings.Contains(text, "❯") {
				t.onboardingDone = true
			}
		}
	}
	for client := range t.clients {
		clients = append(clients, client)
	}
	t.mu.Unlock()
	if scheduleSave > 0 {
		t.mu.Lock()
		if t.messageSaveTimer == nil && !t.finalized && t.currentAssistantID != "" {
			t.messageSaveTimer = time.AfterFunc(scheduleSave, t.flushMessage)
		}
		t.mu.Unlock()
	}

	if shouldSave {
		if err := t.manager.app.store.updateMessage(assistantID, string(messageContent)); err != nil {
			t.manager.app.logError("persist terminal message", err)
		}
	}
	if trust {
		time.AfterFunc(800*time.Millisecond, func() { _, _ = t.Write([]byte("\r")) })
	}
	if theme {
		time.AfterFunc(300*time.Millisecond, func() { _, _ = t.Write([]byte("\r")) })
	}
	t.detectSessionID(false)
	payload, _ := json.Marshal(map[string]any{"type": "output", "data": string(copyData)})
	for _, client := range clients {
		client.enqueue(payload)
	}
}

func codexNeedsStartupConfirmation(text string) bool {
	lower := strings.ToLower(terminalCSI.ReplaceAllString(text, ""))
	return strings.Contains(lower, "welcome") && strings.Contains(lower, "codex") &&
		strings.Contains(lower, "yes") && strings.Contains(lower, "continue")
}

func codexMainScreenReady(text string) bool {
	lower := strings.ToLower(text)
	return strings.Contains(lower, "openai codex") && strings.Contains(lower, "model:") && strings.Contains(lower, "directory:")
}

func (t *TerminalSession) waitLoop() {
	err := t.cmd.Wait()
	exitCode := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = 1
			t.manager.app.logError("wait for PTY", err)
		}
	}
	// The child can exit before the PTY reader has drained the kernel buffer.
	// Give it a short chance to consume trailing output before final persistence
	// and the WebSocket exit event close the terminal.
	select {
	case <-t.readDone:
	case <-time.After(time.Second):
		_ = t.pty.Close()
		select {
		case <-t.readDone:
		case <-time.After(250 * time.Millisecond):
		}
	}
	t.finalize(exitCode)
	close(t.exited)
}

func (t *TerminalSession) finalize(exitCode int) {
	t.closeOnce.Do(func() error {
		_ = t.pty.Close()
		t.detectSessionID(true)
		t.mu.Lock()
		t.finalized = true
		if t.messageSaveTimer != nil {
			t.messageSaveTimer.Stop()
			t.messageSaveTimer = nil
		}
		assistantID := t.currentAssistantID
		content := t.messageOutput.Bytes()
		clients := make([]*wsClient, 0, len(t.clients))
		for client := range t.clients {
			clients = append(clients, client)
		}
		clear(t.clients)
		t.mu.Unlock()
		if assistantID != "" && len(content) > 0 {
			if err := t.manager.app.store.updateMessage(assistantID, string(content)); err != nil {
				t.manager.app.logError("persist final terminal message", err)
			}
		}
		if err := t.manager.app.store.setSessionStatus(t.dbID, "stopped"); err != nil {
			t.manager.app.logError("mark session stopped", err)
		}
		t.manager.finish(t.dbID, t)
		payload, _ := json.Marshal(map[string]any{"type": "exit", "exitCode": exitCode})
		for _, client := range clients {
			client.enqueueFinal(payload)
		}
		return nil
	})
}

func (t *TerminalSession) stop() {
	t.stopOnce.Do(func() {
		go func() {
			defer close(t.stopDone)
			t.mu.Lock()
			t.stopping = true
			t.mu.Unlock()
			t.detectSessionID(true)
			_ = terminateProcessGroup(t.cmd, t.exited, 2*time.Second)
			_ = t.pty.Close()
			select {
			case <-t.readDone:
			case <-time.After(2 * time.Second):
			}
			if err := t.manager.app.store.setSessionStatus(t.dbID, "stopped"); err != nil {
				t.manager.app.logError("mark stopped session", err)
			}
			t.manager.finish(t.dbID, t)
		}()
	})
	<-t.stopDone
}

func terminateProcessGroup(command *exec.Cmd, exited <-chan struct{}, grace time.Duration) error {
	if command == nil || command.Process == nil {
		return nil
	}
	pid := command.Process.Pid
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	_ = command.Process.Signal(syscall.SIGTERM)
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-exited:
		return nil
	case <-timer.C:
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_ = command.Process.Kill()
		killTimer := time.NewTimer(grace)
		defer killTimer.Stop()
		select {
		case <-exited:
		case <-killTimer.C:
		}
	}
	return nil
}

func (t *TerminalSession) Write(data []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stopping {
		return 0, os.ErrClosed
	}
	return t.pty.Write(data)
}

func (t *TerminalSession) SubmitMessage(content string) error {
	if _, err := t.Write([]byte(content)); err != nil {
		return err
	}
	// Ink-based terminal UIs can classify a single write containing a long
	// message plus CR as pasted text and keep it in the editor. Sending Enter
	// separately after a short pause reliably submits in Claude and Codex while
	// preserving the existing API response semantics.
	timer := time.NewTimer(300 * time.Millisecond)
	defer timer.Stop()
	<-timer.C
	_, err := t.Write([]byte("\r"))
	return err
}

func (t *TerminalSession) Resize(cols, rows uint16) error {
	if cols == 0 {
		cols = 120
	}
	if rows == 0 {
		rows = 40
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return pty.Setsize(t.pty, &pty.Winsize{Cols: cols, Rows: rows})
}

func (t *TerminalSession) BeginMessage(assistantID string) {
	t.mu.Lock()
	if t.messageSaveTimer != nil {
		t.messageSaveTimer.Stop()
		t.messageSaveTimer = nil
	}
	previousID := t.currentAssistantID
	previousContent := t.messageOutput.Bytes()
	t.currentAssistantID = assistantID
	t.messageOutput.Reset()
	t.lastMessageSave = time.Now()
	t.mu.Unlock()
	if previousID != "" && len(previousContent) > 0 {
		if err := t.manager.app.store.updateMessage(previousID, string(previousContent)); err != nil {
			t.manager.app.logError("persist previous terminal message", err)
		}
	}
}

func (t *TerminalSession) flushMessage() {
	t.mu.Lock()
	t.messageSaveTimer = nil
	if t.finalized || t.currentAssistantID == "" {
		t.mu.Unlock()
		return
	}
	assistantID := t.currentAssistantID
	content := t.messageOutput.Bytes()
	t.lastMessageSave = time.Now()
	t.mu.Unlock()
	if len(content) == 0 {
		return
	}
	if err := t.manager.app.store.updateMessage(assistantID, string(content)); err != nil {
		t.manager.app.logError("persist delayed terminal message", err)
	}
}

func (t *TerminalSession) OutputSnapshot() []byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.output.Bytes()
}

func (t *TerminalSession) Poll(after int64) ([]byte, int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.poll.Since(after)
}

func (t *TerminalSession) addClient(client *wsClient) {
	t.mu.Lock()
	t.clients[client] = struct{}{}
	snapshot := t.output.Bytes()
	t.mu.Unlock()
	if len(snapshot) > 0 {
		payload, _ := json.Marshal(map[string]any{"type": "output", "data": string(snapshot)})
		client.enqueue(payload)
	}
}

func (t *TerminalSession) removeClient(client *wsClient) {
	t.mu.Lock()
	delete(t.clients, client)
	t.mu.Unlock()
}

func (t *TerminalSession) detectSessionID(force bool) {
	t.mu.Lock()
	if t.agentSessionID != "" || (!force && time.Since(t.lastDetection) < time.Second) {
		t.mu.Unlock()
		return
	}
	t.lastDetection = time.Now()
	agent, sessionDir, workDir := t.agent, t.sessionDir, t.workDir
	existing := t.existingFiles
	t.mu.Unlock()

	files := listSessionFiles(sessionDir)
	type candidate struct {
		path string
		mod  time.Time
	}
	candidates := make([]candidate, 0)
	for path := range files {
		if _, exists := existing[path]; exists {
			continue
		}
		info, err := os.Stat(path)
		if err == nil {
			candidates = append(candidates, candidate{path: path, mod: info.ModTime()})
		}
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].mod.After(candidates[j].mod) })
	for _, candidate := range candidates {
		filename := strings.TrimSuffix(filepath.Base(candidate.path), ".jsonl")
		detected := filename
		if agent == "codex" {
			match := codexSessionSuffix.FindStringSubmatch(filename)
			if len(match) == 2 {
				detected = match[1]
			}
			metadata, err := readCodexSessionMeta(candidate.path)
			if err != nil || metadata.Type != "session_meta" || metadata.Payload.CWD != workDir {
				continue
			}
			detected = firstNonEmpty(metadata.Payload.SessionID, metadata.Payload.ID, detected)
		}
		if detected == "" {
			continue
		}
		t.mu.Lock()
		if t.agentSessionID == "" {
			t.agentSessionID = detected
		}
		t.mu.Unlock()
		if err := t.manager.app.store.setAgentSessionID(t.dbID, agent, detected); err != nil {
			t.manager.app.logError("save agent session id", err)
		}
		return
	}
}

type codexSessionMeta struct {
	Type    string `json:"type"`
	Payload struct {
		CWD       string `json:"cwd"`
		SessionID string `json:"session_id"`
		ID        string `json:"id"`
	} `json:"payload"`
}

func readCodexSessionMeta(path string) (codexSessionMeta, error) {
	var metadata codexSessionMeta
	file, err := os.Open(path)
	if err != nil {
		return metadata, err
	}
	defer file.Close()
	reader := bufio.NewReaderSize(file, 64*1024)
	line, err := reader.ReadBytes('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return metadata, err
	}
	if len(line) > 64*1024 {
		return metadata, errors.New("session metadata too large")
	}
	err = json.Unmarshal(bytes.TrimSpace(line), &metadata)
	return metadata, err
}

func listSessionFiles(root string) map[string]struct{} {
	files := make(map[string]struct{})
	_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			if path == root {
				return filepath.SkipDir
			}
			return nil
		}
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".jsonl") {
			files[path] = struct{}{}
		}
		return nil
	})
	return files
}
