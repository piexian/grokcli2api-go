# refresh_invalid credential cleanup

Date: 2026-07-17 (Asia/Shanghai)

## Meaning

`refresh_invalid` is assigned only after a permanent OAuth refresh failure: missing refresh metadata, an invalid token endpoint, an OAuth refresh response with HTTP 400/401, or a successful response that omits the replacement access token. These credentials are not retried by the pool and require replacement credentials to recover.

This differs from the other disabled reasons that were retained:

- `chat_endpoint_denied`: the credential may still authenticate, but the account is denied Grok Build/chat access.
- `authentication_failed`: request authentication failed and the immediate refresh recovery did not succeed.

## Safety checks

- A temporary standard-library Go utility was built for linux/amd64 and tested against a synthetic credential directory.
- The test verified exact reason matching, backup contents, and preservation of unrelated cooldown/model-cooldown state fields.
- Remote dry-run result before mutation: 19050 target states, 19050 matching files, 19050 unique accounts, 0 unmatched states, 0 duplicate files, and 0 invalid credential JSON files.
- The service was stopped before backup, deletion, and scheduler-state rewrite.

## Cleanup result

- Removed credential files: 19050.
- Removed scheduler state entries: 19050.
- Remaining credential files / loaded accounts: 14892.
- Remaining scheduler state entries: 5040.
- Remaining disabled accounts: 4512 (`chat_endpoint_denied`: 4511, `authentication_failed`: 1).
- Remaining billing-exhausted account cooldowns: 5.
- Remaining accounts with model-level cooldowns: 523.
- Post-cleanup dry run: 0 `refresh_invalid` states and 0 matching files.

## Rollback

Private server-local backup:

- Path: `/home/ubuntu/grokcli2api/backups/refresh-invalid-20260716T231404Z.tar.gz`
- Mode: backup directory 0700, archive 0600, owned by root.
- Size: 10543439 bytes.
- SHA-256: `9be29dbf77115f927bedb4d754ed7b39aa5484f5542fadacca98b6c6d2783796`
- Entries: 19051 (the pre-cleanup scheduler state plus 19050 credential files).

The archive is permission-isolated but not cryptographically encrypted. It remains only on `jp`.

## Runtime verification

- The existing image and configuration were unchanged.
- Restarted container loaded 14892 accounts and remained running with restart count 0 and no OOM.
- Memory dropped from about 492 MiB before cleanup to about 179 MiB after startup settled.
- The new instance emitted only normal startup INFO logs, with no WARN or ERROR lines during verification.
- Public HTTPS health, authenticated model listing, and a live non-streaming Responses request succeeded.
- Public model catalog remained `grok-4.5`, consistent with the remaining eligible credentials.

The old 33942-account process exceeded Compose's default graceful-stop window and logged `shutdown error="context deadline exceeded"`. The cleanup began only after Docker confirmed the container had exited; the state file parsed successfully, the backup was verified, and the restarted instance loaded the expected state. Future maintenance on a pool of this size should use a stop timeout of at least 60 seconds.
