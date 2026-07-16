# jp runtime revalidation

Date: 2026-07-17 (Asia/Shanghai)

## Deployment state

- Public endpoint: `https://build.eloina.cn`.
- Deployed revision: `fa1ac0b4ab1051f66d0efa1e61cb9d23a0f3125d`.
- Image ID: `sha256:eb7a0e0d476e6f54e03c273a0a9817fa941f32743fbfad6cdf0ea909541a391d`.
- The container had run for approximately 11 hours with restart count 0, no OOM, exit code 0, and status `running`.
- Runtime snapshot: about 0.05% CPU, 492 MiB memory, 16 processes, and 17.4 GB received / 16.8 GB sent since container creation.
- Host filesystem was 48% used with about 30 GB available; host available memory was about 1.6 GiB with 313 MiB swap in use.

## Log review

The container emitted 21,282 structured lines since deployment:

| Level/message | Count |
| --- | ---: |
| INFO total | 21004 |
| WARN total | 278 |
| ERROR total | 0 |
| Credential refreshed | 20767 |
| Credential model cooling | 260 |
| Model-catalog batches completed | 223 |
| Credential account disabled | 8 |
| Credential account cooling | 5 |
| Billing batch partially failed | 4 |
| Model refresh batch failed | 1 |

The four billing failures were partial batches between 13:34Z and 17:36Z. The single model refresh failure occurred at 19:32Z for 46 accounts; subsequent batches completed successfully, including batches through 19:59Z. In the most recent hour there were no billing/model batch failures, only expected free-model quota cooldown events.

## Pool state

- Total loaded credentials: 33942.
- Disabled accounts: 23562 (`refresh_invalid`: 19050, `chat_endpoint_denied`: 4511, `authentication_failed`: 1).
- Account-level billing cooldowns: 5, with reset timestamps between 2026-07-18 and 2026-07-23 UTC.
- Accounts with model-level free-quota cooldowns: 1050.
- Tier claims: SuperGrok 10, X Premium 2, X Premium+ 4, missing/unknown 33926.

Current known paid-tier disposition:

| Tier/state | Count |
| --- | ---: |
| SuperGrok available (`grok-4.5`) | 1 |
| SuperGrok billing exhausted | 4 |
| X Premium billing exhausted | 0 |
| X Premium+ billing exhausted | 1 |
| Paid credential refresh invalid | 9 |
| Paid credential authentication failed | 1 |

The only credential that still advertised `grok-composer-2.5-fast` was the authentication-failed SuperGrok credential. Therefore the public model catalog now containing only `grok-4.5` reflects current account eligibility rather than an image regression.

## API verification

- HTTPS root returned 200 with valid TLS and HTTP/2.
- Unauthenticated `/v1/models` returned 401.
- Authenticated `/v1/models` returned `grok-4.5`.
- A non-streaming Responses request completed successfully with reasoning and message items.
- A streaming Responses request completed with the expected event lifecycle, including `response.created`, reasoning deltas, output text, and `response.completed`.
- The container remained running with restart count 0 after both smoke requests.

## Assessment

The deployment is healthy and the new billing cooldown is active. The primary residual risk is credential-pool quality, not service stability: most stored credentials are permanently disabled, five paid accounts are cooling on authoritative billing exhaustion, and only one known paid account is currently schedulable. Billing batch warnings expose only aggregate failure counts, so future observability should retain a sanitized failure-reason breakdown without account identifiers.
