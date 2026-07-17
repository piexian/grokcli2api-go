# jp runtime revalidation after credential cleanup

Date: 2026-07-18 (Asia/Shanghai)

## Instance stability

- Public endpoint: `https://build.eloina.cn`.
- Deployed revision: `fa1ac0b4ab1051f66d0efa1e61cb9d23a0f3125d`.
- Image ID: `sha256:eb7a0e0d476e6f54e03c273a0a9817fa941f32743fbfad6cdf0ea909541a391d`.
- The post-cleanup container had run for approximately 18 hours with restart count 0, no OOM, exit code 0, and status `running`.
- Resource snapshot: about 1.5% CPU, 289 MiB memory, 15 processes, and 28.3 GB received / 29.3 GB sent since container creation.
- Memory rose from about 179 MiB immediately after cleanup to about 289 MiB under sustained traffic, but remained well below the pre-cleanup 492 MiB and showed no OOM/restart signal.
- Host disk remained 48% used with about 30 GB available; host available memory was about 1.7 GiB.
- The private cleanup rollback archive remained present with mode 0600 and unchanged size.

## Logs since the cleaned instance started

The container emitted 33035 structured lines:

| Level/message | Count |
| --- | ---: |
| INFO total | 31457 |
| WARN total | 1578 |
| ERROR total | 0 |
| Credential refreshed | 31140 |
| Credential model cooling | 870 |
| Credential account cooling | 700 |
| Model-catalog batches completed | 300 |
| Model-refresh batch partially failed | 7 |
| Billing-refresh batch failed | 1 |

Account cooldown warnings were almost entirely upstream rate limits (`rate_limited`: 699); one new authoritative `billing_exhausted` cooldown was added. All 870 model cooldown warnings were `model_free_quota_exhausted`.

The seven model-refresh failures occurred in several transient clusters. Later batches completed successfully, including full-success batches after the last failure. The single billing batch failure occurred at 08:43Z for four due accounts. During the most recent hour there were no model/billing batch failures, only normal rate-limit and free-quota cooldown activity.

## Credential pool

- Credential files / loaded accounts: 14892.
- New `refresh_invalid` states after cleanup: 0.
- Disabled accounts: 4512 (`chat_endpoint_denied`: 4511, `authentication_failed`: 1).
- Billing-exhausted account cooldowns: 6.
- Accounts with model-level cooldowns: 963.
- Tier claims: SuperGrok 6, X Premium+ 1, missing/unknown 14885.

All known paid credentials are currently unavailable:

| Tier/state | Count |
| --- | ---: |
| SuperGrok billing exhausted | 5 |
| X Premium+ billing exhausted | 1 |
| SuperGrok authentication failed | 1 |

The earliest paid billing cooldown currently ends at `2026-07-18T18:48:38Z`; the latest ends at `2026-07-23T06:12:16Z`. Until a cooldown clears or a valid paid credential is added, scheduling necessarily falls back to the unknown/free pool.

## API verification

- HTTPS root returned 200 with valid TLS and HTTP/2.
- Unauthenticated `/v1/models` returned 401.
- Authenticated `/v1/models` returned only `grok-4.5`, consistent with current eligible account catalogs.
- A non-streaming Responses request completed successfully with reasoning and message items.
- A streaming Responses request completed through the expected lifecycle and ended with `response.completed`.
- The container remained running with restart count 0 and no OOM after the smoke requests.

## Assessment

No deployment, process-stability, TLS, authentication-gateway, non-streaming, or SSE regression was found. The active operational constraint is account supply: all known paid accounts are cooling or disabled, while the unknown/free pool is producing frequent 429 and included-quota cooldowns under substantial traffic. Memory should continue to be trended because it has grown since startup, although the current value is not itself abnormal for this pool and traffic volume.
