package main

type Profile struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Agent       string `json:"agent"`
	Description string `json:"description"`
	Content     string `json:"content"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

type Session struct {
	ID                  string  `json:"id"`
	Name                string  `json:"name"`
	ProfileID           *string `json:"profile_id"`
	WorkingDir          string  `json:"working_dir"`
	Status              string  `json:"status"`
	Agent               string  `json:"agent"`
	ClaudePID           *int64  `json:"claude_pid"`
	ClaudeSessionID     string  `json:"claude_session_id"`
	CodexSessionID      string  `json:"codex_session_id"`
	RunMode             string  `json:"run_mode"`
	SkipPermissions     int     `json:"skip_permissions"`
	AutoContinueEnabled bool    `json:"auto_continue_enabled"`
	AutoContinueTotal   int     `json:"auto_continue_total"`
	AutoContinueRemain  int     `json:"auto_continue_remaining"`
	AutoContinueMinutes int     `json:"auto_continue_interval_minutes"`
	AutoContinueNextAt  string  `json:"auto_continue_next_at,omitempty"`
	CreatedAt           string  `json:"created_at"`
	UpdatedAt           string  `json:"updated_at"`
	ProfileName         *string `json:"profile_name"`
	IsRunning           bool    `json:"isRunning"`
	TerminalRunning     bool    `json:"terminal_running"`
	ConversationRunning bool    `json:"conversation_running"`
	TurnRunning         bool    `json:"turn_running"`
	RunningMode         string  `json:"running_mode,omitempty"`
}

type Message struct {
	ID        string `json:"id"`
	SessionID string `json:"session_id"`
	Role      string `json:"role"`
	Content   string `json:"content"`
	SourceID  string `json:"-"`
	CreatedAt string `json:"created_at"`
}

type ConversationEvent struct {
	Seq       int64  `json:"seq"`
	ID        string `json:"id"`
	SessionID string `json:"session_id"`
	TurnID    string `json:"turn_id,omitempty"`
	ItemID    string `json:"item_id,omitempty"`
	Type      string `json:"type"`
	Payload   any    `json:"payload"`
	CreatedAt string `json:"created_at"`
}
