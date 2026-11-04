# AGENTS.md

The theoretical model in the theory vault, folder theory-server/, is the source of truth for this repository.

Stack: Go, using the standard library and `modernc.org/sqlite` only (SDEV-1). Any other dependency needs a rule in the model.

## Layout

- `main.go`: the `theory-server` command. It calls `internal/theory`.
- `internal/theory/store.go`: the SQLite database: members, tokens, locks, reports.
- `internal/theory/vaults.go`: vaults as bare git repositories, the hook install, the marker.
- `internal/theory/hook.go`: the push rules, run as `theory-server hook pre-receive`.
- `internal/theory/http.go`: token checks, git through `git http-backend`, and the `/api/v1` API.
- `internal/theory/cli.go`: the admin commands and `serve`.
- `Dockerfile`, `compose.yaml`, `Caddyfile`: the image, and Caddy in front of it for HTTPS.

## Tests

Before every commit, run both, and both must pass (SDEV-2):

    go vet ./...
    go test ./...

Run a single test:

    go test ./internal/theory -run TestPushRejectsCommitsByOthers

Tests use real git repositories in temporary folders and a server on localhost. The test binary stands in for `theory-server` as the pre-receive hook (see `TestMain`). Tests never use the network beyond localhost.

## Running on a laptop

Needs Go and git. All state goes into the folder in `THEORY_DATA` (default `/data`).

    export THEORY_DATA=$PWD/data
    go run . vault create demo
    go run . member add --name "Ada Lovelace" --email ada@example.com
    go run . token create --email ada@example.com
    go build -o theory-server . && ./theory-server serve --addr 127.0.0.1:8080

Then clone `http://127.0.0.1:8080/git/demo.git`, giving the token as the password. Run `serve` from a built binary, not `go run`: git runs the server's own program as the pre-receive hook.
