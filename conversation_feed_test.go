package main

import (
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestConversationRevisionsRemainBoundedAndSurviveReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "revisions.db")
	store, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.createSession(Session{ID: "revisions", Name: "revisions"}); err != nil {
		t.Fatal(err)
	}
	if err := store.createConversationMessage(Message{ID: "streamed", SessionID: "revisions", Role: "assistant", Content: "initial"}); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 2000; index++ {
		if err := store.updateMessage("streamed", fmt.Sprint(index)); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM conversation_revisions`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("revisions grew with tokens: %d, %v", count, err)
	}
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM conversation_changes`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("legacy ledger still growing: %d, %v", count, err)
	}
	initial, err := store.conversationFeed("revisions", 0, 0, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	store.Close()
	store, err = openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.updateMessage("streamed", "after restart"); err != nil {
		t.Fatal(err)
	}
	delta, err := store.conversationFeed("revisions", 0, initial.MessageCursor, initial.EventCursor, true)
	if err != nil || len(delta.Messages) != 1 || delta.Messages[0].Content != "after restart" || delta.MessageCursor <= initial.MessageCursor {
		t.Fatalf("revision cursor lost after restart: %+v, %v", delta, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.conversationFeedContext(ctx, "revisions", 0, 0, 0, 0, false); err == nil {
		t.Fatal("canceled request ignored")
	}
}

func TestConversationRevisionLedgerMigration(t *testing.T) {
	store, err := openStore(filepath.Join(t.TempDir(), "migration.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.createSession(Session{ID: "ledger", Name: "ledger"}); err != nil {
		t.Fatal(err)
	}
	_, err = store.db.Exec(`INSERT INTO conversation_changes(seq,session_id,message_id,deleted) VALUES(100,'ledger','old',0),(200,'ledger','old',1);`)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.initializeConversationFeed(); err != nil {
		t.Fatal(err)
	}
	feed, err := store.conversationFeed("ledger", 0, 0, 0, true)
	if err != nil || feed.MessageCursor != 200 || len(feed.Deleted) != 1 || feed.Deleted[0] != "old" {
		t.Fatalf("old ledger migration invalid: %+v, %v", feed, err)
	}
	if err := store.createConversationMessage(Message{ID: "old", SessionID: "ledger", Role: "user", Content: "recreated"}); err != nil {
		t.Fatal(err)
	}
	feed, err = store.conversationFeed("ledger", 0, 200, 0, true)
	if err != nil || feed.MessageCursor <= 200 || len(feed.Messages) != 1 {
		t.Fatalf("migration clock reused old cursor: %+v, %v", feed, err)
	}
}

func TestConversationRevisionCleanupWithSession(t *testing.T) {
	store, err := openStore(filepath.Join(t.TempDir(), "cleanup.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, id := range []string{"removed", "retained"} {
		if err := store.createSession(Session{ID: id, Name: id}); err != nil {
			t.Fatal(err)
		}
		if err := store.createConversationMessage(Message{ID: id, SessionID: id, Role: "assistant", Content: "saved reply"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.deleteSession("removed"); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM conversation_revisions WHERE session_id='removed'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("session revision cleanup failed: %d, %v", count, err)
	}
	feed, err := store.conversationFeed("retained", 0, 0, 0, false)
	if err != nil || len(feed.Messages) != 1 || feed.Messages[0].Content != "saved reply" {
		t.Fatalf("another session was affected: %+v, %v", feed, err)
	}
}

func TestConversationFeedCompression(t *testing.T) {
	app := testApp(t, testConfig(t))
	for _, test := range []struct {
		header     string
		compressed bool
	}{
		{"gzip, deflate", true}, {"gzip;q=0", false}, {"gzip;q=0.5", true},
		{"br", false}, {"gzip;q=invalid", false}, {"gzip;q=2", false},
	} {
		t.Run(test.header, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/", nil)
			request.Header.Set("Accept-Encoding", test.header)
			response := httptest.NewRecorder()
			value := map[string]string{"content": strings.Repeat("compressed reply", 1000)}
			app.writeFeedJSON(response, request, value)
			var reader io.Reader = response.Body
			if (response.Header().Get("Content-Encoding") == "gzip") != test.compressed {
				t.Fatal("incorrect compression negotiation")
			}
			if test.compressed {
				compressed, err := gzip.NewReader(response.Body)
				if err != nil {
					t.Fatal(err)
				}
				defer compressed.Close()
				reader = compressed
				if response.Body.Len() > 1024 {
					t.Fatal("compressed response unexpectedly large")
				}
			}
			var decoded map[string]string
			if err := json.NewDecoder(reader).Decode(&decoded); err != nil || decoded["content"] != value["content"] {
				t.Fatalf("compression changed content: %v", err)
			}
		})
	}
}

func TestConversationFeedLegacyMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = database.Exec(`CREATE TABLE sessions(id TEXT PRIMARY KEY,name TEXT NOT NULL,profile_id TEXT,working_dir TEXT,status TEXT,claude_pid INTEGER,created_at TEXT,updated_at TEXT);
CREATE TABLE messages(id TEXT PRIMARY KEY,session_id TEXT,role TEXT,content TEXT,channel TEXT,created_at TEXT);
INSERT INTO sessions(id,name) VALUES('legacy','legacy');
INSERT INTO messages VALUES('old-message','legacy','assistant','saved old reply','conversation','2026-01-01');`)
	if err != nil {
		t.Fatal(err)
	}
	database.Close()
	store, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	feed, err := store.conversationFeed("legacy", 0, 0, 0, false)
	if err != nil || len(feed.Messages) != 1 || feed.Messages[0].Content != "saved old reply" {
		t.Fatalf("legacy snapshot lost: %+v, %v", feed, err)
	}
	if err := store.updateMessage("old-message", "updated old reply"); err != nil {
		t.Fatal(err)
	}
	delta, err := store.conversationFeed("legacy", 0, feed.MessageCursor, feed.EventCursor, true)
	if err != nil || len(delta.Messages) != 1 || delta.Messages[0].Content != "updated old reply" {
		t.Fatalf("legacy update lost: %+v, %v", delta, err)
	}
}

func TestConversationFeedCursorDrainsBoundedPages(t *testing.T) {
	app := testApp(t, testConfig(t))
	if err := app.store.createSession(Session{ID: "paged", Name: "paged"}); err != nil {
		t.Fatal(err)
	}
	initial, err := app.store.conversationFeed("paged", 0, 0, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 220; index++ {
		id := fmt.Sprintf("paged-%03d", index)
		if err := app.store.createConversationMessage(Message{ID: id, SessionID: "paged", Role: "assistant", Content: "new message"}); err != nil {
			t.Fatal(err)
		}
		emitConversationEvent(app, "paged", "tool.completed", "turn", id, map[string]any{"assistant_message_id": id, "output": "tool detail"})
	}
	messageCount, eventCount := 0, 0
	for attempts := 0; attempts < 4; attempts++ {
		delta, err := app.store.conversationFeed("paged", 0, initial.MessageCursor, initial.EventCursor, true)
		if err != nil {
			t.Fatal(err)
		}
		if len(delta.Messages) > 100 || len(delta.Events) > 100 {
			t.Fatal("delta exceeded page bound")
		}
		messageCount += len(delta.Messages)
		eventCount += len(delta.Events)
		initial = delta
		if !delta.HasMore {
			break
		}
	}
	if messageCount != 220 || eventCount != 220 {
		t.Fatalf("delta pages lost data: messages=%d events=%d", messageCount, eventCount)
	}
}

func TestConversationFeedLongHistoryAndDelta(t *testing.T) {
	app := testApp(t, testConfig(t))
	session := Session{ID: "feed-session", Name: "long history", Agent: "codex", RunMode: "conversation", CodexSessionID: "keep-resume-id"}
	if err := app.store.createSession(session); err != nil {
		t.Fatal(err)
	}
	if err := app.store.setAgentSessionID(session.ID, "codex", session.CodexSessionID); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 650; index++ {
		if err := app.store.createConversationMessage(Message{ID: fmt.Sprintf("message-%03d", index), SessionID: session.ID, Role: "assistant", Content: strings.Repeat("长会话", 2000)}); err != nil {
			t.Fatal(err)
		}
	}
	for index := 0; index < 300; index++ {
		payload, err := json.Marshal(map[string]any{"message_id": "message-649", "content": strings.Repeat("legacy repeated body", 4000)})
		if err != nil {
			t.Fatal(err)
		}
		if err := app.store.createConversationEvent(&ConversationEvent{ID: fmt.Sprintf("legacy-event-%d", index), SessionID: session.ID, Type: "assistant.message", TurnID: "turn"}, payload); err != nil {
			t.Fatal(err)
		}
	}
	emitConversationEvent(app, session.ID, "tool.completed", "turn", "tool", map[string]any{"command": "echo hello", "output": strings.Repeat("large tool output", 7000)})
	feed, err := app.store.conversationFeed(session.ID, 0, 0, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(feed.Messages) != 50 || feed.Messages[0].ID != "message-600" || feed.Messages[49].ID != "message-649" || !feed.HasOlder {
		t.Fatalf("incorrect latest page: %+v", feed)
	}
	if len(feed.Events) != 1 {
		t.Fatalf("expected only tool summary, got %d events", len(feed.Events))
	}
	payload := feed.Events[0].Payload.(map[string]any)
	if payload["assistant_message_id"] != "message-649" || payload["output"] != nil {
		t.Fatalf("unexpected summary: %+v", payload)
	}
	for _, message := range feed.Messages {
		if len(message.Content) > conversationPreviewBytes || !message.Truncated {
			t.Fatal("message preview exceeded byte budget")
		}
	}
	body, _ := json.Marshal(feed)
	if len(body) > 300*1024 {
		t.Fatalf("snapshot too large: %d bytes", len(body))
	}
	t.Logf("650 long messages and 301 events: first response %d bytes", len(body))
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("Accept-Encoding", "gzip")
	compressed := httptest.NewRecorder()
	app.writeFeedJSON(compressed, request, feed)
	t.Logf("same snapshot with gzip: %d bytes", compressed.Body.Len())
	older, err := app.store.conversationFeed(session.ID, feed.Before, 0, 0, false)
	if err != nil || len(older.Messages) != 50 || older.Messages[0].ID != "message-550" || len(older.Events) != 0 {
		t.Fatalf("older page invalid: %+v, %v", older, err)
	}
	idle, err := app.store.conversationFeed(session.ID, 0, feed.MessageCursor, feed.EventCursor, true)
	if err != nil || len(idle.Messages) != 0 || len(idle.Events) != 0 || idle.HasMore {
		t.Fatalf("idle delta invalid: %+v, %v", idle, err)
	}
	body, _ = json.Marshal(idle)
	t.Logf("unchanged poll: %d bytes", len(body))
	for index := 0; index < 250; index++ {
		if err := app.store.updateMessage("message-649", fmt.Sprintf("updated reply %d", index)); err != nil {
			t.Fatal(err)
		}
	}
	if err := app.store.deleteMessage("message-648"); err != nil {
		t.Fatal(err)
	}
	if err := app.store.createConversationMessage(Message{ID: "new-user", SessionID: session.ID, Role: "user", Content: "continue"}); err != nil {
		t.Fatal(err)
	}
	delta, err := app.store.conversationFeed(session.ID, 0, feed.MessageCursor, feed.EventCursor, true)
	if err != nil || len(delta.Messages) != 2 || delta.Messages[0].Content != "updated reply 249" || len(delta.Deleted) != 1 || delta.Deleted[0] != "message-648" || delta.HasMore {
		t.Fatalf("delta invalid: %+v, %v", delta, err)
	}
	stored, err := app.store.getSession(session.ID)
	if err != nil || stored.CodexSessionID != session.CodexSessionID {
		t.Fatal("resume ID changed")
	}
	if err := app.store.deleteSession(session.ID); err != nil {
		t.Fatal(err)
	}
}

func TestConversationFeedHTTPAndScopedDetails(t *testing.T) {
	cfg := testConfig(t)
	cfg.Experimental.ConversationMode = true
	app := testApp(t, cfg)
	for _, id := range []string{"first", "second"} {
		if err := app.store.createSession(Session{ID: id, Name: id, Agent: "claude", RunMode: "conversation"}); err != nil {
			t.Fatal(err)
		}
	}
	content := strings.Repeat("complete text", 1000)
	if err := app.store.createConversationMessage(Message{ID: "message", SessionID: "first", Role: "assistant", Content: content}); err != nil {
		t.Fatal(err)
	}
	emitConversationEvent(app, "first", "tool.completed", "turn", "item", map[string]any{"assistant_message_id": "message", "command": "ls", "output": "complete output"})
	server := httptest.NewServer(app.routes())
	defer server.Close()
	for _, route := range []string{"feed", "messages/message", "events/1"} {
		response, err := http.Get(server.URL + "/api/sessions/first/conversation/" + route)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("unauthenticated route %s: %d", route, response.StatusCode)
		}
	}
	response := requestJSON(t, server.URL, cfg.Auth.Password, http.MethodGet, "/api/sessions/first/conversation/feed", nil)
	var feed conversationFeed
	decodeBody(t, response, &feed)
	if len(feed.Messages) != 1 || !feed.Messages[0].Truncated || len(feed.Events) != 1 {
		t.Fatalf("snapshot invalid: %+v", feed)
	}
	response = requestJSON(t, server.URL, cfg.Auth.Password, http.MethodGet, "/api/sessions/first/conversation/messages/message", nil)
	var detail map[string]string
	decodeBody(t, response, &detail)
	if detail["content"] != content {
		t.Fatal("full message was not preserved")
	}
	response = requestJSON(t, server.URL, cfg.Auth.Password, http.MethodGet, "/api/sessions/first/conversation/events/1", nil)
	var eventDetail struct {
		Payload map[string]any `json:"payload"`
	}
	decodeBody(t, response, &eventDetail)
	if eventDetail.Payload["output"] != "complete output" {
		t.Fatal("tool detail missing")
	}
	for _, route := range []string{"messages/message", "events/1"} {
		response = requestJSON(t, server.URL, cfg.Auth.Password, http.MethodGet, "/api/sessions/second/conversation/"+route, nil)
		response.Body.Close()
		if response.StatusCode != http.StatusNotFound {
			t.Fatalf("cross-session detail leaked: %s", route)
		}
	}
	for _, query := range []string{"?before=-1", "?message_after=bad", "?before=1&message_after=0"} {
		response = requestJSON(t, server.URL, cfg.Auth.Password, http.MethodGet, "/api/sessions/first/conversation/feed"+query, nil)
		response.Body.Close()
		if response.StatusCode != http.StatusBadRequest {
			t.Fatalf("bad cursor accepted: %s", query)
		}
	}
}

func TestConversationFeedToolPagination(t *testing.T) {
	app := testApp(t, testConfig(t))
	if err := app.store.createSession(Session{ID: "tools", Name: "tools"}); err != nil {
		t.Fatal(err)
	}
	if err := app.store.createConversationMessage(Message{ID: "tools-message", SessionID: "tools", Role: "assistant", Content: "result"}); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 230; index++ {
		emitConversationEvent(app, "tools", "tool.completed", "turn", fmt.Sprint(index), map[string]any{"assistant_message_id": "tools-message", "command": "test", "output": "detail"})
	}
	feed, err := app.store.conversationFeed("tools", 0, 0, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	count := len(feed.Events)
	for feed.HasOlderTools {
		feed, err = app.store.conversationFeedPage("tools", 0, 0, 0, feed.ToolBefore, false)
		if err != nil {
			t.Fatal(err)
		}
		count += len(feed.Events)
	}
	if count != 230 {
		t.Fatalf("tool pagination lost events: %d", count)
	}
}

func TestConversationAssistantEventsDoNotDuplicateBody(t *testing.T) {
	app := testApp(t, testConfig(t))
	if err := app.store.createSession(Session{ID: "compact", Name: "compact"}); err != nil {
		t.Fatal(err)
	}
	emitConversationEvent(app, "compact", "assistant.message", "turn", "item", map[string]any{"message_id": "reply", "content": strings.Repeat("large reply", 1000)})
	events, err := app.store.listConversationEvents("compact", 0, 10)
	if err != nil || len(events) != 1 {
		t.Fatalf("events=%+v error=%v", events, err)
	}
	payload := events[0].Payload.(map[string]any)
	if payload["content"] != nil || payload["message_id"] != "reply" {
		t.Fatalf("assistant event duplicates text or lost ID: %+v", payload)
	}
}
