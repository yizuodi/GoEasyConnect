package main

import (
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"
)

const conversationPreviewBytes = 4096

type feedMessage struct {
	Message
	Position  int64 `json:"position"`
	Revision  int64 `json:"revision"`
	Truncated bool  `json:"truncated"`
}

type conversationFeed struct {
	Messages       []feedMessage       `json:"messages"`
	Events         []ConversationEvent `json:"events"`
	Deleted        []string            `json:"deleted"`
	MessageCursor  int64               `json:"message_cursor"`
	EventCursor    int64               `json:"event_cursor"`
	Before         int64               `json:"before"`
	HasOlder       bool                `json:"has_older"`
	HasMore        bool                `json:"has_more"`
	ToolBefore     int64               `json:"tool_before"`
	HasOlderTools  bool                `json:"has_older_tools"`
	Running        bool                `json:"running"`
	SessionRunning bool                `json:"session_running"`
}

func (s *Store) initializeConversationFeed() error {
	transaction, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer transaction.Rollback()
	_, err = transaction.Exec(`
CREATE TABLE IF NOT EXISTS conversation_changes (
 seq INTEGER PRIMARY KEY AUTOINCREMENT,
 session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
 message_id TEXT NOT NULL,
 deleted INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_conversation_changes_session ON conversation_changes(session_id,seq);
CREATE INDEX IF NOT EXISTS idx_conversation_changes_message ON conversation_changes(message_id,seq);
CREATE TABLE IF NOT EXISTS conversation_revision_clock (id INTEGER PRIMARY KEY CHECK(id=1),seq INTEGER NOT NULL);
INSERT OR IGNORE INTO conversation_revision_clock VALUES(1,0);
CREATE TABLE IF NOT EXISTS conversation_revisions (
 message_id TEXT PRIMARY KEY,
 session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
 seq INTEGER NOT NULL,
 deleted INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_conversation_revisions_session ON conversation_revisions(session_id,seq);
INSERT OR IGNORE INTO conversation_revisions(message_id,session_id,seq,deleted)
 SELECT changes.message_id,changes.session_id,changes.seq,changes.deleted FROM conversation_changes changes
 WHERE changes.seq=(SELECT MAX(latest.seq) FROM conversation_changes latest WHERE latest.message_id=changes.message_id);
UPDATE conversation_revision_clock SET seq=MAX(seq,COALESCE((SELECT MAX(seq) FROM conversation_revisions),0)) WHERE id=1;
CREATE INDEX IF NOT EXISTS idx_messages_channel ON messages(session_id,channel);
CREATE INDEX IF NOT EXISTS idx_conversation_events_turn ON conversation_events(session_id,turn_id,event_type,seq);
CREATE INDEX IF NOT EXISTS idx_conversation_events_display ON conversation_events(session_id,seq) WHERE event_type IN ('tool.started','tool.completed','file.change','turn.failed');
DROP TRIGGER IF EXISTS conversation_message_insert;
DROP TRIGGER IF EXISTS conversation_message_update;
DROP TRIGGER IF EXISTS conversation_message_delete;
CREATE TRIGGER conversation_message_insert AFTER INSERT ON messages
WHEN NEW.channel='conversation' BEGIN
 UPDATE conversation_revision_clock SET seq=seq+1 WHERE id=1;
 INSERT OR REPLACE INTO conversation_revisions(message_id,session_id,seq,deleted) VALUES(NEW.id,NEW.session_id,(SELECT seq FROM conversation_revision_clock WHERE id=1),0);
END;
CREATE TRIGGER conversation_message_update AFTER UPDATE OF content ON messages
WHEN NEW.channel='conversation' AND NEW.content IS NOT OLD.content BEGIN
 UPDATE conversation_revision_clock SET seq=seq+1 WHERE id=1;
 INSERT OR REPLACE INTO conversation_revisions(message_id,session_id,seq,deleted) VALUES(NEW.id,NEW.session_id,(SELECT seq FROM conversation_revision_clock WHERE id=1),0);
END;
CREATE TRIGGER conversation_message_delete AFTER DELETE ON messages
WHEN OLD.channel='conversation' BEGIN
 UPDATE conversation_revision_clock SET seq=seq+1 WHERE id=1;
 INSERT OR REPLACE INTO conversation_revisions(message_id,session_id,seq,deleted) VALUES(OLD.id,OLD.session_id,(SELECT seq FROM conversation_revision_clock WHERE id=1),1);
END;`)
	if err != nil {
		return err
	}
	return transaction.Commit()
}

