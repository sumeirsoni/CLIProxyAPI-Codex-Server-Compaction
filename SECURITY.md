# Security Policy

## Reporting a vulnerability

Report suspected vulnerabilities through [GitHub private vulnerability reporting](https://github.com/sumeirsoni/CLIProxyAPI-Codex-Server-Compaction/security/advisories/new).

Please include a concise description, affected version or commit, impact, and sanitized reproduction steps. Allow maintainers reasonable time to investigate and prepare a fix before public disclosure.

## Do not attach sensitive files

Do not attach or paste live configuration files, `.env` files, OAuth or auth files, access or refresh tokens, API keys, management secrets, logs, request bodies, hook payloads, or compaction state databases.

Before sharing diagnostic output:

- Remove credentials and authorization headers.
- Remove account IDs, auth IDs, session IDs, and private base URLs.
- Replace conversation content with a minimal synthetic reproduction.
- Share only the smallest sanitized excerpt needed to demonstrate the issue.

For server-compaction-specific handling guidance, see [docs/server-compaction/SECURITY.md](docs/server-compaction/SECURITY.md).

For vulnerabilities that affect the unmodified base project, reporters may also need to contact [upstream CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) through its current security channel.
