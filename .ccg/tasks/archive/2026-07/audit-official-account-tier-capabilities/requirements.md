# Requirements

- Determine whether the official Grok CLI exposes or internally labels account tiers.
- Identify whether the CLI carries an explicit tier-to-feature support matrix.
- Separate JWT claims, catalog routing metadata, and live endpoint entitlement.
- Recommend a reliable capability-selection mechanism for a mixed OAuth account pool.
- Determine whether the official CLI declares additional models for SuperGrok accounts and distinguish static declarations from the per-account `/models` catalog.
- Expose the official JWT tier name for each credential in the existing credential-status API.
- Prefer paid OAuth accounts over free accounts while preserving cooldown, model eligibility, affinity, and fallback behavior.
- Fall back to free accounts only when no eligible paid account remains.
- Parse authoritative paid-account billing/usage metadata when available and cool down accounts only on an explicit exhausted or blocked state.
- Evaluate, but do not enable without capability evidence, per-account native Anthropic Messages passthrough.
- Do not persist or reproduce user credentials or personal account identifiers.