func previewText(value string) string {
	if len(value) <= conversationPreviewBytes {
		return value
	}
	value = value[:conversationPreviewBytes]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

func (s *Store) conversationFeed(sessionID string, before, messageAfter, eventAfter int64, delta bool) (conversationFeed, error) {
	return s.conversationFeedPage(sessionID, before, messageAfter, eventAfter, 0, delta)
}

func (s *Store) conversationFeedPage(sessionID string, before, messageAfter, eventAfter, eventBefore int64, delta bool) (conversationFeed, error) {
	return s.conversationFeedContext(context.Background(), sessionID, before, messageAfter, eventAfter, eventBefore, delta)
}

func (s *Store) conversationFeedContext(ctx context.Context, sessionID string, before, messageAfter, eventAfter, eventBefore int64, delta bool) (conversationFeed, error) {
	feed := conversationFeed{Messages: []feedMessage{}, Events: []ConversationEvent{}, Deleted: []string{}}
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return feed, err
	}
	defer transaction.Rollback()
	if err := transaction.QueryRow(`SELECT COALESCE(MAX(seq),0) FROM conversation_revisions WHERE session_id=?`, sessionID).Scan(&feed.MessageCursor); err != nil {
		return feed, err
	}
	if err := transaction.QueryRow(`SELECT COALESCE(MAX(seq),0) FROM conversation_events WHERE session_id=?`, sessionID).Scan(&feed.EventCursor); err != nil {
		return feed, err
	}
	messageQuery := `SELECT rowid,id,session_id,role,substr(COALESCE(content,''),1,4096),created_at,length(CAST(COALESCE(content,'') AS BLOB)),COALESCE((SELECT seq FROM conversation_revisions WHERE message_id=messages.id),0) FROM messages WHERE session_id=? AND channel='conversation'`
	arguments := []any{sessionID}
	if delta {
		rows, err := transaction.Query(`SELECT seq,message_id FROM conversation_revisions WHERE session_id=? AND seq>? AND seq<=? ORDER BY seq LIMIT 100`, sessionID, messageAfter, feed.MessageCursor)
		if err != nil {
			return feed, err
		}
		ids := []string{}
		seen := map[string]bool{}
		cursor := messageAfter
		for rows.Next() {
			var id string
			if err := rows.Scan(&cursor, &id); err != nil {
				rows.Close()
				return feed, err
			}
			if !seen[id] {
				ids = append(ids, id)
				seen[id] = true
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return feed, err
		}
		rows.Close()
		feed.HasMore = cursor < feed.MessageCursor
		feed.MessageCursor = cursor
		if len(ids) == 0 {
			messageQuery += ` AND 0`
		} else {
			messageQuery += ` AND id IN (` + strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",") + `)`
			for _, id := range ids {
				arguments = append(arguments, id)
			}
		}
		feed.Deleted = ids
	} else if before > 0 {
		messageQuery += ` AND rowid<?`
		arguments = append(arguments, before)
	}
	messageQuery += ` ORDER BY rowid DESC LIMIT 50`
	if delta {
		messageQuery = strings.TrimSuffix(messageQuery, " LIMIT 50")
	}
	rows, err := transaction.Query(messageQuery, arguments...)
	if err != nil {
		return feed, err
	}
	existing := map[string]bool{}
	for rows.Next() {
		var message feedMessage
		var size int
		if err := rows.Scan(&message.Position, &message.ID, &message.SessionID, &message.Role, &message.Content, &message.CreatedAt, &size, &message.Revision); err != nil {
			rows.Close()
			return feed, err
		}
		message.Content = previewText(message.Content)
		message.Truncated = size > len(message.Content)
		feed.Messages = append(feed.Messages, message)
		existing[message.ID] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return feed, err
	}
	rows.Close()
	deleted := []string{}
	for _, id := range feed.Deleted {
		if !existing[id] {
			deleted = append(deleted, id)
		}
	}
	feed.Deleted = deleted
	for left, right := 0, len(feed.Messages)-1; left < right; left, right = left+1, right-1 {
		feed.Messages[left], feed.Messages[right] = feed.Messages[right], feed.Messages[left]
	}
	if len(feed.Messages) > 0 {
		feed.Before = feed.Messages[0].Position
		var count int
		if err := transaction.QueryRow(`SELECT EXISTS(SELECT 1 FROM messages WHERE session_id=? AND channel='conversation' AND rowid<?)`, sessionID, feed.Before).Scan(&count); err != nil {
			return feed, err
		}
		feed.HasOlder = count != 0
	}
	eventQuery := `SELECT seq,id,session_id,COALESCE(turn_id,''),COALESCE(item_id,''),event_type,
 json_object('assistant_message_id',COALESCE(json_extract(payload_json,'$.assistant_message_id'),json_extract(payload_json,'$.message_id'),
 (SELECT COALESCE(json_extract(context.payload_json,'$.assistant_message_id'),json_extract(context.payload_json,'$.message_id')) FROM conversation_events context WHERE context.session_id=conversation_events.session_id AND context.turn_id=conversation_events.turn_id AND context.event_type IN ('turn.started','assistant.message') ORDER BY context.seq DESC LIMIT 1)),
 'command',substr(json_extract(payload_json,'$.command'),1,256),'kind',substr(json_extract(payload_json,'$.kind'),1,256),
 'status',substr(json_extract(payload_json,'$.status'),1,256),'phase',json_extract(payload_json,'$.phase'),
 'message',substr(json_extract(payload_json,'$.message'),1,256),'details_available',CASE WHEN event_type IN ('tool.started','tool.completed','file.change') THEN json('true') ELSE json('false') END) AS payload_json,created_at
 FROM conversation_events WHERE session_id=? AND seq<=?`
	eventArgs := []any{sessionID, feed.EventCursor}
	if delta {
		eventQuery += ` AND seq>? AND event_type IN ('tool.started','tool.completed','file.change','turn.failed') ORDER BY seq ASC LIMIT 100`
		eventArgs = append(eventArgs, eventAfter)
	} else {
		eventQuery += ` AND event_type IN ('tool.started','tool.completed','file.change','turn.failed')`
		if eventBefore > 0 {
			eventQuery += ` AND seq<?`
			eventArgs = append(eventArgs, eventBefore)
		}
		if len(feed.Messages) == 0 {
			eventQuery += ` AND 0`
		} else {
			eventQuery = `SELECT * FROM (` + eventQuery + `) WHERE json_extract(payload_json,'$.assistant_message_id') IN (` + strings.TrimSuffix(strings.Repeat("?,", len(feed.Messages)), ",") + `)`
			for _, message := range feed.Messages {
				eventArgs = append(eventArgs, message.ID)
			}
		}
		eventQuery += ` ORDER BY seq DESC LIMIT 101`
	}
	rows, err = transaction.Query(eventQuery, eventArgs...)
	if err != nil {
		return feed, err
	}
	cursor := eventAfter
	for rows.Next() {
		var event ConversationEvent
		var payload string
		if err := rows.Scan(&event.Seq, &event.ID, &event.SessionID, &event.TurnID, &event.ItemID, &event.Type, &payload, &event.CreatedAt); err != nil {
			rows.Close()
			return feed, err
		}
		if err := json.Unmarshal([]byte(payload), &event.Payload); err != nil {
			rows.Close()
			return feed, err
		}
		feed.Events = append(feed.Events, event)
		cursor = event.Seq
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return feed, err
	}
	rows.Close()
	if !delta && len(feed.Events) > 0 {
		feed.HasOlderTools = len(feed.Events) > 100
		if feed.HasOlderTools {
			feed.Events = feed.Events[:100]
		}
		feed.ToolBefore = feed.Events[len(feed.Events)-1].Seq
	}
	if delta {
		if len(feed.Events) < 100 {
			cursor = feed.EventCursor
		}
		feed.HasMore = feed.HasMore || cursor < feed.EventCursor
		feed.EventCursor = cursor
	}
	return feed, transaction.Commit()
}

