# Plan

1. Verify the current official `xai-org/grok-build` source and extract its JWT tier mapping, live subscription sources, model-catalog behavior, and client-side feature gates.
2. Probe the provided short-lived OAuth access token in memory against `/settings`, `/user`, Build `/models`, web `/rest/modes`, public `/models`, native Messages, and credits billing without persisting credentials.
3. Derive official tier key/display fields from the current access token and update them after OAuth refresh.
4. Preserve affinity and model eligibility while preferring paid accounts for new scheduling decisions, with free/unknown fallback on cooldown, unsupported models, or capacity exhaustion.
5. Poll authoritative credits metadata for tiers supported by the official usage surface and proactively cool only explicit exhaustion states until the returned period end.
6. Document the per-account native Messages design without enabling it from tier alone.
7. Run focused tests, race tests, the full Go suite, vet, formatting, diff, and secret checks.

External model orchestration is intentionally omitted per the user's standing instruction unless explicitly requested.
