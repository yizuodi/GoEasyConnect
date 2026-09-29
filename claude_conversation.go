package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
)

type ClaudeConversationSession struct {
	manager         *ConversationManager
	sessionID       string
	claudeSessionID string
	cmd             *exec.Cmd
	stdin           io.WriteCloser
	done            chan struct{}
	stopOnce        sync.Once
	writeMu         sync.Mutex

	mu           sync.Mutex
	turnRunning  bool
	turnID       string
	assistantID  string
	assistant    string
	stopping     bool
	interrupting bool
	toolNames    map[string]string
	toolInputs   map[string]any
}

func (m *ConversationManager) startClaudeSession(session Session) error {
	claudeID := session.ClaudeSessionID
	resume := false
	if claudeID == "" {
		claudeID = uuid.NewString()
		if err := m.app.store.setAgentSessionID(session.ID, "claude", claudeID); err != nil {
			return fmt.Errorf("save Claude session ID: %w", err)
		}
	} else if _, err := m.app.findClaudeSessionFile(claudeID); err == nil {
		resume = true
		if err := m.importClaudeHistory(session.ID, claudeID); err != nil {
			m.app.logError("import Claude conversation history", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("locate Claude session: %w", err)
	}
	args := []string{
		"--print", "--input-format", "stream-json", "--output-format", "stream-json",
		"--verbose", "--replay-user-messages", "--dangerously-skip-permissions",
	}
	if session.ProfileID != nil && *session.ProfileID != "" {
		settings := filepath.Join(m.app.cfg.ClaudeSettingsDir, *session.ProfileID+".json")
		if fileExists(settings) {
			args = append(args, "--settings", settings)
		}
	}
	if resume {
		args = append(args, "--resume", claudeID)
	} else {
		args = append(args, "--session-id", claudeID)
	}
	binary := m.app.cfg.Claude.Binary
	spawnArgs := append([]string(nil), args...)
	if m.app.cfg.Claude.RunAsUser != "" {
		spawnArgs = append([]string{"-u", m.app.cfg.Claude.RunAsUser, "-E", binary}, spawnArgs...)
		binary = "sudo"
	}
	command := exec.Command(binary, spawnArgs...)
	command.Dir = firstNonEmpty(session.WorkingDir, m.app.cfg.DefaultWorkingDir)
	env := map[string]string{"HOME": m.app.cfg.ClaudeUserHome}
	if m.app.cfg.Claude.RunAsUser != "" {
		env["USER"] = m.app.cfg.Claude.RunAsUser
		env["LOGNAME"] = m.app.cfg.Claude.RunAsUser
		if account, err := user.Lookup(m.app.cfg.Claude.RunAsUser); err == nil {
			env["HOME"] = account.HomeDir
		}
	}
	command.Env = mergedEnv(env)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := command.StdinPipe()
	if err != nil {
		return fmt.Errorf("prepare Claude input: %w", err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		return fmt.Errorf("prepare Claude output: %w", err)
	}
	stderr, err := command.StderrPipe()
	if err != nil {
		return fmt.Errorf("prepare Claude errors: %w", err)
	}
	conversation := &ClaudeConversationSession{
		manager: m, sessionID: session.ID, claudeSessionID: claudeID, cmd: command,
		stdin: stdin, done: make(chan struct{}),
		toolNames: make(map[string]string), toolInputs: make(map[string]any),
	}
	m.mu.Lock()
	if _, exists := m.running[session.ID]; exists || m.claudeRunning[session.ID] != nil {
		m.mu.Unlock()
		return errors.New("Session already running")
	}
	if err := command.Start(); err != nil {
		m.mu.Unlock()
		return fmt.Errorf("start Claude stream-json session: %w", err)
	}
	m.claudeRunning[session.ID] = conversation
	m.mu.Unlock()
	go conversation.run(stdout, stderr)
	startupTimer := time.NewTimer(250 * time.Millisecond)
	select {
	case <-conversation.done:
		startupTimer.Stop()
		return errors.New("Claude stream-json process exited during startup")
	case <-startupTimer.C:
	}
	if err := m.app.store.setSessionStatus(session.ID, "running"); err != nil {
		conversation.stop()
		return err
	}
	if !m.IsRunning(session.ID) {
		_ = m.app.store.setSessionStatus(session.ID, "stopped")
		return errors.New("Claude stream-json process exited during startup")
	}
	conversation.emit("session.started", "", "", map[string]any{"session_id": claudeID})
	return nil
}

func (s *ClaudeConversationSession) isTurnRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.turnRunning
}

func (s *ClaudeConversationSession) startTurn(prompt, assistantID, clientMessageID string) (string, error) {
	s.mu.Lock()
	if s.turnRunning {
		s.mu.Unlock()
		return "", errors.New("A conversation turn is already running")
	}
	turnID := uuid.NewString()
	s.turnRunning, s.turnID, s.assistantID, s.assistant = true, turnID, assistantID, ""
	s.interrupting = false
	s.toolNames, s.toolInputs = make(map[string]string), make(map[string]any)
	claudeID := s.claudeSessionID
	s.mu.Unlock()
	message := map[string]any{
		"type": "user", "session_id": claudeID, "parent_tool_use_id": nil,
		"message": map[string]any{"role": "user", "content": prompt},
	}
	if err := s.write(message); err != nil {
		s.mu.Lock()
		s.turnRunning, s.turnID, s.assistantID = false, "", ""
		s.mu.Unlock()
		return "", err
	}
	s.emit("turn.started", turnID, "", map[string]any{"assistant_message_id": assistantID, "client_message_id": clientMessageID})
	return turnID, nil
}

func (s *ClaudeConversationSession) interruptTurn() bool {
	s.mu.Lock()
	if !s.turnRunning {
		s.mu.Unlock()
		return false
	}
	s.interrupting = true
	s.mu.Unlock()
	requestID := uuid.NewString()
	err := s.write(map[string]any{
		"type": "control_request", "request_id": requestID,
		"request": map[string]string{"subtype": "interrupt"},
	})
	if err != nil {
		s.manager.app.logError("interrupt Claude conversation turn", err)
		return false
	}
	return true
}

func (s *ClaudeConversationSession) stop() bool {
	s.stopOnce.Do(func() {
		s.mu.Lock()
		s.stopping = true
		s.mu.Unlock()
		_ = s.stdin.Close()
		_ = terminateProcessGroup(s.cmd, s.done, 2*time.Second)
	})
	<-s.done
	return true
}

func (s *ClaudeConversationSession) write(value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err = s.stdin.Write(encoded)
	return err
}

func (s *ClaudeConversationSession) run(stdout, stderr io.Reader) {
	var stderrBuffer byteRing
	stderrBuffer.max = 64 * 1024
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		buffer := make([]byte, 8192)
		for {
			count, err := stderr.Read(buffer)
			if count > 0 {
				stderrBuffer.Append(buffer[:count])
			}
			if err != nil {
				return
			}
		}
	}()
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), maxCodexJSONLine)
	for scanner.Scan() {
		s.consume(scanner.Bytes())
	}
	scanErr := scanner.Err()
	waitErr := s.cmd.Wait()
	<-stderrDone
	if waitErr != nil {
		s.manager.app.logError("Claude stream-json process", waitErr)
	}
	if scanErr != nil {
		s.manager.app.logError("read Claude stream-json output", scanErr)
	}
	s.mu.Lock()
	stopping, running, turnID := s.stopping, s.turnRunning, s.turnID
	assistantID, assistant := s.assistantID, s.assistant
	s.turnRunning = false
	s.mu.Unlock()
	if assistantID != "" && assistant == "" {
		_ = s.manager.app.store.deleteMessage(assistantID)
	}
	if running && !stopping {
		detail := "Claude stream-json process exited before the turn completed"
		if waitErr != nil || len(stderrBuffer.Bytes()) > 0 {
			detail = "Claude Code exited unexpectedly"
		}
		s.emit("turn.failed", turnID, "", map[string]any{"message": detail})
	}
	s.manager.mu.Lock()
	if s.manager.claudeRunning[s.sessionID] == s {
		delete(s.manager.claudeRunning, s.sessionID)
	}
	s.manager.mu.Unlock()
	if err := s.manager.app.store.setSessionStatus(s.sessionID, "stopped"); err != nil {
		s.manager.app.logError("mark Claude conversation stopped", err)
	}
	if s.manager.app.autoContinue != nil {
		s.manager.app.autoContinue.SessionStopped(s.sessionID)
	}
	close(s.done)
}