func (a *App) conversationFeedSession(w http.ResponseWriter, r *http.Request) bool {
	if !a.cfg.Experimental.ConversationMode {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Conversation mode is disabled"})
		return false
	}
	if _, err := a.store.getSession(r.PathValue("id")); err != nil {
		if isNotFound(err) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "Session not found"})
		} else {
			a.writeInternalError(w, "get conversation session", err)
		}
		return false
	}
	return true
}

func (a *App) handleConversationFeed(w http.ResponseWriter, r *http.Request) {
	if !a.conversationFeedSession(w, r) {
		return
	}
	values := r.URL.Query()
	parseCursor := func(name string) (int64, error) {
		if values.Get(name) == "" {
			return 0, nil
		}
		value, err := strconv.ParseInt(values.Get(name), 10, 64)
		if err != nil || value < 0 {
			return 0, fmt.Errorf("invalid %s cursor", name)
		}
		return value, nil
	}
	before, err := parseCursor("before")
	messageAfter, messageErr := parseCursor("message_after")
	eventAfter, eventErr := parseCursor("event_after")
	eventBefore, toolErr := parseCursor("event_before")
	if err != nil || messageErr != nil || eventErr != nil || toolErr != nil || ((before != 0 || eventBefore != 0) && values.Has("message_after")) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid conversation cursor"})
		return
	}
	feed, err := a.store.conversationFeedContext(r.Context(), r.PathValue("id"), before, messageAfter, eventAfter, eventBefore, values.Has("message_after"))
	if err != nil {
		a.writeInternalError(w, "load conversation feed", err)
		return
	}
	feed.Running = a.conversations.IsTurnRunning(r.PathValue("id"))
	feed.SessionRunning = a.conversations.IsRunning(r.PathValue("id"))
	a.writeFeedJSON(w, r, feed)
}

