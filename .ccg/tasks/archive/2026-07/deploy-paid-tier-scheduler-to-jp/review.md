# jp deployment review

Date: 2026-07-16

## Deployment

- Target: SSH host `jp`, existing Compose project `/home/ubuntu/grokcli2api/docker-compose.yml`.
- Source revision: `fa1ac0b4ab1051f66d0efa1e61cb9d23a0f3125d`.
- Image: `ghcr.io/futureppo/grokcli2api-go:latest`.
- New image ID: `sha256:eb7a0e0d476e6f54e03c273a0a9817fa941f32743fbfad6cdf0ea909541a391d`.
- Previous revision: `fc2f2cdca682aaf1f864317bc9aa0c83c2e8e891`.
- Rollback tag: `ghcr.io/futureppo/grokcli2api-go:rollback-before-fa1ac0b`.
- The existing `.env`, Microwarp container, Compose file, and `/home/ubuntu/grokcli2api/auths:/auths` bind mount were preserved.
- The compressed release artifact checksum matched before remote load.

## Verification

- The Docker build reran `go test ./...` successfully.
- Loaded image architecture is amd64 and its OCI revision label matches `fa1ac0b`.
- Compose recreated only the `grokcli2api` service with `--no-deps --force-recreate`.
- Container remained `running` with restart count 0, no OOM, and the expected image ID for more than eight minutes.
- Host-local root health returned version `0.4.0`.
- Authenticated `/v1/models` returned `grok-4.5` and `grok-composer-2.5-fast`.
- A live non-streaming `/v1/responses` smoke request completed successfully with reasoning and message output items.
- Startup and periodic logs contained no model-refresh or billing-refresh failures.
- `GROK_BILLING_REFRESH_INTERVAL` is unset remotely, so the new five-minute default is active; the deployment remained clean past the first periodic billing interval.
- The scheduler state contained no `billing_exhausted` cooldown after deployment.
- Direct external access to port 8088 is blocked by the network boundary, as intended. The canonical public endpoint `https://build.eloina.cn` returned 200 over verified TLS and HTTP/2; unauthenticated `/v1/models` returned 401, while authenticated model listing and a live Responses request both succeeded.

## Tier inventory

JWT claims were aggregated without printing tokens, subjects, emails, filenames, or account IDs:

| Official tier | Count |
| --- | ---: |
| SuperGrok | 10 |
| X Premium | 2 |
| X Premium+ | 4 |
| No tier claim / unknown | 33926 |

The current deployment has 16 credentials whose claims qualify for paid-first scheduling and the official usage surface. Older tokens without a tier claim remain in the free/unknown fallback group.

## Admin visibility

There is no dedicated tier-count endpoint. `GET /v1/admin/credentials` exposes per-credential tier fields and can be aggregated by a caller, but `jp` currently has no `GROK_ADMIN_KEY`, so administrator routes are not registered. For this pool size, a future `/v1/admin/credentials/summary` endpoint is preferable to transferring every credential record.

## Rollback

Retag `rollback-before-fa1ac0b` as `latest`, then recreate only the `grokcli2api` Compose service. The previous image remains present on `jp`.
