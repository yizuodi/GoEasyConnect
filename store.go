package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	_ "modernc.org/sqlite"
)

type Store struct {
	db   *sql.DB
	path string
}

func openStore(path string) (*Store, error) {
	if err := os.MkdirAll(filepathDir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create database directory: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	store := &Store{db: db, path: path}
	if err := store.initialize(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) initialize() error {
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA foreign_keys=ON",
		"PRAGMA busy_timeout=5000",
		"PRAGMA synchronous=NORMAL",
	} {
		if _, err := s.db.Exec(pragma); err != nil {
			return fmt.Errorf("apply %s: %w", pragma, err)
		}
	}
	schema := `
CREATE TABLE IF NOT EXISTS settings_profiles (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL UNIQUE,
  agent TEXT NOT NULL DEFAULT 'claude',
  description TEXT DEFAULT '',
  content TEXT NOT NULL DEFAULT '{}',
  created_at TEXT DEFAULT (datetime('now')),
  updated_at TEXT DEFAULT (datetime('now'))
);
CREATE TABLE IF NOT EXISTS sessions (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  profile_id TEXT,
  working_dir TEXT DEFAULT '',
  status TEXT DEFAULT 'stopped',
  agent TEXT NOT NULL DEFAULT 'claude',
  claude_pid INTEGER,
  claude_session_id TEXT DEFAULT '',
  codex_session_id TEXT DEFAULT '',
  run_mode TEXT NOT NULL DEFAULT 'terminal',
  skip_permissions INTEGER DEFAULT 0,
  auto_continue_enabled INTEGER NOT NULL DEFAULT 0,
  auto_continue_total INTEGER NOT NULL DEFAULT 0,
  auto_continue_remaining INTEGER NOT NULL DEFAULT 0,
  auto_continue_interval_minutes INTEGER NOT NULL DEFAULT 20,
  auto_continue_next_at TEXT NOT NULL DEFAULT '',
  created_at TEXT DEFAULT (datetime('now')),
  updated_at TEXT DEFAULT (datetime('now')),
  FOREIGN KEY (profile_id) REFERENCES settings_profiles(id) ON DELETE SET NULL
);
CREATE TABLE IF NOT EXISTS messages (
  id TEXT PRIMARY KEY,
  session_id TEXT NOT NULL,
  role TEXT NOT NULL,
  content TEXT DEFAULT '',
  channel TEXT NOT NULL DEFAULT 'terminal',
  source_id TEXT NOT NULL DEFAULT '',
  created_at TEXT DEFAULT (datetime('now')),
  FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_messages_session ON messages(session_id, created_at);`
	schema += `
CREATE TABLE IF NOT EXISTS conversation_events (
  seq INTEGER PRIMARY KEY AUTOINCREMENT,
  id TEXT NOT NULL UNIQUE,
  session_id TEXT NOT NULL,
  turn_id TEXT DEFAULT '',
  item_id TEXT DEFAULT '',
  event_type TEXT NOT NULL,
  payload_json TEXT NOT NULL DEFAULT '{}',
  created_at TEXT DEFAULT (datetime('now')),
  FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_conversation_events_session_seq ON conversation_events(session_id, seq);`
	if _, err := s.db.Exec(schema); err != nil {
		return fmt.Errorf("create schema: %w", err)
	}
	for _, statement := range []string{
		`ALTER TABLE sessions ADD COLUMN claude_session_id TEXT DEFAULT ''`,
		`ALTER TABLE sessions ADD COLUMN skip_permissions INTEGER DEFAULT 0`,
		`ALTER TABLE sessions ADD COLUMN agent TEXT NOT NULL DEFAULT 'claude'`,
		`ALTER TABLE sessions ADD COLUMN codex_session_id TEXT DEFAULT ''`,
		`ALTER TABLE sessions ADD COLUMN run_mode TEXT NOT NULL DEFAULT 'terminal'`,
		`ALTER TABLE sessions ADD COLUMN auto_continue_enabled INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE sessions ADD COLUMN auto_continue_total INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE sessions ADD COLUMN auto_continue_remaining INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE sessions ADD COLUMN auto_continue_interval_minutes INTEGER NOT NULL DEFAULT 20`,
		`ALTER TABLE sessions ADD COLUMN auto_continue_next_at TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE settings_profiles ADD COLUMN agent TEXT NOT NULL DEFAULT 'claude'`,
		`ALTER TABLE messages ADD COLUMN channel TEXT NOT NULL DEFAULT 'terminal'`,
		`ALTER TABLE messages ADD COLUMN source_id TEXT NOT NULL DEFAULT ''`,
	} {
		if _, err := s.db.Exec(statement); err != nil && !strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
			return fmt.Errorf("apply migration: %w", err)
		}
	}
	if _, err := s.db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_messages_conversation_source ON messages(session_id,channel,source_id) WHERE source_id<>''`); err != nil {
		return fmt.Errorf("create conversation source index: %w", err)
	}
	if _, err := s.db.Exec(`UPDATE sessions SET status='stopped',auto_continue_next_at=''`); err != nil {
		return fmt.Errorf("reset stale session status: %w", err)
	}
	s.secureFiles()
	return nil
}

func (s *Store) secureFiles() {
	for _, path := range []string{s.path, s.path + "-wal", s.path + "-shm"} {
		if _, err := os.Stat(path); err == nil {
			_ = os.Chmod(path, 0o600)
		}
	}
}

func (s *Store) Close() error {
	_, _ = s.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	err := s.db.Close()
	s.secureFiles()
	return err
}

func (s *Store) integrityCheck() error {
	var result string
	if err := s.db.QueryRow("PRAGMA integrity_check").Scan(&result); err != nil {
		return err
	}
	if result != "ok" {
		return fmt.Errorf("sqlite integrity check: %s", result)
	}
	return nil
}

func (s *Store) listProfiles(agent string) ([]Profile, error) {
	query := `SELECT id,name,COALESCE(agent,'claude'),COALESCE(description,''),content,created_at,updated_at FROM settings_profiles`
	var rows *sql.Rows
	var err error
	if agent == "" {
		rows, err = s.db.Query(query + ` ORDER BY updated_at DESC`)
	} else {
		rows, err = s.db.Query(query+` WHERE COALESCE(agent,'claude')=? ORDER BY updated_at DESC`, agent)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	profiles := make([]Profile, 0)
	for rows.Next() {
		var profile Profile
		if err := rows.Scan(&profile.ID, &profile.Name, &profile.Agent, &profile.Description, &profile.Content, &profile.CreatedAt, &profile.UpdatedAt); err != nil {
			return nil, err
		}
		profiles = append(profiles, profile)
	}
	return profiles, rows.Err()
}

func (s *Store) getProfile(id string) (Profile, error) {
	var profile Profile
	err := s.db.QueryRow(`SELECT id,name,COALESCE(agent,'claude'),COALESCE(description,''),content,created_at,updated_at FROM settings_profiles WHERE id=?`, id).
		Scan(&profile.ID, &profile.Name, &profile.Agent, &profile.Description, &profile.Content, &profile.CreatedAt, &profile.UpdatedAt)
	return profile, err
}

func (s *Store) getAgentProfile(id, agent string) (Profile, error) {
	profile, err := s.getProfile(id)
	if err == nil && profile.Agent != agent {
		return Profile{}, sql.ErrNoRows
	}
	return profile, err
}

func (s *Store) createProfile(profile Profile) error {
	_, err := s.db.Exec(`INSERT INTO settings_profiles (id,name,agent,description,content) VALUES (?,?,?,?,?)`, profile.ID, profile.Name, profile.Agent, profile.Description, profile.Content)
	s.secureFiles()
	return err
}

func (s *Store) updateProfile(id, name, description, content string) error {
	result, err := s.db.Exec(`UPDATE settings_profiles SET name=?,description=?,content=?,updated_at=datetime('now') WHERE id=?`, name, description, content, id)
	if err != nil {
		return err
	}
	return requireAffected(result)
}

func (s *Store) deleteProfile(id string) error {
	result, err := s.db.Exec(`DELETE FROM settings_profiles WHERE id=?`, id)
	if err != nil {
		return err
	}
	return requireAffected(result)
}

const sessionColumns = `s.id,s.name,s.profile_id,COALESCE(s.working_dir,''),COALESCE(s.status,'stopped'),COALESCE(s.agent,'claude'),s.claude_pid,COALESCE(s.claude_session_id,''),COALESCE(s.codex_session_id,''),COALESCE(s.run_mode,'terminal'),COALESCE(s.skip_permissions,0),COALESCE(s.auto_continue_enabled,0),COALESCE(s.auto_continue_total,0),COALESCE(s.auto_continue_remaining,0),COALESCE(s.auto_continue_interval_minutes,20),COALESCE(s.auto_continue_next_at,''),s.created_at,s.updated_at,p.name`

func (s *Store) listSessions() ([]Session, error) {
	rows, err := s.db.Query(`SELECT ` + sessionColumns + ` FROM sessions s LEFT JOIN settings_profiles p ON s.profile_id=p.id ORDER BY s.updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	sessions := make([]Session, 0)
	for rows.Next() {
		var session Session
		if err := scanSession(rows, &session); err != nil {
			return nil, err
		}
		sessions = append(sessions, session)
	}
	return sessions, rows.Err()
}

