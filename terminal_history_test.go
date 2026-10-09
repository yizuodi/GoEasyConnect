package main

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

func TestTerminalHistoryBytePagination(t *testing.T) {
	app := testApp(t, testConfig(t))
	if err := app.store.createSession(Session{ID: "history", Name: "history"}); err != nil {
		t.Fatal(err)
	}
	expected := ""
	for i := 0; i < 3; i++ {
		content := fmt.Sprint(i) + strings.Repeat("中文output\x1b[31m\n", 20000)
		expected += content
		if err := app.store.createMessage(Message{ID: fmt.Sprint(i), SessionID: "history", Role: "assistant", Content: content}); err != nil {
			t.Fatal(err)
		}
	}
	if err := app.store.createConversationMessage(Message{ID: "not-terminal", SessionID: "history", Role: "assistant", Content: "must not appear"}); err != nil {
		t.Fatal(err)
	}
	before, end := int64(0), int64(0)
	actual := ""
	for pages := 0; ; pages++ {
		if pages > 100 {
			t.Fatal("cursor did not terminate")
		}
		page, err := app.store.terminalHistory(context.Background(), "history", before, end)
		if err != nil {
			t.Fatal(err)
		}
		part := ""
		for _, chunk := range page.Chunks {
			if !utf8.ValidString(chunk.Content) {
				t.Fatal("UTF-8 split across pages")
			}
			part += chunk.Content
		}
		if len(part) > terminalPreviewBytes {
			t.Fatalf("byte cap exceeded: %d", len(part))
		}
		actual = part + actual
		if !page.HasOlder {
			break
		}
		if page.Before == before && page.End == end {
			t.Fatal("cursor stalled")
		}
		before, end = page.Before, page.End
	}
	if actual != expected {
		t.Fatalf("history omitted or duplicated data: got %d, expected %d", len(actual), len(expected))
	}
}

