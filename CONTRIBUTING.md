# Contributing

Thanks for helping improve GoEasyConnect. Keep changes focused and preserve its
small, self-hosted, single-binary design.

## Development

Use Go 1.24 or newer. Frontend files are embedded directly from `web/`; there is
no Node.js build step.

```bash
cp config.example.json config.json
# Set a unique password and local paths before running.
CGO_ENABLED=0 go test -buildvcs=false ./...
go vet ./...
make build
./easyconnect --check --config ./config.json
```

Run `gofmt -w *.go`, `node --check web/app.js`, and the Go test suite before
submitting. Exercise changed behavior in both desktop and mobile pages. Add or
update tests for API, migration, authentication, profile, or PTY changes.

Use `make package VERSION=v0.1.0` to reproduce the Linux amd64/arm64 release
archives locally. Version tags must begin with `v`; pushing a tag publishes a
GitHub Release after tests pass.

## Pull Requests

Use a short imperative commit subject, such as `Protect WebSocket credentials`.
Describe user-visible behavior, security/configuration or schema impact, and
manual verification. Link related issues and include screenshots for UI work.
Never commit credentials, `config.json`, databases, logs, agent profiles, or
compiled binaries.
