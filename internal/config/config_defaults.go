package config

const (
	DefaultPanelGitHubRepository = "https://github.com/router-for-me/Cli-Proxy-API-Management-Center"
	DefaultPprofAddr             = "127.0.0.1:8316"
	DefaultAuthDir               = "~/.cli-proxy-api"

	DefaultCodexServerCompactionStatePath           = "~/.cli-proxy-api/compaction/state.db"
	DefaultCodexServerCompactionTriggerRatio        = 0.70
	DefaultCodexServerCompactionOutputReserveTokens = int64(16_000)
	DefaultCodexServerCompactionSafetyMarginTokens  = int64(8_000)
	DefaultCodexServerCompactionRetainedUserTokens  = int64(20_000)
	DefaultCodexServerCompactionStateTTL            = "168h"
)
