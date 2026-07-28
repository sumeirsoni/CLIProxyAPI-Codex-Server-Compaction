package config

import "testing"

func TestParseConfigBytesCodexServerCompactionDefaults(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte("codex-server-compaction:\n  enabled: true\n  models:\n    ' gpt-5.4 ': 1000000\n    invalid: 0\n"))
	if err != nil {
		t.Fatalf("ParseConfigBytes() error = %v", err)
	}
	compaction := cfg.CodexServerCompaction
	if !compaction.Enabled {
		t.Fatal("codex server compaction was not enabled")
	}
	if compaction.StatePath != DefaultCodexServerCompactionStatePath {
		t.Fatalf("state path = %q", compaction.StatePath)
	}
	if compaction.TriggerRatio != DefaultCodexServerCompactionTriggerRatio {
		t.Fatalf("trigger ratio = %v", compaction.TriggerRatio)
	}
	if compaction.OutputReserveTokens != DefaultCodexServerCompactionOutputReserveTokens {
		t.Fatalf("output reserve = %d", compaction.OutputReserveTokens)
	}
	if compaction.SafetyMarginTokens != DefaultCodexServerCompactionSafetyMarginTokens {
		t.Fatalf("safety margin = %d", compaction.SafetyMarginTokens)
	}
	if compaction.RetainedUserTokens != DefaultCodexServerCompactionRetainedUserTokens {
		t.Fatalf("retained user tokens = %d", compaction.RetainedUserTokens)
	}
	if compaction.StateTTL != DefaultCodexServerCompactionStateTTL {
		t.Fatalf("state TTL default = %q", compaction.StateTTL)
	}
	if len(compaction.Models) != 1 || compaction.Models["gpt-5.4"] != 1000000 {
		t.Fatalf("models = %#v", compaction.Models)
	}
}

func TestParseConfigBytesCodexServerCompactionModelCollisionPrefersExactKey(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte("codex-server-compaction:\n  models:\n    ' gpt-5.4 ': 999\n    gpt-5.4: 1000\n"))
	if err != nil {
		t.Fatalf("ParseConfigBytes() error = %v", err)
	}
	if got := cfg.CodexServerCompaction.Models["gpt-5.4"]; got != 1000 {
		t.Fatalf("colliding normalized model = %d, want 1000", got)
	}
}

func TestParseConfigBytesCodexServerCompactionRemainsDisabled(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte("port: 8317\n"))
	if err != nil {
		t.Fatalf("ParseConfigBytes() error = %v", err)
	}
	if cfg.CodexServerCompaction.Enabled {
		t.Fatal("codex server compaction enabled by default")
	}
	if cfg.CodexServerCompaction.StatePath == "" || cfg.CodexServerCompaction.TriggerRatio == 0 {
		t.Fatalf("defaults were not applied: %#v", cfg.CodexServerCompaction)
	}
}
