# Official account tier and capability review

Date: 2026-07-16

Official source baseline: `xai-org/grok-build` commit `c68e39f60462f28d9be5e683d9cbe2c57b1a5027` (local HEAD matched remote HEAD during review).

## Official tier sources

The CLI has no shared subscription enum or static tier-to-model matrix. It obtains tier information from:

1. CCP `/v1/settings` field `subscription_tier_display` (preferred display value).
2. `GET /v1/user?include=subscription` field `subscriptionTier` (live subscription check).
3. Numeric access-token JWT `tier` claim (fallback and catalog-refresh synchronization).

Current official JWT mapping:

| Claim | Key | Display | `/user` value |
| ---: | --- | --- | --- |
| 0 | `free` | Free | no qualifying subscription |
| 1 | `supergrok` | SuperGrok | `GrokPro` |
| 2 | `x_basic` | X Basic | `XBasic` |
| 3 | `x_premium` | X Premium | `XPremium` |
| 4 | `x_premium_plus` | X Premium+ | `XPremiumPlus` |
| 5 | `supergrok_heavy` | SuperGrok Heavy | `SuperGrokPro` |
| 6 | `supergrok_lite` | SuperGrok Lite | `SuperGrokLite` |

Unknown numeric tiers are preserved and fail open in the official client. Free and X Basic are the only statically restricted personal tiers for `/usage`, Imagine, video, and voice UI/tool gates. SuperGrok Heavy is recognized as the maximum tier for billing upsell presentation. Grok Build access itself is controlled by server `/settings allow_access`, not by the local name table.

## Model capability sources

The official CLI treats three catalogs as different products:

- Build coding catalog: OAuth `GET cli-chat-proxy.grok.com/v1/models`.
- Public API catalog: API-key or `api:access` `GET api.x.ai/v1/models`.
- Grok web chat modes: `POST grok.com/rest/modes`, with per-mode `available`, `requiresUpgrade`, `unavailable`, or `comingSoon` state.

A successful Build catalog fetch replaces the bundled fallback catalog entirely. The CLI refreshes the access token and `/v1/models` after a subscription upgrade, proving that the effective Build model list is server-filtered per token. There is no hard-coded SuperGrok model list.

## Client version source

The public `xai-org/grok-build` repository currently has no Git tags or GitHub Releases, so an adapter cannot reliably auto-update its client header from repository tags. The official updater uses npm dist-tags for npm installations and the `https://x.ai/cli/stable` channel pointer (with the public GCS pointer as fallback) for internal installations. A future resolver should keep explicit `GROK_CLIENT_VERSION` precedence, refresh the trusted stable pointer asynchronously, validate strict semver, cache the last known good value, and retain a compiled fallback. Requests must not block on version discovery.

## Sanitized live result

The provided credential was described as SuperGrok, but all authoritative signals identify it as X Premium+:

- JWT claim: tier 4.
- `/v1/settings`: `subscription_tier_display = X Premium+`, `allow_access = true`.
- `/v1/user?include=subscription`: `subscriptionTier = XPremiumPlus`.
- Build models: `grok-4.5`, `grok-composer-2.5-fast` (both advertise `responses`).
- Web chat modes: Auto, Fast, and Expert available; Heavy requires upgrade.
- Public API advertised ten IDs: three Grok 4.20 variants, Grok 4.3, Grok 4.5, Grok Build 0.1, two image models, and two video models.
- Native `/v1/messages`: proxy and public API both returned 200; proxy streaming returned standard Anthropic SSE events.
- Credits billing: 200 with 5% included usage, a weekly period, zero on-demand/prepaid balance, and unified billing enabled.

The public API's ten IDs do not expand the Build CLI catalog. `/models.apiBackend` is also not a complete endpoint capability list: it advertised only `responses` even though native Messages succeeded.

## Implemented adapter behavior

- Admin credential status now exposes official `subscription_tier` and `subscription_tier_display`, derived from the current access token and refreshed with token rotation.
- New, unpinned requests prefer all eligible paid-tier accounts before free/unknown accounts. Existing affinity, model eligibility, cooldowns, least-inflight selection, capacity limits, and retry exclusion remain authoritative.
- Tiers on which the official client exposes `/usage` poll `billing?format=credits` every five minutes by default; Free and X Basic are excluded.
- Billing exhaustion requires included usage at 100% and no remaining on-demand or prepaid credit. The account cools until the server period end, falling back to the configured quota cooldown only when no future end is available.
- A recovered billing snapshot clears only a `billing_exhausted` cooldown. It cannot replace or clear rate-limit, auth, model, or upstream quota cooldowns.
- Billing snapshots are redacted and in memory only; OAuth files receive no tier or billing-derived fields.

## Native Messages assessment

Direct native Messages is preferable after capability confirmation because it preserves Anthropic `top_k`, thinking, stop semantics, content blocks, usage, and SSE without a Responses translation layer. It must not be enabled for every non-free tier based on the JWT claim alone.

Recommended future design:

- Per-account capability state: `unknown`, `native_messages`, or `responses_only`, keyed to credential generation.
- Free accounts default to `responses_only`; X Premium+ may be seeded as verified only when explicitly configured or successfully observed.
- On a definitive entitlement rejection before execution, mark that account `responses_only` and retry through the existing Responses converter.
- On native success, cache capability until token/tier generation changes.
- Never cross-fallback after timeout, connection loss after write, or ambiguous 5xx because the native request may already have executed.
- Do not infer capability from `/models.apiBackend`; separately test SuperGrok, SuperGrok Lite, SuperGrok Heavy, X Basic, and X Premium credentials.

Native Messages routing was not enabled in this change because only free and X Premium+ have live evidence.

## Verification

- `go test ./...`
- `go vet ./...`
- `go test -race ./internal/auth ./internal/grok`
- `git diff --check`
- JWT and long token-value scans across the changed source, documentation, and task reports

All checks passed. No credential, email address, subject identifier, or raw billing response was persisted.
