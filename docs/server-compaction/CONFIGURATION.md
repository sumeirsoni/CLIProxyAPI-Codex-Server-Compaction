# Server Compaction Configuration

Server compaction is configured under the top-level `codex-server-compaction` key in `config.yaml`. It remains disabled unless `enabled: true` is set explicitly.

## Complete example

```yaml
codex-server-compaction:
  enabled: false
  state-path: "~/.cli-proxy-api/compaction/state.db"
  trigger-ratio: 0.70
  output-reserve-tokens: 16000
  safety-margin-tokens: 8000
  retained-user-tokens: 20000
  state-ttl: "168h"
  models:
    gpt-5.6-sol: 1000000
```

To enable the feature after testing, change only `enabled` to `true` and keep an explicit model allowlist.

## Field reference

### `enabled`

- Type: Boolean
- Default: `false`
- Purpose: Globally enables the server-compaction path.

If false, no compaction state is read or written and no remote compaction preflight is sent.

### `state-path`

- Type: String
- Default: `~/.cli-proxy-api/compaction/state.db`
- Purpose: Path to the local bbolt state database.

A leading `~` is expanded to the service user's home directory. The parent directory is created with owner-only permissions where supported, and the database is opened with mode `0600`. Use a path on local persistent storage. Do not place it in a public, shared, or source-controlled directory.

### `trigger-ratio`

- Type: Number greater than `0` and less than `1`
- Default: `0.70`
- Purpose: Fraction of usable context at which compaction is considered.

Values outside the valid range are normalized to the default.

The threshold is calculated as:

```text
threshold = (context window - output reserve - safety margin) * trigger ratio
```

For a 1,000,000-token model with the defaults:

```text
(1,000,000 - 16,000 - 8,000) * 0.70 = 683,200
```

The resulting trigger threshold is `683200` estimated input tokens.

### `output-reserve-tokens`

- Type: Positive integer
- Default: `16000`
- Purpose: Reserves context for model output before calculating the trigger threshold.

Zero and negative values are normalized to the default.

### `safety-margin-tokens`

- Type: Positive integer
- Default: `8000`
- Purpose: Adds headroom for tokenizer estimation differences and request growth.

Zero and negative values are normalized to the default.

### `retained-user-tokens`

- Type: Positive integer
- Default: `20000`
- Purpose: Keeps a recent user-content tail outside the compacted prefix when selecting a compaction boundary.

This is a target used while walking input items. It is not a guarantee that every request can retain exactly this number of tokens. Zero and negative values are normalized to the default.

### `state-ttl`

- Type: Positive Go duration string
- Default: `168h`
- Purpose: Expires old lineage entries during state loading and saving.

Examples include `24h`, `168h`, and `336h`. Invalid, empty, zero, and negative durations are normalized to `168h`.

### `models`

- Type: Mapping from exact resolved model key to positive context-window tokens
- Default: No models
- Purpose: Acts as both an allowlist and the context-window source.

Entries with blank keys or non-positive values are removed during normalization. Surrounding whitespace is trimmed. If duplicate keys become identical after trimming, the already exact key wins.

## Exact resolved model keys

Model lookup is an exact, case-sensitive map lookup performed after normal CLIProxyAPI routing and alias resolution and after any thinking suffix is removed. Wildcards, prefixes, family matching, and fallback matching are not supported.

Configure the final Codex model name that the executor sends upstream. For example:

```yaml
codex-server-compaction:
  enabled: true
  models:
    gpt-5.6-sol: 1000000
```

If a client requests a compatibility alias such as `claude-codex`, and routing resolves it to `gpt-5.6-sol`, the compaction key must still be `gpt-5.6-sol`. Adding `claude-codex` to this map does not convert an Anthropic Claude model into a compatible Codex model. GPT compatibility aliases remain routing aliases only.

When the resolved key is absent, the request follows the normal non-compaction path.

## Choosing values

- Start with one explicitly verified Codex model.
- Use the model's actual context window, not a client alias's advertised value.
- Keep enough output reserve for the workload's maximum expected response.
- Keep a safety margin because token counts are estimates.
- Place state on persistent local storage if lineage reuse across restarts is desired.
- Use separate state paths for intentionally isolated service instances.

Review [Security](SECURITY.md) before enabling state persistence.