func (a *App) handleConversationMessageDetail(w http.ResponseWriter, r *http.Request) {
	if !a.conversationFeedSession(w, r) {
		return
	}
	var content string
	err := a.store.db.QueryRowContext(r.Context(), `SELECT COALESCE(content,'') FROM messages WHERE session_id=? AND id=? AND channel='conversation'`, r.PathValue("id"), r.PathValue("messageID")).Scan(&content)
	a.writeConversationDetail(w, r, content, err)
}

func (a *App) handleConversationEventDetail(w http.ResponseWriter, r *http.Request) {
	if !a.conversationFeedSession(w, r) {
		return
	}
	var payload string
	err := a.store.db.QueryRowContext(r.Context(), `SELECT payload_json FROM conversation_events WHERE session_id=? AND seq=?`, r.PathValue("id"), r.PathValue("seq")).Scan(&payload)
	if err != nil {
		a.writeConversationDetail(w, r, "", err)
		return
	}
	var value any
	if err := json.Unmarshal([]byte(payload), &value); err != nil {
		a.writeInternalError(w, "decode conversation detail", err)
		return
	}
	a.writeFeedJSON(w, r, map[string]any{"payload": value})
}

func (a *App) writeConversationDetail(w http.ResponseWriter, r *http.Request, content string, err error) {
	if err == sql.ErrNoRows {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Conversation detail not found"})
	} else if err != nil {
		a.writeInternalError(w, "load conversation detail", err)
	} else {
		a.writeFeedJSON(w, r, map[string]string{"content": content})
	}
}

func acceptsFeedGzip(header string) bool {
	for _, encoding := range strings.Split(header, ",") {
		parts := strings.Split(encoding, ";")
		if strings.TrimSpace(parts[0]) != "gzip" {
			continue
		}
		quality := 1.0
		for _, parameter := range parts[1:] {
			key, value, ok := strings.Cut(strings.TrimSpace(parameter), "=")
			if ok && key == "q" {
				parsed, err := strconv.ParseFloat(value, 64)
				if err != nil || parsed < 0 || parsed > 1 {
					return false
				}
				quality = parsed
			}
		}
		return quality > 0
	}
	return false
}

func (a *App) writeFeedJSON(w http.ResponseWriter, r *http.Request, value any) {
	encoded, err := json.Marshal(value)
	if err != nil {
		a.writeInternalError(w, "encode conversation feed", err)
		return
	}
	w.Header().Add("Vary", "Accept-Encoding")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if len(encoded) < 1024 || !acceptsFeedGzip(r.Header.Get("Accept-Encoding")) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(encoded)
		return
	}
	w.Header().Set("Content-Encoding", "gzip")
	w.WriteHeader(http.StatusOK)
	writer, _ := gzip.NewWriterLevel(w, gzip.BestSpeed)
	_, _ = writer.Write(encoded)
	_ = writer.Close()
}
