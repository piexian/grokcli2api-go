# Requirements

- Remove only credential files whose derived account ID is currently disabled with reason `refresh_invalid` on `jp`.
- Verify the target count before mutation and do not remove `chat_endpoint_denied`, `authentication_failed`, billing-cooled, model-cooled, or usable credentials.
- Create a rollback archive containing every targeted credential and the pre-cleanup scheduler state.
- Stop the service during file deletion and state rewrite to prevent concurrent persistence races.
- Remove matching scheduler-state entries, restart the existing Compose service, and verify the reduced pool count, container health, logs, models, and a live Responses request.
- Never print or persist credential contents, tokens, emails, subjects, or account IDs in task reports.
- Do not invoke external multi-model review unless explicitly requested by the user.
