# Rollback

Server compaction is opt-in and can be disabled without changing normal CLIProxyAPI routing.

## Fast feature rollback

1. Set `codex-server-compaction.enabled` to `false` in the active private configuration.
2. Remove the wrapper activation variable or disable the automatic `PreCompact` blocking hook so Claude Code's native fallback is available again.
3. Reload or restart the service using the deployment's normal service manager.
4. Start a new client turn and confirm requests route normally without new compaction state updates.
5. Confirm Claude Code's local compaction remains enabled as the client-side fallback.

The remaining server-compaction fields and state database are inert while the feature is disabled.

## Binary rollback

If the fork binary itself must be removed:

1. Disable server compaction and stop the fork service.
2. Restore a previously validated upstream CLIProxyAPI binary or package.
3. Use a configuration accepted by that upstream version. Remove the `codex-server-compaction` block if the target version rejects unknown fields or if configuration management requires an exact schema.
4. Start the upstream service.
5. Verify health, authentication, model routing, streaming, tool use, and a representative Claude-protocol request.
6. Update the Claude wrapper or service endpoint only if the rollback uses a different listener.

Do not overwrite the last known-good binary until rollback has been tested.

## State handling

The state database is not required after rollback. Choose one of these options:

- Retain it offline with private permissions for a short rollback window.
- Move it to an encrypted, access-controlled incident archive if required by policy.
- Delete it if lineage reuse is no longer needed and retention policy permits deletion.

Never attach the database to a public issue. Do not reuse it with a different account, base URL, or model configuration.

## Rollback verification

Confirm all of the following:

- The active process is the intended version.
- The feature is disabled or absent.
- No remote-compaction beta request is expected from the rollback version.
- Normal Claude-protocol requests routed to Codex succeed.
- Requests routed to actual Anthropic Claude models still use their normal path.
- Logs contain no repeated restart, authentication, or state-path errors.
- Local client compaction remains available.

If requests still contain compaction artifacts after rollback, stop the client session and begin a fresh session rather than attempting to send OpenAI artifacts to an incompatible model.
