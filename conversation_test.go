package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCodexAppServerPersistsAcrossTurns(t *testing.T) {
	cfg := testConfig(t)
	cfg.Experimental.ConversationMode = true
	if err := os.MkdirAll(cfg.DefaultWorkingDir, 0o700); err != nil {
		t.Fatal(err)
	}
	requestLog := filepath.Join(cfg.BaseDir, "app-server-requests.jsonl")
	cfg.Codex.Binary = fakeAppServerLauncher(t, requestLog)
	app := testApp(t, cfg)
	session := Session{ID: "conversation-session", Name: "conversation", Agent: "codex", WorkingDir: cfg.DefaultWorkingDir, RunMode: "conversation", SkipPermissions: 1}
	if err := app.store.createSession(session); err != nil {
		t.Fatal(err)
	}
	if err := app.store.setSkipPermissions(session.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := app.conversations.StartSession(session); err != nil {
		t.Fatal(err)
	}

	for index, prompt := range []string{"hello", "again"} {
		userID, assistantID := fmt.Sprintf("user-%d", index), fmt.Sprintf("assistant-%d", index)
		if err := app.store.createConversationMessage(Message{ID: userID, SessionID: session.ID, Role: "user", Content: prompt}); err != nil {
			t.Fatal(err)
		}
		if err := app.store.createConversationMessage(Message{ID: assistantID, SessionID: session.ID, Role: "assistant"}); err != nil {
			t.Fatal(err)
		}
		if _, err := app.conversations.StartTurn(session.ID, prompt, assistantID, userID); err != nil {
			t.Fatal(err)
		}
		waitForTurn(t, app, session.ID)
		if !app.conversations.IsRunning(session.ID) {
			t.Fatal("app-server exited after a completed turn")
		}
	}

	messages, err := app.store.listMessagesByChannel(session.ID, "conversation", 10, 0)
	if err != nil || len(messages) != 4 || messages[1].Content != "answer: hello" || messages[3].Content != "answer: again" {
		t.Fatalf("messages=%#v err=%v", messages, err)
	}
	stored, err := app.store.getSession(session.ID)
	if err != nil || stored.CodexSessionID != fakeThreadID {
		t.Fatalf("session=%#v err=%v", stored, err)
	}
	events, err := app.store.listConversationEvents(session.ID, 0, 100)
	if err != nil || !containsConversationEvent(events, "tool.completed") || !containsConversationEvent(events, "turn.completed") {
		t.Fatalf("events=%#v err=%v", events, err)
	}
	if !conversationEventHasAssistantID(events, "turn.started") {
		t.Fatalf("turn event is not linked to its assistant message: %#v", events)
	}
	requests := readRequestMethods(t, requestLog)
	if countValue(requests, "initialize") != 1 || countValue(requests, "thread/start") != 1 || countValue(requests, "turn/start") != 2 {
		t.Fatalf("unexpected requests: %v", requests)
	}
	if !app.conversations.StopSession(session.ID) || app.conversations.IsRunning(session.ID) {
		t.Fatal("app-server did not stop")
	}
}

func TestCodexAppServerResumeImportsHistoryWithoutDuplicates(t *testing.T) {
	cfg := testConfig(t)
	cfg.Experimental.ConversationMode = true
	requestLog := filepath.Join(cfg.BaseDir, "resume-requests.jsonl")
	cfg.Codex.Binary = fakeAppServerLauncher(t, requestLog)
	app := testApp(t, cfg)
	session := Session{ID: "resume-session", Agent: "codex", WorkingDir: cfg.DefaultWorkingDir, RunMode: "conversation", SkipPermissions: 1, CodexSessionID: fakeThreadID}
	if err := os.MkdirAll(cfg.DefaultWorkingDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := app.store.createSession(session); err != nil {
		t.Fatal(err)
	}
	if err := app.store.setSkipPermissions(session.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := app.store.setAgentSessionID(session.ID, "codex", fakeThreadID); err != nil {
		t.Fatal(err)
	}
	// Live notifications and resumed history can use different source IDs for
	// the same logical messages. Also model the duplicate left by an older
	// import so the first resume repairs existing databases.
	for _, message := range []Message{
		{ID: "live-user", SessionID: session.ID, Role: "user", Content: "historical question", SourceID: "local-user-id"},
		{ID: "live-agent", SessionID: session.ID, Role: "assistant", Content: "historical answer", SourceID: "msg_live_agent"},
		{ID: "old-import-agent", SessionID: session.ID, Role: "assistant", Content: "historical answer", SourceID: "history-agent"},
	} {
		if err := app.store.createConversationMessage(message); err != nil {
			t.Fatal(err)
		}
	}
	for attempt := 0; attempt < 2; attempt++ {
		if err := app.conversations.StartSession(session); err != nil {
			t.Fatal(err)
		}
		app.conversations.StopSession(session.ID)
	}
	messages, err := app.store.listMessagesByChannel(session.ID, "conversation", 20, 0)
	if err != nil || len(messages) != 2 || messages[0].Content != "historical question" || messages[1].Content != "historical answer" {
		t.Fatalf("imported messages=%#v err=%v", messages, err)
	}
	var retainedID, sourceID string
	if err := app.store.db.QueryRow(`SELECT id,source_id FROM messages WHERE session_id=? AND role='assistant'`, session.ID).Scan(&retainedID, &sourceID); err != nil {
		t.Fatal(err)
	}
	if retainedID != "live-agent" || sourceID != "history-agent" {
		t.Fatalf("reconciled assistant id=%q source=%q", retainedID, sourceID)
	}
	if countValue(readRequestMethods(t, requestLog), "thread/resume") != 2 {
		t.Fatal("thread was not resumed twice")
	}
}

func TestCodexAppServerInterruptsTurnWithoutStoppingSession(t *testing.T) {
	cfg := testConfig(t)
	cfg.Experimental.ConversationMode = true
	t.Setenv("FAKE_CODEX_HOLD_TURN", "1")
	requestLog := filepath.Join(cfg.BaseDir, "interrupt-requests.jsonl")
	cfg.Codex.Binary = fakeAppServerLauncher(t, requestLog)
	app := testApp(t, cfg)
	session := Session{ID: "interrupt-session", Agent: "codex", WorkingDir: cfg.DefaultWorkingDir, RunMode: "conversation", SkipPermissions: 1}
	if err := os.MkdirAll(cfg.DefaultWorkingDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := app.store.createSession(session); err != nil {
		t.Fatal(err)
	}
	if err := app.store.setSkipPermissions(session.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := app.conversations.StartSession(session); err != nil {
		t.Fatal(err)
	}
	if err := app.store.createConversationMessage(Message{ID: "assistant", SessionID: session.ID, Role: "assistant"}); err != nil {
		t.Fatal(err)
	}
	if _, err := app.conversations.StartTurn(session.ID, "wait", "assistant", "user"); err != nil {
		t.Fatal(err)
	}
	if !app.conversations.InterruptTurn(session.ID) {
		t.Fatal("turn interrupt was not sent")
	}
	waitForTurn(t, app, session.ID)
	if !app.conversations.IsRunning(session.ID) {
		t.Fatal("interrupt stopped the app-server")
	}
	messages, err := app.store.listMessagesByChannel(session.ID, "conversation", 10, 0)
	if err != nil || len(messages) != 0 {
		t.Fatalf("empty interrupted response was retained: messages=%#v err=%v", messages, err)
	}
	methods := readRequestMethods(t, requestLog)
	if countValue(methods, "turn/interrupt") != 1 {
		t.Fatalf("requests=%v", methods)
	}
}

func TestCodexAppRuntimeSecretAndArgumentPlacement(t *testing.T) {
	cfg := testConfig(t)
	app := testApp(t, cfg)
	secret := "never-store-this-secret"
	profile := Profile{ID: "codex-profile", Name: "codex-profile", Agent: "codex", Content: "api_key = \"" + secret + "\"\nbase_url = \"https://provider.example/v1\"\nmodel = \"provider-model\"\nwire_api = \"responses\"\n"}
	if err := app.store.createProfile(profile); err != nil {
		t.Fatal(err)
	}
	profileID := profile.ID
	runtime, err := app.codexAppRuntime(Session{ID: "runtime", Agent: "codex", ProfileID: &profileID, WorkingDir: cfg.DefaultWorkingDir})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(runtime.args, " ")
	if strings.Contains(joined, "--profile") || !strings.Contains(joined, `model_provider="easyconnect"`) || !strings.HasSuffix(joined, "app-server --stdio") {
		t.Fatalf("args=%q", joined)
	}
	if strings.Contains(joined, secret) || runtime.env["OPENAI_API_KEY"] != secret {
		t.Fatalf("credential placement args=%q", joined)
	}
	encodedConfig, err := json.Marshal(runtime.config)
	if err != nil || strings.Contains(string(encodedConfig), secret) || runtime.config["model"] != "provider-model" {
		t.Fatalf("unsafe or incomplete thread config=%s err=%v", encodedConfig, err)
	}
	payload := redactPayload(map[string]any{"output": "token=" + secret}, secret).(map[string]any)
	if strings.Contains(payload["output"].(string), secret) {
		t.Fatalf("secret was not redacted: %#v", payload)
	}
}

func TestConversationModeValidation(t *testing.T) {
	cfg := testConfig(t)
	app := testApp(t, cfg)
	if err := app.conversations.StartSession(Session{Agent: "codex", RunMode: "conversation", SkipPermissions: 1}); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("disabled error=%v", err)
	}
	cfg.Experimental.ConversationMode = true
	if err := app.conversations.StartSession(Session{Agent: "claude", RunMode: "conversation", SkipPermissions: 1}); err == nil || !strings.Contains(err.Error(), "Codex only") {
		t.Fatalf("Claude error=%v", err)
	}
	if err := app.conversations.StartSession(Session{Agent: "codex", RunMode: "conversation"}); err == nil || !strings.Contains(err.Error(), "Skip Perms") {
		t.Fatalf("permissions error=%v", err)
	}
}

type testWriteCloser struct{ bytes.Buffer }

func (w *testWriteCloser) Close() error { return nil }

func TestRPCResponseCorrelationAndServerRequestRejection(t *testing.T) {
	writer := &testWriteCloser{}
	server := &AppServerSession{stdin: writer, pending: make(map[string]chan rpcResponse)}
	channel := make(chan rpcResponse, 1)
	server.pending["7"] = channel
	server.consumeRPC([]byte(`{"id":7,"result":{"ok":true}}`))
	select {
	case response := <-channel:
		if !bytes.Contains(response.Result, []byte(`"ok":true`)) {
			t.Fatalf("result=%s", response.Result)
		}
	default:
		t.Fatal("response was not correlated")
	}
	server.consumeRPC([]byte(`{"id":9,"method":"item/tool/requestUserInput","params":{}}`))
	if !strings.Contains(writer.String(), `"code":-32601`) {
		t.Fatalf("server request response=%q", writer.String())
	}
	server.consumeRPC([]byte(`{"method":"unknown/event","params":{}}`))
}

const fakeThreadID = "01999999-1111-7222-8333-444444444444"

func fakeAppServerLauncher(t *testing.T, requestLog string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-codex")
	script := "#!/bin/sh\nexec \"$FAKE_CODEX_TEST_BINARY\" -test.run=TestFakeCodexAppServerProcess -- \"$@\"\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_CODEX_TEST_BINARY", os.Args[0])
	t.Setenv("FAKE_CODEX_REQUEST_LOG", requestLog)
	t.Setenv("FAKE_CODEX_HELPER", "1")
	return path
}

func TestFakeCodexAppServerProcess(t *testing.T) {
	if os.Getenv("FAKE_CODEX_HELPER") != "1" {
		return
	}
	logFile, err := os.OpenFile(os.Getenv("FAKE_CODEX_REQUEST_LOG"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		os.Exit(2)
	}
	defer logFile.Close()
	turnNumber := 0
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		_, _ = logFile.Write(append(line, '\n'))
		_ = logFile.Sync()
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params map[string]any  `json:"params"`
		}
		if json.Unmarshal(line, &request) != nil || len(request.ID) == 0 {
			continue
		}
		switch request.Method {
		case "initialize":
			fakeRPCResult(request.ID, map[string]any{"codexHome": "/tmp/codex", "platformFamily": "unix", "platformOs": "linux", "userAgent": "fake"})
		case "thread/start":
			fakeRPCResult(request.ID, map[string]any{"thread": map[string]any{"id": fakeThreadID, "turns": []any{}}})
		case "thread/resume":
			turns := []any{map[string]any{"id": "history-turn", "status": "completed", "items": []any{
				map[string]any{"id": "history-user", "type": "userMessage", "content": []any{map[string]any{"type": "text", "text": "historical question"}}},
				map[string]any{"id": "history-agent", "type": "agentMessage", "text": "historical answer"},
			}}}
			fakeRPCResult(request.ID, map[string]any{"thread": map[string]any{"id": fakeThreadID, "turns": turns}})
		case "turn/start":
			turnNumber++
			turnID := fmt.Sprintf("turn-%d", turnNumber)
			fakeRPCResult(request.ID, map[string]any{"turn": map[string]any{"id": turnID, "status": "inProgress", "items": []any{}}})
			fakeNotification("turn/started", map[string]any{"threadId": fakeThreadID, "turn": map[string]any{"id": turnID, "status": "inProgress", "items": []any{}}})
			if os.Getenv("FAKE_CODEX_HOLD_TURN") == "1" {
				continue
			}
			prompt := ""
			if input, ok := request.Params["input"].([]any); ok && len(input) > 0 {
				if first, ok := input[0].(map[string]any); ok {
					prompt, _ = first["text"].(string)
				}
			}
			itemID := fmt.Sprintf("agent-%d", turnNumber)
			fakeNotification("item/started", map[string]any{"threadId": fakeThreadID, "turnId": turnID, "startedAtMs": 1, "item": map[string]any{"id": "tool-1", "type": "commandExecution", "command": "pwd", "commandActions": []any{}, "cwd": "/tmp", "status": "inProgress"}})
			fakeNotification("item/commandExecution/outputDelta", map[string]any{"threadId": fakeThreadID, "turnId": turnID, "itemId": "tool-1", "delta": "/tmp\\n"})
			fakeNotification("item/completed", map[string]any{"threadId": fakeThreadID, "turnId": turnID, "completedAtMs": 2, "item": map[string]any{"id": "tool-1", "type": "commandExecution", "command": "pwd", "commandActions": []any{}, "cwd": "/tmp", "status": "completed", "aggregatedOutput": "/tmp\\n", "exitCode": 0}})
			fakeNotification("item/agentMessage/delta", map[string]any{"threadId": fakeThreadID, "turnId": turnID, "itemId": itemID, "delta": "answer: "})
			fakeNotification("item/agentMessage/delta", map[string]any{"threadId": fakeThreadID, "turnId": turnID, "itemId": itemID, "delta": prompt})
			fakeNotification("item/completed", map[string]any{"threadId": fakeThreadID, "turnId": turnID, "completedAtMs": 3, "item": map[string]any{"id": itemID, "type": "agentMessage", "text": "answer: " + prompt}})
			fakeNotification("turn/completed", map[string]any{"threadId": fakeThreadID, "turn": map[string]any{"id": turnID, "status": "completed", "items": []any{}}})
		case "turn/interrupt":
			fakeRPCResult(request.ID, map[string]any{})
			turnID, _ := request.Params["turnId"].(string)
			fakeNotification("turn/completed", map[string]any{"threadId": fakeThreadID, "turn": map[string]any{"id": turnID, "status": "interrupted", "items": []any{}}})
		}
	}
	os.Exit(0)
}

func fakeRPCResult(id json.RawMessage, result any) {
	encoded, _ := json.Marshal(map[string]any{"id": id, "result": result})
	fmt.Println(string(encoded))
}
func fakeNotification(method string, params any) {
	encoded, _ := json.Marshal(map[string]any{"method": method, "params": params})
	fmt.Println(string(encoded))
}

func waitForTurn(t *testing.T, app *App, sessionID string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !app.conversations.IsTurnRunning(sessionID) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("conversation turn did not finish")
}

func readRequestMethods(t *testing.T, path string) []string {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	methods := []string{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var request struct {
			Method string `json:"method"`
		}
		if json.Unmarshal(scanner.Bytes(), &request) == nil && request.Method != "" {
			methods = append(methods, request.Method)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return methods
}
func countValue(values []string, expected string) int {
	count := 0
	for _, value := range values {
		if value == expected {
			count++
		}
	}
	return count
}
func containsConversationEvent(events []ConversationEvent, eventType string) bool {
	for _, event := range events {
		if event.Type == eventType {
			return true
		}
	}
	return false
}

func conversationEventHasAssistantID(events []ConversationEvent, eventType string) bool {
	for _, event := range events {
		if event.Type != eventType {
			continue
		}
		payload, _ := event.Payload.(map[string]any)
		if stringField(payload, "assistant_message_id") != "" {
			return true
		}
	}
	return false
}

var _ io.WriteCloser = (*testWriteCloser)(nil)
