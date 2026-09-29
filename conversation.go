package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/pelletier/go-toml/v2"
)

const (
	maxCodexJSONLine = 2 * 1024 * 1024
	maxToolOutput    = 128 * 1024
	rpcTimeout       = 20 * time.Second
)

type ConversationManager struct {
	app           *App
	mu            sync.RWMutex
	running       map[string]*AppServerSession
	claudeRunning map[string]*ClaudeConversationSession
}

type AppServerSession struct {
	manager   *ConversationManager
	sessionID string
	threadID  string
	apiKey    string
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	done      chan struct{}
	stopOnce  sync.Once

	writeMu sync.Mutex
	rpcMu   sync.Mutex
	nextID  int64
	pending map[string]chan rpcResponse

	mu          sync.Mutex
	turnRunning bool
	turnID      string
	assistantID string
	assistant   string
	stopping    bool
	toolOutput  map[string]string
}

type rpcResponse struct {
	Result json.RawMessage
	Error  *rpcError
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type codexAppRuntime struct {
	binary    string
	args      []string
	runAsUser string
	env       map[string]string
	workDir   string
	apiKey    string
	config    map[string]any
}

func newConversationManager(app *App) *ConversationManager {
	return &ConversationManager{
		app: app, running: make(map[string]*AppServerSession),
		claudeRunning: make(map[string]*ClaudeConversationSession),
	}
}

func (m *ConversationManager) get(sessionID string) *AppServerSession {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.running[sessionID]
}

func (m *ConversationManager) getClaude(sessionID string) *ClaudeConversationSession {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.claudeRunning[sessionID]
}

func (m *ConversationManager) IsRunning(sessionID string) bool {
	return m.get(sessionID) != nil || m.getClaude(sessionID) != nil
}

func (m *ConversationManager) IsTurnRunning(sessionID string) bool {
	if claude := m.getClaude(sessionID); claude != nil {
		return claude.isTurnRunning()
	}
	server := m.get(sessionID)
	if server == nil {
		return false
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	return server.turnRunning
}

func (m *ConversationManager) StartSession(session Session) error {
	if !m.app.cfg.Experimental.ConversationMode {
		return errors.New("Conversation mode is disabled")
	}
	if session.RunMode != "conversation" {
		return errors.New("Session is not configured for conversation mode")
	}
	if session.SkipPermissions == 0 {
		return errors.New("Enable Skip Perms before using conversation mode")
	}
	if session.Agent == "claude" {
		return m.startClaudeSession(session)
	}
	if session.Agent != "codex" {
		return errors.New("Unsupported conversation agent")
	}
	runtime, err := m.app.codexAppRuntime(session)
	if err != nil {
		return err
	}

	m.mu.Lock()
	if _, exists := m.running[session.ID]; exists || m.claudeRunning[session.ID] != nil {
		m.mu.Unlock()
		return errors.New("Session already running")
	}
	server := &AppServerSession{
		manager: m, sessionID: session.ID, threadID: session.CodexSessionID,
		apiKey: runtime.apiKey, done: make(chan struct{}), pending: make(map[string]chan rpcResponse),
		toolOutput: make(map[string]string),
	}
	binary, args := runtime.binary, append([]string(nil), runtime.args...)
	if runtime.runAsUser != "" {
		args = append([]string{"-u", runtime.runAsUser, "-E", binary}, args...)
		binary = "sudo"
	}
	command := exec.Command(binary, args...)
	command.Dir = runtime.workDir
	command.Env = mergedEnv(runtime.env)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := command.StdinPipe()
	if err != nil {
		m.mu.Unlock()
		return fmt.Errorf("prepare Codex input: %w", err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		m.mu.Unlock()
		return fmt.Errorf("prepare Codex output: %w", err)
	}
	stderr, err := command.StderrPipe()
	if err != nil {
		m.mu.Unlock()
		return fmt.Errorf("prepare Codex errors: %w", err)
	}
	if err := command.Start(); err != nil {
		m.mu.Unlock()
		return fmt.Errorf("start Codex app-server: %w", err)
	}
	server.cmd, server.stdin = command, stdin
	m.running[session.ID] = server
	m.mu.Unlock()
	go server.run(stdout, stderr)

	ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
	defer cancel()
	if err := server.call(ctx, "initialize", map[string]any{
		"clientInfo":   map[string]string{"name": "EasyConnect", "version": "1"},
		"capabilities": map[string]any{"experimentalApi": true},
	}, nil); err != nil {
		m.StopSession(session.ID)
		return fmt.Errorf("initialize Codex app-server: %w", err)
	}
	if err := server.notify("initialized", nil); err != nil {
		m.StopSession(session.ID)
		return fmt.Errorf("finish Codex initialization: %w", err)
	}
	threadParams := map[string]any{
		"cwd": runtime.workDir, "approvalPolicy": "never", "sandbox": "danger-full-access",
	}
	if len(runtime.config) > 0 {
		threadParams["config"] = runtime.config
	}
	method := "thread/start"
	if session.CodexSessionID != "" {
		method = "thread/resume"
		threadParams["threadId"] = session.CodexSessionID
	}
	var response struct {
		Thread struct {
			ID    string            `json:"id"`
			Turns []json.RawMessage `json:"turns"`
		} `json:"thread"`
	}
	if err := server.call(ctx, method, threadParams, &response); err != nil {
		m.StopSession(session.ID)
		return fmt.Errorf("%s: %w", method, err)
	}
	if response.Thread.ID == "" {
		m.StopSession(session.ID)
		return errors.New("Codex app-server returned an empty thread ID")
	}
	server.mu.Lock()
	server.threadID = response.Thread.ID
	server.mu.Unlock()
	if err := m.app.store.setAgentSessionID(session.ID, "codex", response.Thread.ID); err != nil {
		m.StopSession(session.ID)
		return fmt.Errorf("save Codex thread ID: %w", err)
	}
	if err := server.importTurns(response.Thread.Turns); err != nil {
		m.app.logError("import Codex conversation history", err)
	}
	if err := m.app.store.setSessionStatus(session.ID, "running"); err != nil {
		m.StopSession(session.ID)
		return err
	}
	server.emit("session.started", "", "", map[string]any{"thread_id": response.Thread.ID})
	return nil
}

func (a *App) codexAppRuntime(session Session) (codexAppRuntime, error) {
	runtime := codexRuntime{}
	args := []string{"-c", "check_for_update_on_startup=false"}
	if session.ProfileID != nil && *session.ProfileID != "" {
		profile, err := a.store.getAgentProfile(*session.ProfileID, "codex")
		if err != nil {
			return codexAppRuntime{}, errors.New("Selected Codex profile was not found")
		}
		runtime, err = parseCodexRuntime(profile.Content)
		if err != nil {
			return codexAppRuntime{}, err
		}
		args = append(args, customProviderArgs(runtime)...)
	}
	args = append(args, "app-server", "--stdio")
	env := map[string]string{"CODEX_HOME": a.cfg.CodexHome}
	if a.cfg.Codex.RunAsUser != "" {
		env["HOME"] = a.cfg.CodexUserHome
	}
	if runtime.APIKey != "" {
		env["OPENAI_API_KEY"] = runtime.APIKey
	}
	var threadConfig map[string]any
	if strings.TrimSpace(runtime.ProfileContent) != "" {
		if err := toml.Unmarshal([]byte(runtime.ProfileContent), &threadConfig); err != nil {
			return codexAppRuntime{}, fmt.Errorf("prepare Codex app-server config: %w", err)
		}
	}
	return codexAppRuntime{
		binary: a.cfg.Codex.Binary, args: args, runAsUser: a.cfg.Codex.RunAsUser,
		env: env, workDir: firstNonEmpty(session.WorkingDir, a.cfg.DefaultWorkingDir), apiKey: runtime.APIKey, config: threadConfig,
	}, nil
}

func (m *ConversationManager) StartTurn(sessionID, prompt, assistantID, clientMessageID string) (string, error) {
	if claude := m.getClaude(sessionID); claude != nil {
		return claude.startTurn(prompt, assistantID, clientMessageID)
	}
	server := m.get(sessionID)
	if server == nil {
		return "", errors.New("Conversation session is not running")
	}
	server.mu.Lock()
	if server.turnRunning {
		server.mu.Unlock()
		return "", errors.New("A conversation turn is already running")
	}
	server.turnRunning, server.assistantID, server.assistant = true, assistantID, ""
	server.turnID = ""
	server.toolOutput = make(map[string]string)
	threadID := server.threadID
	server.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
	defer cancel()
	params := map[string]any{
		"threadId":            threadID,
		"input":               []map[string]any{{"type": "text", "text": prompt}},
		"clientUserMessageId": clientMessageID,
	}
	var response struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if err := server.call(ctx, "turn/start", params, &response); err != nil {
		server.mu.Lock()
		server.turnRunning, server.assistantID = false, ""
		server.mu.Unlock()
		return "", err
	}
	server.mu.Lock()
	if server.turnID == "" {
		server.turnID = response.Turn.ID
	}
	turnID := server.turnID
	server.mu.Unlock()
	return turnID, nil
}

func (m *ConversationManager) InterruptTurn(sessionID string) bool {
	if claude := m.getClaude(sessionID); claude != nil {
		return claude.interruptTurn()
	}
	server := m.get(sessionID)
	if server == nil {
		return false
	}
	server.mu.Lock()
	if !server.turnRunning || server.turnID == "" {
		server.mu.Unlock()
		return false
	}
	threadID, turnID := server.threadID, server.turnID
	server.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.call(ctx, "turn/interrupt", map[string]string{"threadId": threadID, "turnId": turnID}, nil); err != nil {
		m.app.logError("interrupt Codex conversation turn", err)
		return false
	}
	return true
}

func (m *ConversationManager) StopSession(sessionID string) bool {
	if claude := m.getClaude(sessionID); claude != nil {
		return claude.stop()
	}
	server := m.get(sessionID)
	if server == nil {
		return false
	}
	server.stopOnce.Do(func() {
		server.mu.Lock()
		server.stopping = true
		server.mu.Unlock()
		_ = server.stdin.Close()
		_ = terminateProcessGroup(server.cmd, server.done, 2*time.Second)
	})
	<-server.done
	return true
}

func (m *ConversationManager) stopAll() {
	m.mu.RLock()
	servers := make([]*AppServerSession, 0, len(m.running))
	for _, server := range m.running {
		servers = append(servers, server)
	}
	claudeSessions := make([]*ClaudeConversationSession, 0, len(m.claudeRunning))
	for _, session := range m.claudeRunning {
		claudeSessions = append(claudeSessions, session)
	}
	m.mu.RUnlock()
	for _, server := range servers {
		m.StopSession(server.sessionID)
	}
	for _, session := range claudeSessions {
		session.stop()
	}
}

func (m *ConversationManager) finish(server *AppServerSession) {
	m.mu.Lock()
	if m.running[server.sessionID] == server {
		delete(m.running, server.sessionID)
	}
	m.mu.Unlock()
}

func (s *AppServerSession) run(stdout, stderr io.Reader) {
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
		s.consumeRPC(scanner.Bytes())
	}
	scanErr := scanner.Err()
	waitErr := s.cmd.Wait()
	<-stderrDone
	if scanErr != nil {
		s.manager.app.logError("read Codex app-server output", scanErr)
	}
	s.failPending(errors.New("Codex app-server exited"))
	s.mu.Lock()
	stopping, turnRunning, turnID := s.stopping, s.turnRunning, s.turnID
	assistantID, assistant := s.assistantID, s.assistant
	s.turnRunning = false
	s.mu.Unlock()
	if assistantID != "" && assistant == "" {
		_ = s.manager.app.store.deleteMessage(assistantID)
	}
	if turnRunning && !stopping {
		detail := safeCodexError(stderrBuffer.Bytes(), s.apiKey)
		if waitErr == nil && scanErr == nil {
			detail = "Codex app-server exited before the turn completed"
		}
		s.emit("turn.failed", turnID, "", map[string]any{"message": detail})
	}
	if err := s.manager.app.store.setSessionStatus(s.sessionID, "stopped"); err != nil {
		s.manager.app.logError("mark conversation session stopped", err)
	}
	if s.manager.app.autoContinue != nil {
		s.manager.app.autoContinue.SessionStopped(s.sessionID)
	}
	s.manager.finish(s)
	close(s.done)
}

func (s *AppServerSession) call(ctx context.Context, method string, params any, result any) error {
	id := strconv.FormatInt(s.nextRequestID(), 10)
	responseChannel := make(chan rpcResponse, 1)
	s.rpcMu.Lock()
	s.pending[id] = responseChannel
	s.rpcMu.Unlock()
	if err := s.write(map[string]any{"id": json.Number(id), "method": method, "params": params}); err != nil {
		s.rpcMu.Lock()
		delete(s.pending, id)
		s.rpcMu.Unlock()
		return err
	}
	select {
	case response := <-responseChannel:
		if response.Error != nil {
			return fmt.Errorf("Codex RPC %d: %s", response.Error.Code, response.Error.Message)
		}
		if result != nil && len(response.Result) > 0 {
			if err := json.Unmarshal(response.Result, result); err != nil {
				return fmt.Errorf("decode %s response: %w", method, err)
			}
		}
		return nil
	case <-ctx.Done():
		s.rpcMu.Lock()
		delete(s.pending, id)
		s.rpcMu.Unlock()
		return ctx.Err()
	case <-s.done:
		return errors.New("Codex app-server exited")
	}
}

func (s *AppServerSession) nextRequestID() int64 {
	s.rpcMu.Lock()
	defer s.rpcMu.Unlock()
	s.nextID++
	return s.nextID
}

func (s *AppServerSession) notify(method string, params any) error {
	message := map[string]any{"method": method}
	if params != nil {
		message["params"] = params
	}
	return s.write(message)
}

func (s *AppServerSession) write(message any) error {
	encoded, err := json.Marshal(message)
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err = s.stdin.Write(encoded)
	return err
}

func (s *AppServerSession) consumeRPC(line []byte) {
	var message struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
		Result json.RawMessage `json:"result"`
		Error  *rpcError       `json:"error"`
	}
	if json.Unmarshal(line, &message) != nil {
		return
	}
	if len(message.ID) > 0 && message.Method == "" {
		id := strings.Trim(string(message.ID), `"`)
		s.rpcMu.Lock()
		pending := s.pending[id]
		delete(s.pending, id)
		s.rpcMu.Unlock()
		if pending != nil {
			pending <- rpcResponse{Result: message.Result, Error: message.Error}
		}
		return
	}
	if len(message.ID) > 0 && message.Method != "" {
		_ = s.write(map[string]any{"id": json.RawMessage(message.ID), "error": map[string]any{"code": -32601, "message": "EasyConnect does not support interactive app-server requests"}})
		return
	}
	if message.Method != "" {
		s.consumeNotification(message.Method, message.Params)
	}
}

func (s *AppServerSession) failPending(err error) {
	s.rpcMu.Lock()
	pending := s.pending
	s.pending = make(map[string]chan rpcResponse)
	s.rpcMu.Unlock()
	for _, channel := range pending {
		channel <- rpcResponse{Error: &rpcError{Code: -32000, Message: err.Error()}}
	}
}

func (s *AppServerSession) consumeNotification(method string, raw json.RawMessage) {
	var params map[string]any
	if json.Unmarshal(raw, &params) != nil {
		return
	}
	threadID := stringField(params, "threadId")
	if threadID != "" && threadID != s.threadID {
		return
	}
	turnID := stringField(params, "turnId")
	if turn, ok := params["turn"].(map[string]any); ok && turnID == "" {
		turnID = stringField(turn, "id")
	}
	switch method {
	case "turn/started":
		s.mu.Lock()
		s.turnID = turnID
		s.turnRunning = true
		assistantID := s.assistantID
		s.mu.Unlock()
		s.emit("turn.started", turnID, "", map[string]any{"assistant_message_id": assistantID})
	case "item/agentMessage/delta":
		itemID := stringField(params, "itemId")
		delta := redactText(stringField(params, "delta"), s.apiKey)
		s.mu.Lock()
		s.assistant += delta
		content, messageID := s.assistant, s.assistantID
		s.mu.Unlock()
		if messageID != "" {
			_ = s.manager.app.store.updateMessage(messageID, content)
			_ = s.manager.app.store.setMessageSourceID(messageID, itemID)
		}
		s.emit("assistant.message", turnID, itemID, map[string]any{"message_id": messageID, "content": content})
	case "item/commandExecution/outputDelta":
		itemID, delta := stringField(params, "itemId"), redactText(stringField(params, "delta"), s.apiKey)
		s.mu.Lock()
		s.toolOutput[itemID] = truncateText(s.toolOutput[itemID]+delta, maxToolOutput)
		s.mu.Unlock()
	case "item/started", "item/completed":
		item, _ := params["item"].(map[string]any)
		s.consumeItem(method, turnID, item)
	case "turn/completed":
		turn, _ := params["turn"].(map[string]any)
		status := stringField(turn, "status")
		s.mu.Lock()
		assistantID, assistant := s.assistantID, s.assistant
		s.turnRunning = false
		s.turnID = ""
		s.assistantID = ""
		s.mu.Unlock()
		if assistantID != "" && assistant == "" {
			_ = s.manager.app.store.deleteMessage(assistantID)
		}
		typeName := "turn.completed"
		if status == "interrupted" {
			typeName = "turn.cancelled"
		}
		if status == "failed" {
			typeName = "turn.failed"
		}
		payload := map[string]any{"status": status}
		if failure, ok := turn["error"].(map[string]any); ok {
			payload["message"] = redactText(stringField(failure, "message"), s.apiKey)
		}
		s.emit(typeName, turnID, "", payload)
	case "error":
		if retrying, _ := params["willRetry"].(bool); retrying {
			return
		}
		failure, _ := params["error"].(map[string]any)
		s.emit("turn.failed", turnID, "", map[string]any{"message": redactText(stringField(failure, "message"), s.apiKey)})
	}
}

func (s *AppServerSession) consumeItem(method, turnID string, item map[string]any) {
	if item == nil {
		return
	}
	itemID, itemType := stringField(item, "id"), stringField(item, "type")
	phase := "started"
	if method == "item/completed" {
		phase = "completed"
	}
	switch itemType {
	case "agentMessage":
		content := redactText(stringField(item, "text"), s.apiKey)
		if content == "" {
			return
		}
		s.mu.Lock()
		s.assistant = content
		messageID := s.assistantID
		s.mu.Unlock()
		if messageID != "" {
			_ = s.manager.app.store.updateMessage(messageID, content)
			_ = s.manager.app.store.setMessageSourceID(messageID, itemID)
		}
		s.emit("assistant.message", turnID, itemID, map[string]any{"message_id": messageID, "content": content})
	case "commandExecution", "mcpToolCall", "webSearch", "dynamicToolCall":
		s.mu.Lock()
		streamed := s.toolOutput[itemID]
		s.mu.Unlock()
		output := firstNonEmpty(stringField(item, "aggregatedOutput"), stringField(item, "output"), streamed)
		payload := map[string]any{
			"kind": itemType, "phase": phase,
			"command": truncateText(firstNonEmpty(stringField(item, "command"), stringField(item, "query"), stringField(item, "tool")), 16*1024),
			"output":  truncateText(output, maxToolOutput), "status": stringField(item, "status"),
		}
		if value, ok := item["exitCode"]; ok {
			payload["exit_code"] = value
		}
		s.emit("tool."+phase, turnID, itemID, payload)
	case "fileChange":
		payload := map[string]any{"phase": phase, "status": stringField(item, "status")}
		if changes, ok := item["changes"]; ok {
			payload["changes"] = changes
		}
		s.emit("file.change", turnID, itemID, payload)
	case "reasoning", "plan":
		text := truncateText(firstNonEmpty(stringField(item, "text"), strings.Join(stringSlice(item["summary"]), "\n")), 32*1024)
		if text != "" {
			s.emit("activity", turnID, itemID, map[string]any{"kind": itemType, "content": text})
		}
	}
}

func (s *AppServerSession) importTurns(turns []json.RawMessage) error {
	history := make([]Message, 0)
	for _, raw := range turns {
		var turn struct {
			ID    string `json:"id"`
			Items []struct {
				ID       string `json:"id"`
				ClientID string `json:"clientId"`
				Type     string `json:"type"`
				Text     string `json:"text"`
				Content  []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"items"`
		}
		if err := json.Unmarshal(raw, &turn); err != nil {
			return err
		}
		for _, item := range turn.Items {
			role, content := "", ""
			switch item.Type {
			case "userMessage":
				role = "user"
				parts := make([]string, 0, len(item.Content))
				for _, part := range item.Content {
					if part.Type == "text" && part.Text != "" {
						parts = append(parts, part.Text)
					}
				}
				content = strings.Join(parts, "\n")
			case "agentMessage":
				role, content = "assistant", item.Text
			}
			if role == "" || content == "" {
				continue
			}
			sourceID := firstNonEmpty(item.ClientID, item.ID, turn.ID+":"+role)
			history = append(history, Message{ID: uuid.NewString(), SessionID: s.sessionID, Role: role, Content: redactText(content, s.apiKey), SourceID: sourceID})
		}
	}
	return s.manager.app.store.syncConversationHistory(s.sessionID, history)
}

func (s *AppServerSession) emit(eventType, turnID, itemID string, payload map[string]any) {
	payload = redactPayload(payload, s.apiKey).(map[string]any)
	emitConversationEvent(s.manager.app, s.sessionID, eventType, turnID, itemID, payload)
}

func stringField(value map[string]any, key string) string {
	text, _ := value[key].(string)
	return text
}
func stringSlice(value any) []string {
	raw, _ := value.([]any)
	result := make([]string, 0, len(raw))
	for _, item := range raw {
		if text, ok := item.(string); ok {
			result = append(result, text)
		}
	}
	return result
}
func truncateText(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "\n… output truncated by EasyConnect …"
}

func redactPayload(value any, secret string) any {
	switch typed := value.(type) {
	case string:
		return redactText(typed, secret)
	case []any:
		result := make([]any, len(typed))
		for index := range typed {
			result[index] = redactPayload(typed[index], secret)
		}
		return result
	case map[string]any:
		result := make(map[string]any, len(typed))
		for key, item := range typed {
			result[key] = redactPayload(item, secret)
		}
		return result
	default:
		return value
	}
}

func redactText(value, secret string) string {
	if secret == "" {
		return value
	}
	return strings.ReplaceAll(value, secret, "[REDACTED]")
}