func (s *Store) getSession(id string) (Session, error) {
	var session Session
	row := s.db.QueryRow(`SELECT `+sessionColumns+` FROM sessions s LEFT JOIN settings_profiles p ON s.profile_id=p.id WHERE s.id=?`, id)
	err := scanSession(row, &session)
	return session, err
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanSession(row rowScanner, session *Session) error {
	return row.Scan(&session.ID, &session.Name, &session.ProfileID, &session.WorkingDir, &session.Status, &session.Agent, &session.ClaudePID, &session.ClaudeSessionID, &session.CodexSessionID, &session.RunMode, &session.SkipPermissions, &session.AutoContinueEnabled, &session.AutoContinueTotal, &session.AutoContinueRemain, &session.AutoContinueMinutes, &session.AutoContinueNextAt, &session.CreatedAt, &session.UpdatedAt, &session.ProfileName)
}

func (s *Store) createSession(session Session) error {
	mode := session.RunMode
	if mode == "" {
		mode = "terminal"
	}
	_, err := s.db.Exec(`INSERT INTO sessions (id,name,profile_id,working_dir,status,agent,run_mode) VALUES (?,?,?,?,?,?,?)`, session.ID, session.Name, session.ProfileID, session.WorkingDir, "stopped", session.Agent, mode)
	s.secureFiles()
	return err
}

func (s *Store) updateSession(id, name string) error {
	result, err := s.db.Exec(`UPDATE sessions SET name=?,updated_at=datetime('now') WHERE id=?`, name, id)
	if err != nil {
		return err
	}
	return requireAffected(result)
}

func (s *Store) deleteSession(id string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM messages WHERE session_id=?`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM conversation_events WHERE session_id=?`, id); err != nil {
		return err
	}
	result, err := tx.Exec(`DELETE FROM sessions WHERE id=?`, id)
	if err != nil {
		return err
	}
	if err := requireAffected(result); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) setSessionStatus(id, status string) error {
	_, err := s.db.Exec(`UPDATE sessions SET status=? WHERE id=?`, status, id)
	return err
}

