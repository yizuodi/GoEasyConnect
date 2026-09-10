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
	ID              string  `json:"id"`
	Name            string  `json:"name"`
	ProfileID       *string `json:"profile_id"`
	WorkingDir      string  `json:"working_dir"`
	Status          string  `json:"status"`
	Agent           string  `json:"agent"`
	ClaudePID       *int64  `json:"claude_pid"`
	ClaudeSessionID string  `json:"claude_session_id"`
	CodexSessionID  string  `json:"codex_session_id"`
	SkipPermissions int     `json:"skip_permissions"`
	CreatedAt       string  `json:"created_at"`
	UpdatedAt       string  `json:"updated_at"`
	ProfileName     *string `json:"profile_name"`
	IsRunning       bool    `json:"isRunning"`
}

type Message struct {
	ID        string `json:"id"`
	SessionID string `json:"session_id"`
	Role      string `json:"role"`
	Content   string `json:"content"`
	CreatedAt string `json:"created_at"`
}
