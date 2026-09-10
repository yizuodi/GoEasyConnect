package main

import (
	"database/sql"
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
  skip_permissions INTEGER DEFAULT 0,
  created_at TEXT DEFAULT (datetime('now')),
  updated_at TEXT DEFAULT (datetime('now')),
  FOREIGN KEY (profile_id) REFERENCES settings_profiles(id) ON DELETE SET NULL
);
CREATE TABLE IF NOT EXISTS messages (
  id TEXT PRIMARY KEY,
  session_id TEXT NOT NULL,
  role TEXT NOT NULL,
  content TEXT DEFAULT '',
  created_at TEXT DEFAULT (datetime('now')),
  FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_messages_session ON messages(session_id, created_at);`
	if _, err := s.db.Exec(schema); err != nil {
		return fmt.Errorf("create schema: %w", err)
	}
	for _, statement := range []string{
		`ALTER TABLE sessions ADD COLUMN claude_session_id TEXT DEFAULT ''`,
		`ALTER TABLE sessions ADD COLUMN skip_permissions INTEGER DEFAULT 0`,
		`ALTER TABLE sessions ADD COLUMN agent TEXT NOT NULL DEFAULT 'claude'`,
		`ALTER TABLE sessions ADD COLUMN codex_session_id TEXT DEFAULT ''`,
		`ALTER TABLE settings_profiles ADD COLUMN agent TEXT NOT NULL DEFAULT 'claude'`,
	} {
		if _, err := s.db.Exec(statement); err != nil && !strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
			return fmt.Errorf("apply migration: %w", err)
		}
	}
	if _, err := s.db.Exec(`UPDATE sessions SET status='stopped'`); err != nil {
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

func (s *Store) listSessions() ([]Session, error) {
	rows, err := s.db.Query(`SELECT s.id,s.name,s.profile_id,COALESCE(s.working_dir,''),COALESCE(s.status,'stopped'),COALESCE(s.agent,'claude'),s.claude_pid,COALESCE(s.claude_session_id,''),COALESCE(s.codex_session_id,''),COALESCE(s.skip_permissions,0),s.created_at,s.updated_at,p.name FROM sessions s LEFT JOIN settings_profiles p ON s.profile_id=p.id ORDER BY s.updated_at DESC`)
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
	row := s.db.QueryRow(`SELECT s.id,s.name,s.profile_id,COALESCE(s.working_dir,''),COALESCE(s.status,'stopped'),COALESCE(s.agent,'claude'),s.claude_pid,COALESCE(s.claude_session_id,''),COALESCE(s.codex_session_id,''),COALESCE(s.skip_permissions,0),s.created_at,s.updated_at,p.name FROM sessions s LEFT JOIN settings_profiles p ON s.profile_id=p.id WHERE s.id=?`, id)
	err := row.Scan(&session.ID, &session.Name, &session.ProfileID, &session.WorkingDir, &session.Status, &session.Agent, &session.ClaudePID, &session.ClaudeSessionID, &session.CodexSessionID, &session.SkipPermissions, &session.CreatedAt, &session.UpdatedAt, &session.ProfileName)
	return session, err
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanSession(row rowScanner, session *Session) error {
	return row.Scan(&session.ID, &session.Name, &session.ProfileID, &session.WorkingDir, &session.Status, &session.Agent, &session.ClaudePID, &session.ClaudeSessionID, &session.CodexSessionID, &session.SkipPermissions, &session.CreatedAt, &session.UpdatedAt, &session.ProfileName)
}

func (s *Store) createSession(session Session) error {
	_, err := s.db.Exec(`INSERT INTO sessions (id,name,profile_id,working_dir,status,agent) VALUES (?,?,?,?,?,?)`, session.ID, session.Name, session.ProfileID, session.WorkingDir, "stopped", session.Agent)
	s.secureFiles()
	return err
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

func (s *Store) setAgentSessionID(id, agent, agentSessionID string) error {
	query := `UPDATE sessions SET claude_session_id=? WHERE id=?`
	if agent == "codex" {
		query = `UPDATE sessions SET codex_session_id=? WHERE id=?`
	}
	_, err := s.db.Exec(query, agentSessionID, id)
	return err
}

func (s *Store) listMessages(sessionID string, limit, offset int) ([]Message, error) {
	rows, err := s.db.Query(`SELECT id,session_id,role,COALESCE(content,''),created_at FROM messages WHERE session_id=? ORDER BY created_at ASC LIMIT ? OFFSET ?`, sessionID, limit, offset)
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
	_, err := s.db.Exec(`INSERT INTO messages (id,session_id,role,content) VALUES (?,?,?,?)`, message.ID, message.SessionID, message.Role, message.Content)
	return err
}

func (s *Store) updateMessage(id, content string) error {
	_, err := s.db.Exec(`UPDATE messages SET content=? WHERE id=?`, content, id)
	return err
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
