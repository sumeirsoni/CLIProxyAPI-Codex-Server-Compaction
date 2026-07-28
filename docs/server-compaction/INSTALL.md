# Installation

This fork is published at [CLIProxyAPI Codex Server Compaction](https://github.com/sumeirsoni/CLIProxyAPI-Codex-Server-Compaction). Build and deploy it as a separate service from upstream CLIProxyAPI so rollback remains straightforward.

## Build from source

Go 1.26 or newer is required. The repository's `go.mod` is the authoritative Go version.

```sh
REPOSITORY_URL="https://github.com/sumeirsoni/CLIProxyAPI-Codex-Server-Compaction.git"
SOURCE_DIR="${HOME}/src/CLIProxyAPI-Codex-Server-Compaction"
INSTALL_DIR="${HOME}/.local/lib/cliproxyapi-compaction"
BIN_DIR="${HOME}/.local/bin"

mkdir -p "$(dirname "${SOURCE_DIR}")" "${INSTALL_DIR}" "${BIN_DIR}"
git clone "${REPOSITORY_URL}" "${SOURCE_DIR}"
cd "${SOURCE_DIR}"
go test ./...
go build -o "${INSTALL_DIR}/cli-proxy-api-compaction" ./cmd/server
ln -sfn "${INSTALL_DIR}/cli-proxy-api-compaction" "${BIN_DIR}/cli-proxy-api-compaction"
```

Copy `config.example.yaml` to a private location outside the repository, set the API keys and OAuth configuration required by your deployment, and configure `codex-server-compaction` using [CONFIGURATION.md](CONFIGURATION.md). Keep `enabled: false` through the first normal proxy smoke test.

## Generic macOS LaunchAgent

LaunchAgent property lists do not expand shell variables inside `ProgramArguments`. Render the placeholders below before loading the file. Keep the rendered plist in a user-private location and do not commit it.

Variables used by this example:

```sh
LABEL="com.example.cliproxyapi-compaction"
BINARY_PATH="${HOME}/.local/lib/cliproxyapi-compaction/cli-proxy-api-compaction"
CONFIG_PATH="${HOME}/.config/cliproxyapi-compaction/config.yaml"
LOG_DIR="${HOME}/Library/Logs/cliproxyapi-compaction"
PLIST_PATH="${HOME}/Library/LaunchAgents/${LABEL}.plist"
```

Template:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>@@LABEL@@</string>
  <key>ProgramArguments</key>
  <array>
    <string>@@BINARY_PATH@@</string>
    <string>--config</string>
    <string>@@CONFIG_PATH@@</string>
  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>StandardOutPath</key>
  <string>@@LOG_DIR@@/stdout.log</string>
  <key>StandardErrorPath</key>
  <string>@@LOG_DIR@@/stderr.log</string>
  <key>ProcessType</key>
  <string>Background</string>
</dict>
</plist>
```

Create the log directory, replace every `@@...@@` placeholder with the corresponding absolute value, validate with `plutil -lint`, then load it:

```sh
mkdir -p "${LOG_DIR}" "$(dirname "${PLIST_PATH}")"
plutil -lint "${PLIST_PATH}"
launchctl bootstrap "gui/$(id -u)" "${PLIST_PATH}"
launchctl kickstart -k "gui/$(id -u)/${LABEL}"
```

To reload after a change:

```sh
launchctl bootout "gui/$(id -u)/${LABEL}" || true
launchctl bootstrap "gui/$(id -u)" "${PLIST_PATH}"
```

## Generic Linux systemd user service

A user service avoids hard-coding a system account. Define deployment variables and render them into a private environment file:

```sh
SERVICE_DIR="${HOME}/.config/systemd/user"
ENV_DIR="${HOME}/.config/cliproxyapi-compaction"
ENV_FILE="${ENV_DIR}/service.env"
SERVICE_FILE="${SERVICE_DIR}/cliproxyapi-compaction.service"
BINARY_PATH="${HOME}/.local/lib/cliproxyapi-compaction/cli-proxy-api-compaction"
CONFIG_PATH="${ENV_DIR}/config.yaml"

mkdir -p "${SERVICE_DIR}" "${ENV_DIR}"
chmod 700 "${ENV_DIR}"
```

`service.env` template:

```text
CLIPROXY_BINARY=@@BINARY_PATH@@
CLIPROXY_CONFIG=@@CONFIG_PATH@@
```

Replace both placeholders with absolute paths. systemd environment files do not perform shell variable expansion.

Service unit:

```ini
[Unit]
Description=CLIProxyAPI Codex Server Compaction
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=%h/.config/cliproxyapi-compaction/service.env
ExecStart=/bin/sh -c 'exec "$CLIPROXY_BINARY" --config "$CLIPROXY_CONFIG"'
Restart=on-failure
RestartSec=5
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=read-only
ReadWritePaths=%h/.cli-proxy-api %h/.config/cliproxyapi-compaction

[Install]
WantedBy=default.target
```

Adjust `ReadWritePaths` if `auth-dir`, `state-path`, or logs use different private directories. Then validate and start:

```sh
chmod 600 "${ENV_FILE}"
systemd-analyze --user verify "${SERVICE_FILE}"
systemctl --user daemon-reload
systemctl --user enable --now cliproxyapi-compaction.service
```

For a system service, use a dedicated unprivileged account, absolute paths, a root-owned environment file, and the narrowest practical `ReadWritePaths`.

## Claude client wrapper

A wrapper keeps proxy selection explicit and avoids changing a global shell profile. Store it outside the repository and restrict its permissions if it references an API key.

```sh
#!/bin/sh
set -eu

: "${CLIPROXY_BASE_URL:=http://127.0.0.1:8317}"
: "${CLIPROXY_API_KEY:?set CLIPROXY_API_KEY in a private environment source}"

export ANTHROPIC_BASE_URL="${CLIPROXY_BASE_URL}"
export ANTHROPIC_AUTH_TOKEN="${CLIPROXY_API_KEY}"
exec claude "$@"
```

Use a CLIProxyAPI model alias that routes to the intended Codex model, but configure `codex-server-compaction.models` with the final resolved Codex key. Test model routing while compaction is still disabled, then enable the feature and begin with a non-critical session.

## PreCompact hook guidance

Claude Code can invoke a `PreCompact` hook before its own automatic or manual local compaction. Server compaction happens in the proxy and does not require a hook. If you configure one, use it only for local observability or policy reminders.

Recommended behavior:

- Accept both automatic and manual PreCompact events.
- Read hook input from standard input only if needed.
- Avoid logging prompt content, tool results, OAuth data, headers, or full hook payloads.
- Do not copy OpenAI encrypted compaction artifacts into Claude configuration or hook output.
- Do not delete the server state database from a hook.
- Keep Claude's local compaction available as a fallback. The proxy detects local-compaction requests and skips its own server-compaction path for them.
- Make the hook fast and non-blocking so it does not interrupt the client.

A minimal hook command can emit a fixed reminder without recording session data:

```sh
printf '%s\n' 'Claude local compaction is starting; verify proxy server-compaction health separately.' >&2
```

Hook configuration formats can change between Claude Code releases. Confirm the current `PreCompact` schema in the official Claude Code documentation before deploying a settings change.

## Initial validation

1. Start with `enabled: false` and confirm normal Claude-protocol routing to the intended Codex model.
2. Confirm the selected credential is Codex OAuth, not API-key-only authentication.
3. Add only the exact final model key and its verified context window.
4. Enable the feature.
5. Use a disposable long-context session and inspect sanitized service warnings for fail-open events.
6. Confirm the state database is created with private permissions.
7. Practice the steps in [ROLLBACK.md](ROLLBACK.md).
