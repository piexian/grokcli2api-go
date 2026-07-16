# Public HTTPS verification

Date: 2026-07-16

Canonical endpoint: `https://build.eloina.cn`

- Root health returned HTTP 200 and service version `0.4.0`.
- TLS verification succeeded (`ssl_verify_result=0`) and HTTP/2 was negotiated.
- Unauthenticated `GET /v1/models` returned HTTP 401.
- Authenticated `GET /v1/models` returned `grok-4.5` and `grok-composer-2.5-fast`.
- An authenticated `POST /v1/responses` request completed successfully with model `grok-4.5`, reasoning output, message output, and no error.
- No API key or response content was persisted in this task.

The earlier direct-port timeout concerned `http://43.165.167.88:8088`; it does not represent the public service path and is now documented as an expected network boundary.
