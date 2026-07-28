# Server Compaction Security

Server compaction adds persistent local state and an additional authenticated request to the selected Codex upstream. Treat the service, configuration, OAuth files, logs, and state database as sensitive.

For vulnerability reporting, follow the repository-level [security policy](../../SECURITY.md).

## Sensitive state

The state database stores opaque OpenAI compaction artifacts plus compatibility and lineage metadata. Metadata includes values derived from execution scope, resolved model, selected auth, Codex account, base URL, request structure, token estimates, and timestamps.

The implementation does not intentionally store plaintext OAuth access tokens in the compaction database. However, the database can still reveal operational metadata and contains artifacts tied to authenticated model use. Do not publish, attach, or commit it.

Recommended controls:

- Keep `state-path` on local storage owned by the service account.
- Restrict the parent directory to the service account, normally mode `0700`.
- Restrict the database to the service account, normally mode `0600`.
- Do not place state in a synchronized public folder or repository.
- Encrypt disks and backups according to the sensitivity of proxied conversations.
- Use separate state paths for deployments that should not share lineage.
- Securely delete retired copies according to local policy and storage capabilities.

## OAuth and configuration

Codex remote compaction is sent with the same selected Codex OAuth context used for the routed request. OAuth files, refresh tokens, access tokens, API keys, management secrets, proxy credentials, and config files must remain private.

- Never commit live `config.yaml`, `.env`, auth directories, OAuth files, or service environment files.
- Do not pass credentials directly on command lines where process listings or shell history can expose them.
- Restrict credential files to the service account.
- Use a dedicated service account where practical.
- Review third-party backup, crash-reporting, and endpoint-management tools that may collect these files.

## Logging

The compaction request body can contain opaque artifacts and conversation-derived data. The feature suppresses normal request-body logging when a Codex compaction item is present, but operators must still configure logging conservatively.

- Keep debug logging disabled in production unless actively diagnosing a problem.
- Do not log request bodies, authorization headers, hook payloads, OAuth metadata, or state database contents.
- Restrict log file access and retention.
- Sanitize logs before sharing them in an issue or vulnerability report.
- Treat account IDs, auth IDs, base URLs with embedded credentials, and session identifiers as sensitive.

## Network trust

Remote compaction uses the selected credential's configured Codex base URL. Only configure trusted, compatible endpoints. A custom base URL receives the authenticated compaction preflight and the associated request prefix.

Use TLS, validate the destination, and review any configured HTTP or SOCKS proxy. Do not assume that an endpoint is safe merely because it implements a Codex-compatible API shape.

## Fail-open behavior

The feature is intentionally fail-open for availability. If tokenization, state loading, state persistence, preflight execution, response parsing, or related compaction work fails, the proxy logs a warning and sends the original un-compacted request through the normal route.

Consequences:

- Compaction failure does not block the request by default.
- The original request may still reach the configured upstream even when the compaction database is unavailable or corrupt.
- A request near the model limit may fail upstream because the original context is larger than expected.
- Operators requiring fail-closed policy enforcement must not rely on this feature as a data-loss-prevention or request-gating control.

Monitor sanitized fail-open warnings and investigate repeated failures. Do not include sensitive logs or state files in reports.

## Artifact compatibility

OpenAI encrypted compaction artifacts are opaque and compatibility-bound. Do not attempt to replay them across accounts, base URLs, resolved models, or unrelated sessions. Actual Anthropic Claude models cannot consume them. GPT compatibility aliases are routing aliases only.

## Backups and incident response

If credentials may have been exposed:

1. Stop the affected service.
2. Revoke or rotate exposed OAuth credentials, API keys, and management secrets.
3. Preserve only the minimum sanitized evidence required by policy.
4. Remove exposed configs, logs, state databases, archives, and service environment files from public locations and history where possible.
5. Rebuild from a trusted source and create a fresh state database.
6. Report a product vulnerability through GitHub private vulnerability reporting as described in the root security policy.