func (s *Store) setSessionProfile(id string, profileID *string) error {
	_, err := s.db.Exec(`UPDATE sessions SET profile_id=?,updated_at=datetime('now') WHERE id=?`, profileID, id)
	return err
}

func (s *Store) setSkipPermissions(id string, enabled bool) error {
	value := 0
	if enabled {
		value = 1
	}
	_, err := s.db.Exec(`UPDATE sessions SET skip_permissions=? WHERE id=?`, value, id)
	return err
}

func (s *Store) setSessionRunMode(id, mode string) error {
	result, err := s.db.Exec(`UPDATE sessions SET run_mode=?,updated_at=datetime('now') WHERE id=?`, mode, id)
	if err != nil {
		return err
	}
	return requireAffected(result)
}

func (s *Store) setAutoContinue(id string, enabled bool, total, minutes int, nextAt string) error {
	value := 0
	if enabled {
		value = 1
	}
	result, err := s.db.Exec(`UPDATE sessions SET auto_continue_enabled=?,auto_continue_total=?,auto_continue_remaining=?,auto_continue_interval_minutes=?,auto_continue_next_at=?,updated_at=datetime('now') WHERE id=?`, value, total, total, minutes, nextAt, id)
	if err != nil {
		return err
	}
	return requireAffected(result)
}

