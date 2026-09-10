# Repository Guidelines

## Project Structure & Module Organization

GoEasyConnect is a single Go application with embedded browser assets. `main.go`
and `app.go` manage startup and shutdown. `http.go`, `http_sessions.go`, and
`websocket.go` expose the API and terminal transport. `session.go` owns PTY and
agent lifecycle logic, `store.go` manages SQLite, and `profile.go` handles
Claude JSON and Codex TOML profiles. Desktop and mobile assets live in `web/`.
Tests are colocated with source as `*_test.go`.

## Build, Test, and Development Commands

- `make build` creates the static `easyconnect` binary with embedded assets.
- `make package VERSION=v0.1.0` creates Linux amd64/arm64 release archives and
  SHA-256 checksums under `dist/`.
- `make test` runs the Go test suite without CGO.
- `make vet` runs Go's static analyzer.
- `make check` runs tests, vetting, and a production-style build.
- `node --check web/app.js` checks desktop JavaScript syntax. Extract and check
  the inline script in `web/mobile.html` after mobile UI changes.
- `./easyconnect --check --config ./config.json` validates configuration and
  database integrity without starting the HTTP server.

## Coding Style & Naming Conventions

Run `gofmt` on every Go change. Follow standard Go naming: exported identifiers
use `PascalCase`, internal identifiers use `camelCase`, and errors add operation
context. Keep handlers small and grouped by resource. Frontend code uses
two-space indentation, semicolons in `web/app.js`, and kebab-case CSS classes.
Avoid unrelated formatting of vendored xterm files or the self-contained mobile
page.

## Testing Guidelines

Add focused tests beside the affected package and name them `TestBehavior`.
Cover authentication, input bounds, credential redaction, migration, and cleanup
paths for backend changes. PTY tests may skip only when `/dev/ptmx` is genuinely
unavailable. Exercise desktop and mobile views manually for UI work.

## Commit & Pull Request Guidelines

Use short imperative subjects such as `Protect WebSocket credentials`. Keep
commits focused. Pull requests should explain behavior, security/configuration
or schema impact, and verification performed. Link issues and include UI
screenshots when relevant. Never commit `config.json`, credentials, databases,
logs, agent profiles, or compiled binaries.