func TestTerminalHistoryLongSessionRemainsBounded(t *testing.T) {
	app := testApp(t, testConfig(t))
	if err := app.store.createSession(Session{ID: "large", Name: "large", CodexSessionID: "keep-id"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 30; i++ {
		if err := app.store.createMessage(Message{ID: fmt.Sprint(i), SessionID: "large", Role: "assistant", Content: strings.Repeat("\x1b[31moutput\r\n", 150000)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := app.store.createMessage(Message{ID: "newest", SessionID: "large", Role: "user", Content: "latest input"}); err != nil {
		t.Fatal(err)
	}
	page, err := app.store.terminalHistory(context.Background(), "large", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Chunks) != 2 || page.Chunks[1].Content != "latest input" || !page.HasOlder || !page.Truncated {
		t.Fatalf("not latest bounded history: %+v", page)
	}
	encoded, _ := json.Marshal(page)
	if len(encoded) > 1024*1024 {
		t.Fatalf("response too large: %d", len(encoded))
	}
	t.Logf("58.5 MB terminal fixture: initial JSON %d bytes", len(encoded))
	var count int
	if err := app.store.db.QueryRow("SELECT COUNT(*) FROM messages WHERE session_id='large'").Scan(&count); err != nil || count != 31 {
		t.Fatal("history altered", count, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := app.store.terminalHistory(ctx, "large", 0, 0); err == nil {
		t.Fatal("canceled query accepted")
	}
}

func TestTerminalHistoryHTTPAuthenticationAndBounds(t *testing.T) {
	app := testApp(t, testConfig(t))
	server := httptest.NewServer(app.routes())
	defer server.Close()
	if err := app.store.createSession(Session{ID: "mine", Name: "mine"}); err != nil {
		t.Fatal(err)
	}
	if err := app.store.createMessage(Message{ID: "secret", SessionID: "mine", Role: "assistant", Content: "mine only"}); err != nil {
		t.Fatal(err)
	}
	if err := app.store.createSession(Session{ID: "other", Name: "other"}); err != nil {
		t.Fatal(err)
	}
	if err := app.store.createMessage(Message{ID: "other-message", SessionID: "other", Role: "assistant", Content: "other private content"}); err != nil {
		t.Fatal(err)
	}
	var otherPosition int64
	if err := app.store.db.QueryRow(`SELECT rowid FROM messages WHERE id='other-message'`).Scan(&otherPosition); err != nil {
		t.Fatal(err)
	}
	page, err := app.store.terminalHistory(context.Background(), "mine", otherPosition, 3)
	if err != nil || len(page.Chunks) != 1 || page.Chunks[0].Content != "mine only" {
		t.Fatalf("cursor leaked or truncated another session: %+v, %v", page, err)
	}
	for _, check := range []struct {
		path, password string
		status         int
	}{
		{"/api/sessions/mine/terminal/history", "invalid", http.StatusUnauthorized},
		{"/api/sessions/mine/terminal/history", app.cfg.Auth.Password, http.StatusOK},
		{"/api/sessions/missing/terminal/history", app.cfg.Auth.Password, http.StatusNotFound},
		{"/api/sessions/mine/terminal/history?before=-1", app.cfg.Auth.Password, http.StatusBadRequest},
		{"/api/sessions/mine/terminal/history?end=1", app.cfg.Auth.Password, http.StatusBadRequest},
		{"/api/sessions/mine/terminal/history?before=bad", app.cfg.Auth.Password, http.StatusBadRequest},
	} {
		response := requestJSON(t, server.URL, check.password, http.MethodGet, check.path, nil)
		if response.StatusCode != check.status {
			t.Errorf("%s: %d != %d", check.path, response.StatusCode, check.status)
		}
		response.Body.Close()
	}
}

func TestTerminalHistoryGzipRoundTrip(t *testing.T) {
	app := testApp(t, testConfig(t))
	server := httptest.NewServer(app.routes())
	defer server.Close()
	if err := app.store.createSession(Session{ID: "compressed", Name: "compressed"}); err != nil {
		t.Fatal(err)
	}
	if err := app.store.createMessage(Message{ID: "compressed-message", SessionID: "compressed", Role: "assistant", Content: strings.Repeat("\x1b[31mline\r\n", 50000)}); err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequest(http.MethodGet, server.URL+"/api/sessions/compressed/terminal/history", nil)
	request.Header.Set("Authorization", "Bearer "+app.cfg.Auth.Password)
	request.Header.Set("Accept-Encoding", "gzip")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.Header.Get("Content-Encoding") != "gzip" {
		t.Fatal("history response is not compressed")
	}
	encoded, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := gzip.NewReader(strings.NewReader(string(encoded)))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	var page terminalHistoryPage
	if err := json.NewDecoder(reader).Decode(&page); err != nil || len(page.Chunks) != 1 || !page.HasOlder {
		t.Fatal("invalid compressed history", err)
	}
	t.Logf("repetitive terminal fixture: compressed first page %d bytes", len(encoded))
}

func TestTerminalTailEscapeBoundaries(t *testing.T) {
	for _, check := range []struct {
		data     string
		limit    int
		expected string
	}{
		{"old中文", 5, "文"},
		{"old\x1b[12345mNEW", 6, "NEW"},
		{"old\x1b]title\x07NEW", 6, "NEW"},
		{"old\x1bPcommand\x1b\\NEW", 7, "NEW"},
		{"ok\x1b[31", 100, "ok"},
	} {
		actual := string(terminalTail([]byte(check.data), check.limit, true))
		if actual != check.expected {
			t.Errorf("%q got %q want %q", check.data, actual, check.expected)
		}
	}
	if actual := string(terminalTail([]byte("ok\x1b[31"), 100, false)); actual != "ok\x1b[31" {
		t.Fatal("live escape continuation discarded")
	}
}

func TestTerminalLiveInitialSnapshotsBoundedAndIncremental(t *testing.T) {
	terminal := &TerminalSession{output: byteRing{max: 2 * 1024 * 1024}, poll: pollBuffer{maxBytes: 1024 * 1024, maxItems: 10}, clients: make(map[*wsClient]struct{})}
	data := []byte(strings.Repeat("old\x1b[31m\r\n", 150000) + "latest")
	terminal.output.Append(data)
	terminal.poll.Append(data)
	client := &wsClient{send: make(chan wsOutbound, 64), done: make(chan struct{})}
	terminal.addClient(client)
	payload := <-client.send
	var snapshot struct {
		Data     string `json:"data"`
		Snapshot bool   `json:"snapshot"`
	}
	if err := json.Unmarshal(payload.payload, &snapshot); err != nil {
		t.Fatal(err)
	}
	if !snapshot.Snapshot || len(snapshot.Data) > terminalPreviewBytes || !strings.HasSuffix(snapshot.Data, "latest") {
		t.Fatal("unbounded WebSocket snapshot")
	}
	output, seq := terminal.Poll(0)
	if len(output) > terminalPreviewBytes || string(output) != snapshot.Data {
		t.Fatal("poll and WebSocket snapshots differ")
	}
	terminal.output.Append([]byte("new"))
	terminal.poll.Append([]byte("new"))
	output, next := terminal.Poll(seq)
	if string(output) != "new" || next <= seq {
		t.Fatal("incremental polling missed output")
	}
}

func TestTerminalSnapshotAndLiveDeltaOrdering(t *testing.T) {
	app := testApp(t, testConfig(t))
	for attempt := 0; attempt < 100; attempt++ {
		terminal := &TerminalSession{manager: app.sessions, output: byteRing{max: 1024}, poll: pollBuffer{maxBytes: 1024, maxItems: 10}, clients: make(map[*wsClient]struct{}), onboardingDone: true, agentSessionID: "known"}
		terminal.output.Append([]byte("old"))
		client := &wsClient{send: make(chan wsOutbound, 64), done: make(chan struct{})}
		var wait sync.WaitGroup
		wait.Add(2)
		go func() { defer wait.Done(); terminal.addClient(client) }()
		go func() { defer wait.Done(); terminal.consumeOutput([]byte("new")) }()
		wait.Wait()
		first := <-client.send
		var snapshot struct {
			Data     string `json:"data"`
			Snapshot bool   `json:"snapshot"`
		}
		if err := json.Unmarshal(first.payload, &snapshot); err != nil {
			t.Fatal(err)
		}
		if !snapshot.Snapshot {
			t.Fatal("live delta arrived before initial snapshot")
		}
		if snapshot.Data == "oldnew" {
			if len(client.send) != 0 {
				t.Fatal("snapshot replayed live delta twice")
			}
		} else if snapshot.Data == "old" {
			if len(client.send) != 1 {
				t.Fatal("missing live delta")
			}
			if err := json.Unmarshal((<-client.send).payload, &snapshot); err != nil || snapshot.Data != "new" {
				t.Fatal("incorrect delta", err)
			}
		} else {
			t.Fatal("incorrect snapshot", snapshot.Data)
		}
	}
}
