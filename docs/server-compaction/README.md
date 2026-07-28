# Codex Server Compaction

Codex server compaction is an opt-in feature in the unofficial [CLIProxyAPI Codex Server Compaction fork](https://github.com/sumeirsoni/CLIProxyAPI-Codex-Server-Compaction). It gives Claude-protocol clients routed to compatible OpenAI Codex models access to OpenAI's remote compaction flow before the request reaches the configured context threshold.

This feature is disabled by default. See [Installation](INSTALL.md), [Configuration](CONFIGURATION.md), and [Security](SECURITY.md) before enabling it.

## Important compatibility boundary

OpenAI remote compaction returns an opaque encrypted artifact that only a compatible OpenAI Codex model and account context can consume. Actual Anthropic Claude models cannot consume that artifact. GPT compatibility aliases remain routing aliases only and do not make Claude models artifact-compatible.

The proxy activates this path only when all of the following are true:

- `codex-server-compaction.enabled` is `true`.
- The incoming request uses the Claude protocol.
- Routing selects the Codex executor with OAuth credentials.
- The final resolved Codex model has an exact positive entry in `codex-server-compaction.models`.
- A stable Claude Code execution scope is available.
- The request is not already a Claude local-compaction request.
- The estimated input reaches the configured threshold.

Other requests continue through the normal proxy path.

## Request flow

1. A Claude-protocol client sends a request to CLIProxyAPI.
2. Normal routing and model alias resolution select a Codex OAuth credential and final Codex model.
3. The proxy estimates input tokens for the translated Codex request.
4. Below the threshold, the request is sent unchanged apart from normal translation and routing behavior.
5. At or above the threshold, the proxy checks the local state database for the longest compatible prior compaction lineage.
6. A compatible artifact is replayed when possible. If more history must be compacted, the proxy sends a remote compaction preflight to the same Codex base URL with the same selected OAuth account.
7. The returned opaque artifact and lineage metadata are stored locally, then the compacted request is sent to Codex.
8. If compaction state, tokenization, preflight, parsing, or persistence fails, the feature fails open and sends the original request.

Compatibility is scoped by execution identity, resolved model, selected auth, Codex account, base URL, and a request fingerprint. Artifacts are not intentionally shared across incompatible scopes.

## Support matrix

| Client protocol | Routed provider/model | Server compaction | Notes |
| --- | --- | --- | --- |
| Claude protocol | Compatible Codex OAuth model with an exact configured key | Supported | Intended path for this fork |
| Claude protocol | Codex API-key-only credential | Not activated | The implementation requires Codex OAuth account metadata |
| Claude protocol | Anthropic Claude model | Not supported | Claude cannot consume OpenAI encrypted compaction artifacts |
| Claude protocol | Other providers | Not supported | No compatible remote artifact path |
| OpenAI Chat Completions | Codex model | Not activated | Feature is limited to Claude-origin requests |
| OpenAI Responses | Codex model | Not activated by this feature | Native client behavior remains separate |
| Any protocol | GPT compatibility alias routed to Claude | Not supported | Aliases affect routing only |
| Any supported path | Feature disabled or model key absent | Not activated | Normal proxy behavior continues |

## Documentation

- [Installation and service examples](INSTALL.md)
- [Configuration reference](CONFIGURATION.md)
- [Security and sensitive state](SECURITY.md)
- [Rollback](ROLLBACK.md)
- [Maintenance and upstream syncing](MAINTENANCE.md)

For the base project, see [upstream CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI).
