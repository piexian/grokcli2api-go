# Contributing

Thanks for contributing to `grokcli2api-go`.

## Development

Requirements: Go 1.23 or newer.

```bash
test -z "$(gofmt -l $(git ls-files '*.go'))"
go test -count=1 ./...
go test -race -count=1 ./... # Linux
go vet ./...
go build -trimpath ./cmd/grok2api
docker build .
```

Run `gofmt` on changed Go files. Keep the production implementation on the Go standard library unless a dependency is clearly justified.

## Live smoke

The real inference smoke is disabled by default. Run it only after every offline gate above has passed, including Linux race testing and the Docker build:

```bash
GROK_LIVE_SMOKE=1 \
GROK_LIVE_SMOKE_OFFLINE_GATES=passed \
go test -count=1 -run '^TestLiveInferenceSmoke$' ./internal/grok
```

It reads `auths/live-01.json` by default; set `GROK_LIVE_SMOKE_AUTH_FILE` to use another file. The source must resolve to exactly one logical credential so initialization performs one account catalog discovery. The smoke gives the service only a temporary `0600` copy with refresh tokens, ID tokens, email fields, and stale legacy model lists removed. It refuses access tokens with less than ten minutes remaining and fails if the source credential hash or Git worktree changes. Response bodies, tokens, and account identifiers are never printed.

## Pull requests

- Keep changes focused and describe protocol compatibility implications.
- Add tests for request conversion, response conversion, streaming events, and errors as applicable.
- Never commit session tokens, API keys, authentication files, or unsanitized upstream traffic.
- Update the README when public endpoints, environment variables, or compatibility behavior changes.
