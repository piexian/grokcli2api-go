# SuperGrok OAuth Messages probe

Date: 2026-07-16

Security handling:

- Used only the user-provided short-lived access token.
- Did not use or persist the refresh token.
- Sent the access token to an in-memory test script on `jp`; no credential value, email, subject, or file path was written to this repository or emitted in results.

Credential metadata relevant to capability selection:

- OAuth tier: 4
- Scopes included `grok-cli:access` and `api:access`.
- Test model was fixed to `grok-4.5`.

Results:

- Build proxy `/v1/models`: 200; models `grok-4.5` and `grok-composer-2.5-fast`, both advertised as `responses`; no `messages` backend advertised.
- Build proxy `/v1/responses`: 200; response routed to `grok-4.5-build`.
- Build proxy `/v1/messages` non-streaming: 200; Anthropic `message` response with `thinking` and `text` blocks.
- Build proxy `/v1/messages` streaming: 200; `message_start`, content block, delta, message delta, and `message_stop` events received.
- Public `api.x.ai/v1/models`: 200.
- Public `api.x.ai/v1/messages`: 200 for `grok-4.5`.

Conclusion:

SuperGrok tier 4 OAuth supports native Anthropic Messages on both the Build proxy and the public API. This differs from the tested free OAuth, where Responses succeeded but Messages returned `personal-team-blocked:spending-limit`. Because `/v1/models` advertised only `responses` for both account classes, backend capability must be selected per account; catalog `apiBackend` alone is insufficient.