func (s *Store) disableAutoContinue(id string) error {
	result, err := s.db.Exec(`UPDATE sessions SET auto_continue_enabled=0,auto_continue_next_at='',updated_at=datetime('now') WHERE id=?`, id)
	if err != nil {
		return err
	}
	return requireAffected(result)
}

func (s *Store) rescheduleAutoContinue(id, nextAt string) error {
	_, err := s.db.Exec(`UPDATE sessions SET auto_continue_next_at=? WHERE id=? AND auto_continue_enabled=1`, nextAt, id)
	return err
}

func (s *Store) recordAutoContinue(id, nextAt string) error {
	result, err := s.db.Exec(`UPDATE sessions SET auto_continue_remaining=auto_continue_remaining-1,auto_continue_enabled=CASE WHEN auto_continue_remaining<=1 THEN 0 ELSE 1 END,auto_continue_next_at=CASE WHEN auto_continue_remaining<=1 THEN '' ELSE ? END,updated_at=datetime('now') WHERE id=? AND auto_continue_enabled=1 AND auto_continue_remaining>0`, nextAt, id)
	if err != nil {
		return err
	}
	return requireAffected(result)
}

func (s *Store) setAgentSessionID(id, agent, agentSessionID string) error {
	query := `UPDATE sessions SET claude_session_id=? WHERE id=?`
	if agent == "codex" {
		query = `UPDATE sessions SET codex_session_id=? WHERE id=?`
	}
	_, err := s.db.Exec(query, agentSessionID, id)
	return err
}

func (s *Store) listMessages(sessionID string, limit, offset int) ([]Message, error) {
	return s.listMessagesByChannel(sessionID, "terminal", limit, offset)
}

