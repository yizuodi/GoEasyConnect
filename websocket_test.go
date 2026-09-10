package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestWebSocketAuthenticationAndOutput(t *testing.T) {
	cfg := testConfig(t)
	app := testApp(t, cfg)
	server := httptest.NewServer(app.routes())
	defer server.Close()
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"

	_, response, err := websocket.Dial(context.Background(), wsURL+"?ticket=wrong", nil)
	if err == nil || response == nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthorized dial response=%v err=%v", response, err)
	}
	terminal := &TerminalSession{
		manager: app.sessions, dbID: "ws-session", output: byteRing{max: 1024},
		poll: pollBuffer{maxBytes: 1024, maxItems: 10}, clients: make(map[*wsClient]struct{}), exited: make(chan struct{}), readDone: make(chan struct{}),
	}
	terminal.output.Append([]byte("embedded-output"))
	app.sessions.mu.Lock()
	app.sessions.running[terminal.dbID] = terminal
	app.sessions.mu.Unlock()
	t.Cleanup(func() {
		app.sessions.mu.Lock()
		delete(app.sessions.running, terminal.dbID)
		app.sessions.mu.Unlock()
	})

	ticket := issueWebSocketTicket(t, server.URL, cfg.Auth.Password)
	connection, _, err := websocket.Dial(context.Background(), wsURL+"?ticket="+ticket, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.CloseNow()
	message, _ := json.Marshal(wsMessage{Type: "subscribe", SessionID: terminal.dbID})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := connection.Write(ctx, websocket.MessageText, message); err != nil {
		t.Fatal(err)
	}
	_, payload, err := connection.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(payload), "embedded-output") {
		t.Fatalf("unexpected WebSocket payload: %s", payload)
	}
	_, response, err = websocket.Dial(context.Background(), wsURL+"?ticket="+ticket, nil)
	if err == nil || response == nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("reused ticket response=%v err=%v", response, err)
	}
}

func TestWebSocketReceivesExitBeforeSessionCleanup(t *testing.T) {
	cfg := testConfig(t)
	app := testApp(t, cfg)
	server := httptest.NewServer(app.routes())
	defer server.Close()

	session := Session{ID: "ws-exit-session", Name: "exit", Agent: "claude", WorkingDir: cfg.DefaultWorkingDir}
	if err := app.store.createSession(session); err != nil {
		t.Fatal(err)
	}
	ptyFile, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	terminal := &TerminalSession{
		manager: app.sessions, dbID: session.ID, agent: "claude", pty: ptyFile,
		output: byteRing{max: 1024}, poll: pollBuffer{maxBytes: 1024, maxItems: 10},
		clients: make(map[*wsClient]struct{}), exited: make(chan struct{}), readDone: make(chan struct{}),
	}
	app.sessions.mu.Lock()
	app.sessions.running[terminal.dbID] = terminal
	app.sessions.mu.Unlock()

	ticket := issueWebSocketTicket(t, server.URL, cfg.Auth.Password)
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws?ticket=" + ticket
	connection, _, err := websocket.Dial(context.Background(), wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.CloseNow()
	message, _ := json.Marshal(wsMessage{Type: "subscribe", SessionID: terminal.dbID})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := connection.Write(ctx, websocket.MessageText, message); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		terminal.mu.Lock()
		subscribed := len(terminal.clients) == 1
		terminal.mu.Unlock()
		if subscribed {
			break
		}
		time.Sleep(time.Millisecond)
	}
	terminal.finalize(7)
	_, payload, err := connection.Read(ctx)
	if err != nil {
		t.Fatalf("read exit event: %v", err)
	}
	if !strings.Contains(string(payload), `"type":"exit"`) || !strings.Contains(string(payload), `"exitCode":7`) {
		t.Fatalf("unexpected exit payload: %s", payload)
	}
	if app.sessions.IsRunning(session.ID) {
		t.Fatal("finalized session remains registered")
	}
}

func issueWebSocketTicket(t *testing.T, serverURL, password string) string {
	t.Helper()
	response := requestJSON(t, serverURL, password, http.MethodPost, "/api/ws-ticket", nil)
	if response.StatusCode != http.StatusOK || response.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("ticket status=%d cache=%q", response.StatusCode, response.Header.Get("Cache-Control"))
	}
	var result struct {
		Ticket string `json:"ticket"`
	}
	decodeBody(t, response, &result)
	if result.Ticket == "" {
		t.Fatal("empty WebSocket ticket")
	}
	return result.Ticket
}
