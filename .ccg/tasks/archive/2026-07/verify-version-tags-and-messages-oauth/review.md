# Version source and Messages OAuth review

## Client version

- `xai-org/grok-build` public repository currently has no Git tags and no GitHub Releases, so repository-tag polling cannot work.
- Official code compiles `xai-grok-version::VERSION` from `GROK_VERSION`, falling back to the crate package version.
- Official updater uses `npm view @xai-official/grok` for npm installs and the public `x.ai/cli/{stable,alpha}` channel pointers for internal installs.
- Live checks returned `0.2.101` from npm `latest`, npm `alpha`, `https://x.ai/cli/stable`, and the public GCS stable pointer.

Recommended resolver:

1. Explicit `GROK_CLIENT_VERSION` wins.
2. Load a cached last-known-good semver.
3. For the trusted cli-chat-proxy only, refresh `https://x.ai/cli/stable` asynchronously with the official GCS pointer as fallback.
4. Validate strict semver and a small response size; do not block startup or requests.
5. Retain a pinned compiled fallback and optionally let CI update that fallback after tests.

## Messages OAuth

Source proves only the client path:

- `ApiBackend::Messages` exists and sends `{base_url}/messages`.
- Session tokens resolve through the default Bearer auth scheme.
- The remote model parser can consume `apiBackend=messages` from `/v1/models`.
- The repository Messages tests use a mock inference server, not production OAuth.

Production free-OAuth A/B probe on 2026-07-16, with credentials kept entirely on `jp`:

- OAuth `/v1/models`: 200; one model, backend `responses`, no Messages model advertised.
- Same OAuth and `grok-4.5`, `/v1/responses`: 200; response model `grok-4.5-build-free`.
- Same OAuth and `grok-4.5`, `/v1/messages`: 403 `personal-team-blocked:spending-limit`.

Therefore the route exists, but native Messages is not available to the current free OAuth entitlement. The adapter's downstream Messages-to-upstream Responses conversion is the correct behavior for this reverse-engineered target. Paid or specially entitled OAuth remains unverified and must not be inferred from sampler source alone.