func (s *ClaudeConversationSession) consume(line []byte) {
	var event map[string]any
	if json.Unmarshal(line, &event) != nil {
		return
	}
	typeName := stringField(event, "type")
	switch typeName {
	case "system":
		if stringField(event, "subtype") == "init" {
			if sessionID := stringField(event, "session_id"); sessionID != "" {
				s.mu.Lock()
				s.claudeSessionID = sessionID
				s.mu.Unlock()
				if err := s.manager.app.store.setAgentSessionID(s.sessionID, "claude", sessionID); err != nil {
					s.manager.app.logError("save Claude conversation session ID", err)
				}
			}
		}
	case "assistant":
		s.consumeAssistant(event)
	case "user":
		s.consumeToolResults(event)
	case "result":
		s.finishTurn(event)
	}
}

func (s *ClaudeConversationSession) consumeAssistant(event map[string]any) {
	message, _ := event["message"].(map[string]any)
	content, _ := message["content"].([]any)
	messageSource := firstNonEmpty(stringField(event, "uuid"), stringField(message, "id"))
	for _, raw := range content {
		item, _ := raw.(map[string]any)
		switch stringField(item, "type") {
		case "text":
			text := stringField(item, "text")
			if text == "" {
				continue
			}
			s.mu.Lock()
			s.assistant += text
			assistant, assistantID, turnID := s.assistant, s.assistantID, s.turnID
			s.mu.Unlock()
			if assistantID != "" {
				_ = s.manager.app.store.updateMessage(assistantID, assistant)
				if messageSource != "" {
					_ = s.manager.app.store.setMessageSourceID(assistantID, messageSource)
				}
			}
			s.emit("assistant.message", turnID, messageSource, map[string]any{"message_id": assistantID, "content": assistant})
		case "thinking":
			s.emit("activity", s.currentTurnID(), messageSource, map[string]any{"kind": "reasoning", "content": truncateText(stringField(item, "thinking"), 32*1024)})
		case "tool_use":
			toolID, name := stringField(item, "id"), stringField(item, "name")
			input := item["input"]
			s.mu.Lock()
			s.toolNames[toolID], s.toolInputs[toolID] = name, input
			s.mu.Unlock()
			s.emit("tool.started", s.currentTurnID(), toolID, map[string]any{
				"kind": name, "phase": "started", "command": claudeToolSummary(name, input), "status": "inProgress",
			})
		}
	}
}

