package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func TestLegacyDatabaseMigrationPreservesData(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dbPath := filepath.Join(root, "easyclaude.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
CREATE TABLE settings_profiles (id TEXT PRIMARY KEY,name TEXT NOT NULL UNIQUE,description TEXT DEFAULT '',content TEXT NOT NULL DEFAULT '{}',created_at TEXT DEFAULT (datetime('now')),updated_at TEXT DEFAULT (datetime('now')));
CREATE TABLE sessions (id TEXT PRIMARY KEY,name TEXT NOT NULL,profile_id TEXT,working_dir TEXT DEFAULT '',status TEXT DEFAULT 'stopped',claude_pid INTEGER,created_at TEXT DEFAULT (datetime('now')),updated_at TEXT DEFAULT (datetime('now')));
CREATE TABLE messages (id TEXT PRIMARY KEY,session_id TEXT NOT NULL,role TEXT NOT NULL,content TEXT DEFAULT '',created_at TEXT DEFAULT (datetime('now')));
INSERT INTO settings_profiles(id,name,content) VALUES('p1','legacy-profile','{}');
INSERT INTO sessions(id,name,profile_id,working_dir,status) VALUES('s1','legacy-session','p1','/tmp','running');`)
	if err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	store, err := openStore(dbPath)
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	defer store.Close()
	profiles, err := store.listProfiles("")
	if err != nil || len(profiles) != 1 || profiles[0].Agent != "claude" {
		t.Fatalf("profiles=%#v err=%v", profiles, err)
	}
	session, err := store.getSession("s1")
	if err != nil {
		t.Fatal(err)
	}
	if session.Agent != "claude" || session.Status != "stopped" || session.SkipPermissions != 0 {
		t.Fatalf("unexpected migrated session: %#v", session)
	}
	if mode := fileMode(t, dbPath); mode != 0o600 {
		t.Fatalf("database mode=%o", mode)
	}
}

func fileMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}