func (s *Store) listMessagesByChannel(sessionID, channel string, limit, offset int) ([]Message, error) {
	rows, err := s.db.Query(`SELECT id,session_id,role,COALESCE(content,''),created_at FROM messages WHERE session_id=? AND COALESCE(channel,'terminal')=? ORDER BY created_at ASC, rowid ASC LIMIT ? OFFSET ?`, sessionID, channel, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	messages := make([]Message, 0)
	for rows.Next() {
		var message Message
		if err := rows.Scan(&message.ID, &message.SessionID, &message.Role, &message.Content, &message.CreatedAt); err != nil {
			return nil, err
		}
		messages = append(messages, message)
	}
	return messages, rows.Err()
}

func (s *Store) createMessage(message Message) error {
	_, err := s.db.Exec(`INSERT INTO messages (id,session_id,role,content,channel) VALUES (?,?,?,?,?)`, message.ID, message.SessionID, message.Role, message.Content, "terminal")
	return err
}

func (s *Store) createConversationMessage(message Message) error {
	_, err := s.db.Exec(`INSERT INTO messages (id,session_id,role,content,channel,source_id) VALUES (?,?,?,?,?,?)`, message.ID, message.SessionID, message.Role, message.Content, "conversation", message.SourceID)
	return err
}

func (s *Store) importConversationMessage(message Message) error {
	if message.SourceID == "" {
		return errors.New("conversation source id is required")
	}
	var existingID string
	err := s.db.QueryRow(`SELECT id FROM messages WHERE session_id=? AND channel='conversation' AND source_id='' AND role=? AND content=? ORDER BY rowid LIMIT 1`, message.SessionID, message.Role, message.Content).Scan(&existingID)
	if err == nil {
		_, err = s.db.Exec(`UPDATE messages SET source_id=? WHERE id=?`, message.SourceID, existingID)
		return err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	_, err = s.db.Exec(`INSERT OR IGNORE INTO messages (id,session_id,role,content,channel,source_id) VALUES (?,?,?,?,?,?)`, message.ID, message.SessionID, message.Role, message.Content, "conversation", message.SourceID)
	return err
}

// syncConversationHistory reconciles the app-server's canonical history with
// messages already saved while turns were streamed. Codex can expose a live
// item as msg_* and later return the same item as item-* on thread/resume, so
// source ID alone is not stable across those two APIs.
func (s *Store) syncConversationHistory(sessionID string, history []Message) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	rows, err := tx.Query(`SELECT id,role,COALESCE(content,''),COALESCE(source_id,'') FROM messages WHERE session_id=? AND channel='conversation' ORDER BY rowid`, sessionID)
	if err != nil {
		return err
	}
	existing := make([]Message, 0)
	for rows.Next() {
		var message Message
		if err := rows.Scan(&message.ID, &message.Role, &message.Content, &message.SourceID); err != nil {
			rows.Close()
			return err
		}
		existing = append(existing, message)
	}
	if err := rows.Close(); err != nil {
		return err
	}

	type messageGroup struct {
		history  []Message
		existing []Message
		retained int
	}
	groups := make(map[string]*messageGroup)
	groupKey := func(message Message) string { return message.Role + "\x00" + message.Content }
	for _, message := range history {
		key := groupKey(message)
		if groups[key] == nil {
			groups[key] = &messageGroup{}
		}
		groups[key].history = append(groups[key].history, message)
	}
	for _, message := range existing {
		if group := groups[groupKey(message)]; group != nil {
			group.existing = append(group.existing, message)
		}
	}

	// Remove surplus copies before assigning canonical source IDs, because a
	// newer imported duplicate may already own the source ID needed by the
	// older live row that we retain.
	for _, group := range groups {
		for index := len(group.history); index < len(group.existing); index++ {
			if _, err := tx.Exec(`DELETE FROM messages WHERE id=?`, group.existing[index].ID); err != nil {
				return err
			}
		}
	}
	for _, group := range groups {
		shared := len(group.history)
		if len(group.existing) < shared {
			shared = len(group.existing)
		}
		group.retained = shared
		for index := 0; index < shared; index++ {
			canonical := group.history[index]
			if _, err := tx.Exec(`UPDATE messages SET source_id=? WHERE id=?`, canonical.SourceID, group.existing[index].ID); err != nil {
				return err
			}
		}
	}
	seen := make(map[string]int)
	for _, message := range history {
		key := groupKey(message)
		index := seen[key]
		seen[key]++
		if index >= groups[key].retained {
			if _, err := tx.Exec(`INSERT OR IGNORE INTO messages (id,session_id,role,content,channel,source_id) VALUES (?,?,?,?,?,?)`, message.ID, sessionID, message.Role, message.Content, "conversation", message.SourceID); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func (s *Store) updateMessage(id, content string) error {
	_, err := s.db.Exec(`UPDATE messages SET content=? WHERE id=?`, content, id)
	return err
}

func (s *Store) setMessageSourceID(id, sourceID string) error {
	if id == "" || sourceID == "" {
		return nil
	}
	_, err := s.db.Exec(`UPDATE OR IGNORE messages SET source_id=? WHERE id=?`, sourceID, id)
	return err
}

func (s *Store) deleteMessage(id string) error {
	_, err := s.db.Exec(`DELETE FROM messages WHERE id=?`, id)
	return err
}

func (s *Store) createConversationEvent(event *ConversationEvent, payload []byte) error {
	result, err := s.db.Exec(`INSERT INTO conversation_events (id,session_id,turn_id,item_id,event_type,payload_json) VALUES (?,?,?,?,?,?)`, event.ID, event.SessionID, event.TurnID, event.ItemID, event.Type, string(payload))
	if err != nil {
		return err
	}
	event.Seq, err = result.LastInsertId()
	return err
}

func (s *Store) listConversationEvents(sessionID string, after int64, limit int) ([]ConversationEvent, error) {
	rows, err := s.db.Query(`SELECT seq,id,session_id,COALESCE(turn_id,''),COALESCE(item_id,''),event_type,payload_json,created_at FROM conversation_events WHERE session_id=? AND seq>? ORDER BY seq ASC LIMIT ?`, sessionID, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := make([]ConversationEvent, 0)
	for rows.Next() {
		var event ConversationEvent
		var payload []byte
		if err := rows.Scan(&event.Seq, &event.ID, &event.SessionID, &event.TurnID, &event.ItemID, &event.Type, &payload, &event.CreatedAt); err != nil {
			return nil, err
		}
		var value any
		if json.Unmarshal(payload, &value) != nil {
			value = map[string]any{}
		}
		event.Payload = value
		events = append(events, event)
	}
	return events, rows.Err()
}

func requireAffected(result sql.Result) error {
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func isUniqueConstraint(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "unique constraint")
}

func isNotFound(err error) bool {
	return errors.Is(err, sql.ErrNoRows)
}