func (s *ClaudeConversationSession) consumeToolResults(event map[string]any) {
	message, _ := event["message"].(map[string]any)
	content, _ := message["content"].([]any)
	for _, raw := range content {
		item, _ := raw.(map[string]any)
		if stringField(item, "type") != "tool_result" {
			continue
		}
		toolID := stringField(item, "tool_use_id")
		s.mu.Lock()
		name, input := s.toolNames[toolID], s.toolInputs[toolID]
		s.mu.Unlock()
		output := claudeContentText(item["content"])
		status := "completed"
		if failed, _ := item["is_error"].(bool); failed {
			status = "failed"
		}
		s.emit("tool.completed", s.currentTurnID(), toolID, map[string]any{
			"kind": name, "phase": "completed", "command": claudeToolSummary(name, input),
			"output": truncateText(output, maxToolOutput), "status": status,
		})
	}
}

func (s *ClaudeConversationSession) finishTurn(event map[string]any) {
	s.mu.Lock()
	turnID, assistantID, assistant := s.turnID, s.assistantID, s.assistant
	interrupted := s.interrupting
	s.turnRunning, s.turnID, s.assistantID, s.interrupting = false, "", "", false
	s.mu.Unlock()
	if assistantID != "" && assistant == "" {
		_ = s.manager.app.store.deleteMessage(assistantID)
	}
	isError, _ := event["is_error"].(bool)
	subtype := stringField(event, "subtype")
	eventType := "turn.completed"
	if interrupted {
		eventType = "turn.cancelled"
	} else if isError || subtype != "success" {
		eventType = "turn.failed"
	}
	payload := map[string]any{"status": subtype}
	if isError {
		payload["message"] = "Claude Code could not complete this turn"
	}
	s.emit(eventType, turnID, "", payload)
}

func (s *ClaudeConversationSession) currentTurnID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.turnID
}

func (s *ClaudeConversationSession) emit(eventType, turnID, itemID string, payload map[string]any) {
	emitConversationEvent(s.manager.app, s.sessionID, eventType, turnID, itemID, payload)
}

