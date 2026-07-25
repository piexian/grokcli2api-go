FROM node:24-slim AS web-build
WORKDIR /src/frontend
COPY frontend/ ./
RUN corepack enable && \
    pnpm install --frozen-lockfile && \
    pnpm build

FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY webdist_*.go ./
COPY cmd ./cmd
COPY internal ./internal
COPY --from=web-build /src/frontend/dist ./frontend/dist
RUN CGO_ENABLED=0 go test -count=1 ./... && \
    CGO_ENABLED=0 go build -tags embed_web -trimpath -ldflags="-s -w" -o /out/grok2api ./cmd/grok2api

FROM alpine:3.22
RUN apk add --no-cache ca-certificates && adduser -D -H -u 10001 app
COPY --from=build /out/grok2api /usr/local/bin/grok2api
USER app
EXPOSE 8088
ENTRYPOINT ["/usr/local/bin/grok2api"]
