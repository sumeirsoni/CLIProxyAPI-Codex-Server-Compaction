# Maintenance

This repository is an unofficial fork of [upstream CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI). Maintain the compaction patch as a small, reviewable delta and revalidate the upstream beta behavior regularly.

## Upstream sync procedure

1. Fetch the upstream default branch and tags.
2. Review upstream release notes, configuration changes, executor changes, translation changes, authentication changes, and Go version changes.
3. Rebase or merge according to the fork's published policy. Resolve conflicts by preserving upstream behavior outside the compaction integration points.
4. Do not rewrite generated files manually.
5. Run formatting, tests, vet, builds, and focused race tests.
6. Review the complete diff against upstream, not only conflict resolutions.
7. Repeat the end-to-end compatibility checks below before publishing a fork release.

Pay particular attention to changes in:

- Claude-to-Codex request translation
- Codex OAuth metadata and account selection
- model alias and suffix resolution
- token counting
- request caching and session identity
- response streaming and request logging
- Codex base URL and beta headers
- config loading, normalization, and management APIs

## Beta revalidation

Remote compaction uses the Codex beta feature `remote_compaction_v2`. Treat it as unstable. Revalidate after every upstream sync and before every public release, even if compaction files had no textual conflicts.

Use disposable credentials and non-sensitive synthetic conversations. Never run validation against production conversations or include live credentials in fixtures.

Validate at least:

1. A Claude-protocol request routed to an allowlisted Codex OAuth model remains unchanged below the threshold.
2. The same route triggers a remote compaction preflight at the calculated threshold.
3. The preflight uses the selected Codex OAuth account and configured base URL.
4. The `remote_compaction_v2` beta value is present without deleting other configured beta values.
5. The returned artifact can be replayed by the same compatible model and account context.
6. A longer compatible transcript reuses the longest lineage and can create a newer generation.
7. Changed execution scope, resolved model, auth, account, base URL, or request fingerprint prevents unsafe lineage reuse.
8. Actual Anthropic Claude routes never receive OpenAI compaction artifacts.
9. OpenAI-origin requests do not activate this Claude-origin feature.
10. Claude local-compaction requests bypass server compaction.
11. API-key-only Codex credentials do not activate the OAuth-only path.
12. State database failures, tokenizer failures, malformed beta responses, and upstream errors fail open to the original request.
13. Request logging does not record bodies containing compaction items.
14. Disabled configuration produces no state access or compaction preflight.

If the beta header, endpoint, event shape, encryption semantics, token accounting, or account binding changes, disable the feature until implementation and documentation are updated.

## Model maintenance

The `models` map is an explicit exact-key allowlist. For each supported model:

- Verify the final resolved Codex key used by the executor.
- Verify the current context window from a trustworthy source or controlled test.
- Recalculate the trigger threshold after changing the context window, reserves, margin, or ratio.
- Test artifact creation and replay with the same model and account.
- Remove model entries that are retired, renamed, or no longer beta-compatible.

Do not add broad aliases or guessed model-family entries. GPT compatibility aliases remain routing aliases only.

## State compatibility and migration

State is compatibility-scoped and expires by `state-ttl`, but the on-disk schema and upstream artifact contract can still change.

Before a release that changes state behavior:

1. Back up only if policy permits, using encrypted private storage.
2. Test opening state created by the previous fork release.
3. Confirm incompatible entries are ignored rather than replayed.
4. Prefer a fresh database when artifact compatibility is uncertain.
5. Document any required state deletion in release notes without publishing local paths or database content.

## Release checklist

- Fork notice and public URL are current.
- Upstream link is current.
- Server compaction remains disabled by default.
- `config.example.yaml` uses a currently verified model example.
- Public docs contain no credentials, private IDs, personal paths, or deployment-specific details.
- CI uses the Go version from `go.mod`.
- Linux, macOS, and Windows formatting, test, vet, and build jobs pass.
- Focused Linux race tests pass.
- Markdown links and YAML parse successfully.
- Docker publishing remains disabled until a fork-owned registry is deliberately configured.
- Release tags match the fork-specific release pattern.
- Rollback has been practiced with the candidate artifact.