func (m *ConversationManager) importClaudeHistory(sessionID, claudeID string) error {
	path, err := m.app.findClaudeSessionFile(claudeID)
	if err != nil {
		return err
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	history := make([]Message, 0)
	assistantContent := ""
	assistantSource := ""
	flushAssistant := func() {
		if strings.TrimSpace(assistantContent) == "" {
			assistantContent, assistantSource = "", ""
			return
		}
		if assistantSource == "" {
			assistantSource = uuid.NewString()
		}
		history = append(history, Message{ID: uuid.NewString(), SessionID: sessionID, Role: "assistant", Content: assistantContent, SourceID: assistantSource})
		assistantContent, assistantSource = "", ""
	}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), maxCodexJSONLine)
	for scanner.Scan() {
		var record map[string]any
		if json.Unmarshal(scanner.Bytes(), &record) != nil {
			continue
		}
		role := stringField(record, "type")
		if role != "user" && role != "assistant" {
			continue
		}
		if boolField(record, "isSidechain") || boolField(record, "isMeta") || boolField(record, "isCompactSummary") {
			continue
		}
		message, _ := record["message"].(map[string]any)
		if role == "user" {
			text := claudeUserText(message["content"])
			if strings.TrimSpace(text) == "" {
				continue
			}
			flushAssistant()
			sourceID := firstNonEmpty(stringField(record, "uuid"), stringField(message, "id"))
			if sourceID == "" {
				sourceID = uuid.NewString()
			}
			history = append(history, Message{ID: uuid.NewString(), SessionID: sessionID, Role: "user", Content: text, SourceID: sourceID})
			continue
		}
		text := claudeTextBlocks(message["content"])
		if text != "" {
			assistantContent += text
			assistantSource = firstNonEmpty(stringField(record, "uuid"), stringField(message, "id"), assistantSource)
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	flushAssistant()
	return m.app.store.syncConversationHistory(sessionID, history)
}

func findClaudeSessionFile(root, sessionID string) (string, error) {
	var selected string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			if path == root {
				return walkErr
			}
			return nil
		}
		if !entry.IsDir() && entry.Name() == sessionID+".jsonl" {
			selected = path
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if selected == "" {
		return "", os.ErrNotExist
	}
	return selected, nil
}

func (a *App) findClaudeSessionFile(sessionID string) (string, error) {
	roots := []string{a.cfg.ClaudeProjectsDir}
	if a.cfg.Claude.RunAsUser != "" {
		userRoot := filepath.Join(homeForRunAsUser(a.cfg.Claude.RunAsUser, a.cfg.ClaudeUserHome), ".claude", "projects")
		if userRoot != roots[0] {
			roots = append(roots, userRoot)
		}
	}
	var lastErr error
	for _, root := range roots {
		path, err := findClaudeSessionFile(root, sessionID)
		if err == nil {
			return path, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = os.ErrNotExist
	}
	return "", lastErr
}

func claudeTextBlocks(value any) string {
	items, _ := value.([]any)
	parts := make([]string, 0)
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		if stringField(item, "type") == "text" && stringField(item, "text") != "" {
			parts = append(parts, stringField(item, "text"))
		}
	}
	return strings.Join(parts, "")
}

func claudeUserText(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	items, _ := value.([]any)
	parts := make([]string, 0, len(items))
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		if stringField(item, "type") == "text" && stringField(item, "text") != "" {
			parts = append(parts, stringField(item, "text"))
		}
	}
	return strings.Join(parts, "")
}

func boolField(value map[string]any, key string) bool {
	result, _ := value[key].(bool)
	return result
}

func claudeContentText(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	items, _ := value.([]any)
	parts := make([]string, 0, len(items))
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		if text := firstNonEmpty(stringField(item, "text"), stringField(item, "content")); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n")
}

func claudeToolSummary(name string, input any) string {
	values, _ := input.(map[string]any)
	for _, key := range []string{"command", "query", "pattern", "file_path", "path", "description"} {
		if text := stringField(values, key); text != "" {
			return truncateText(text, 16*1024)
		}
	}
	encoded, _ := json.Marshal(input)
	if len(encoded) == 0 || string(encoded) == "null" {
		return name
	}
	return truncateText(string(encoded), 16*1024)
}

func emitConversationEvent(app *App, sessionID, eventType, turnID, itemID string, payload map[string]any) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return
	}
	if len(encoded) > maxToolOutput {
		payload = map[string]any{"message": "Event details exceeded the display limit", "truncated": true}
		encoded, _ = json.Marshal(payload)
	}
	event := ConversationEvent{ID: uuid.NewString(), SessionID: sessionID, TurnID: turnID, ItemID: itemID, Type: eventType, Payload: payload}
	if err := app.store.createConversationEvent(&event, encoded); err != nil {
		app.logError("save conversation event", err)
	}
}
