package executor

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestMaybeApplyCodexServerCompactionUsesSelectedOAuthAndSkipsInjectedImageTool(t *testing.T) {
	var received []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Errorf("request path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer selected-token" {
			t.Errorf("authorization = %q", got)
		}
		if got := r.Header.Get("Chatgpt-Account-Id"); got != "account-1" {
			t.Errorf("Chatgpt-Account-Id = %q", got)
		}
		beta := r.Header.Get("X-Codex-Beta-Features")
		if !strings.Contains(beta, "existing_beta") || !strings.Contains(beta, codexRemoteCompactionBeta) {
			t.Errorf("beta features = %q", beta)
		}
		var errRead error
		received, errRead = io.ReadAll(r.Body)
		if errRead != nil {
			t.Errorf("read request: %v", errRead)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"type":"response.output_item.done","item":{"type":"compaction","encrypted_content":"opaque"}}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"status":"completed","usage":{"output_tokens":24}}}`+"\n\n")
	}))
	defer server.Close()

	cfg := &config.Config{}
	cfg.CodexServerCompaction = config.CodexServerCompactionConfig{
		Enabled:             true,
		StatePath:           filepath.Join(t.TempDir(), "state", "compaction.db"),
		TriggerRatio:        0.8,
		OutputReserveTokens: 1,
		SafetyMarginTokens:  1,
		RetainedUserTokens:  8,
		StateTTL:            "1h",
		Models:              map[string]int64{"gpt-5.4": 80},
	}
	executor := NewCodexExecutor(cfg)
	auth := &cliproxyauth.Auth{
		ID:       "auth-1",
		Provider: "codex",
		Attributes: map[string]string{
			"base_url": server.URL,
		},
		Metadata: map[string]any{
			"access_token": "selected-token",
			"account_id":   "account-1",
		},
	}
	original := []byte(`{"metadata":{"user_id":"user_session_11111111-1111-1111-1111-111111111111"},"messages":[{"role":"user","content":"current"}]}`)
	body := []byte(`{"model":"gpt-5.4","instructions":"keep","stream":true,"tools":[{"type":"image_generation","output_format":"png"}],"input":[` +
		`{"type":"message","role":"developer","content":[{"type":"input_text","text":"developer instructions"}]},` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"` + strings.Repeat("old context ", 80) + `"}]},` +
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"assistant response"}]},` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"recent user"}]},` +
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"recent assistant"}]},` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"current-secret"}]}` +
		`]}`)
	req := cliproxyexecutor.Request{Model: "gpt-5.4", Payload: original}
	opts := cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FromString("claude"),
		OriginalRequest: original,
		Headers: http.Header{
			"X-Claude-Code-Session-Id": []string{"session-1"},
			"X-Codex-Beta-Features":    []string{"existing_beta"},
		},
	}

	got := executor.maybeApplyCodexServerCompaction(context.Background(), opts.SourceFormat, auth, req, opts, "gpt-5.4", server.URL, "selected-token", body, true)
	if len(received) == 0 {
		t.Fatal("compaction preflight was not sent")
	}
	if strings.Contains(string(received), "image_generation") {
		t.Fatalf("preflight retained proxy-injected image tool: %s", received)
	}
	if !gjson.GetBytes(received, "stream").Bool() || gjson.GetBytes(received, "store").Bool() {
		t.Fatalf("preflight stream/store mismatch: %s", received)
	}
	if strings.Contains(string(received), "current-secret") {
		t.Fatalf("preflight included current pending user turn: %s", received)
	}
	if !strings.Contains(string(got), `"type":"compaction"`) {
		t.Fatalf("replay missing compaction artifact: %s", got)
	}
	if !strings.Contains(string(got), "current-secret") {
		t.Fatalf("replay missing current pending user turn: %s", got)
	}
	if !strings.Contains(string(got), "image_generation") {
		t.Fatalf("normal request did not restore proxy-injected image tool: %s", got)
	}
}

func TestMaybeApplyCodexServerCompactionFailsOpenOnStateError(t *testing.T) {
	blockedParent := filepath.Join(t.TempDir(), "not-a-directory")
	if errWrite := os.WriteFile(blockedParent, []byte("blocked"), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	cfg := &config.Config{}
	cfg.CodexServerCompaction = config.CodexServerCompactionConfig{
		Enabled:             true,
		StatePath:           filepath.Join(blockedParent, "compaction.db"),
		TriggerRatio:        0.8,
		OutputReserveTokens: 1,
		SafetyMarginTokens:  1,
		RetainedUserTokens:  8,
		StateTTL:            "1h",
		Models:              map[string]int64{"gpt-5.4": 80},
	}
	executor := NewCodexExecutor(cfg)
	auth := &cliproxyauth.Auth{
		ID:         "auth-1",
		Provider:   "codex",
		Attributes: map[string]string{"base_url": "https://example.test"},
		Metadata:   map[string]any{"access_token": "token", "account_id": "account-1"},
	}
	original := []byte(`{"metadata":{"user_id":"user_session_11111111-1111-1111-1111-111111111111"}}`)
	body := []byte(`{"model":"gpt-5.4","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"` + strings.Repeat("history ", 100) + `"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"reply"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"current"}]}]}`)
	req := cliproxyexecutor.Request{Model: "gpt-5.4", Payload: original}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude"), OriginalRequest: original, Headers: http.Header{"X-Claude-Code-Session-Id": []string{"session-1"}}}
	got := executor.maybeApplyCodexServerCompaction(context.Background(), opts.SourceFormat, auth, req, opts, "gpt-5.4", "https://example.test", "token", body, false)
	if !bytes.Equal(got, body) {
		t.Fatalf("fail-open body changed\ngot:  %s\nwant: %s", got, body)
	}
}

func TestCountCodexInputTokensCountsReasoningTextButNotCompactionCiphertext(t *testing.T) {
	encoder, errEncoder := tokenizerForCodexModel("gpt-5.4")
	if errEncoder != nil {
		t.Fatal(errEncoder)
	}
	base := []byte(`{"input":[{"type":"reasoning","summary":[],"content":[]},{"type":"compaction","encrypted_content":"short"}]}`)
	withReasoning := []byte(`{"input":[{"type":"reasoning","summary":[{"type":"summary_text","text":"reasoning summary words"}],"content":[{"type":"reasoning_text","text":"private reasoning content words"}]},{"type":"compaction","encrypted_content":"short"}]}`)
	withLargeCipher := []byte(`{"input":[{"type":"reasoning","summary":[],"content":[]},{"type":"compaction","encrypted_content":"` + strings.Repeat("ciphertext", 1000) + `"}]}`)
	baseCount, _ := countCodexInputTokens(encoder, base)
	reasoningCount, _ := countCodexInputTokens(encoder, withReasoning)
	cipherCount, _ := countCodexInputTokens(encoder, withLargeCipher)
	if reasoningCount <= baseCount {
		t.Fatalf("reasoning count = %d, base = %d", reasoningCount, baseCount)
	}
	if cipherCount != baseCount {
		t.Fatalf("ciphertext changed token count: got %d, want %d", cipherCount, baseCount)
	}
}

func TestMergeCodexBetaFeatureDeduplicates(t *testing.T) {
	headers := http.Header{"X-Codex-Beta-Features": []string{"one,remote_compaction_v2"}}
	mergeCodexBetaFeature(headers, codexRemoteCompactionBeta)
	if got := headers.Get("X-Codex-Beta-Features"); got != "one,remote_compaction_v2" {
		t.Fatalf("beta features = %q", got)
	}
}

func TestCodexRequestContainsCompaction(t *testing.T) {
	if !codexRequestContainsCompaction([]byte(`{"input":[{"type":"compaction","encrypted_content":"opaque"}]}`)) {
		t.Fatal("compaction replay was not detected")
	}
	if codexRequestContainsCompaction([]byte(`{"input":[{"type":"message","role":"user"}]}`)) {
		t.Fatal("ordinary request was detected as compaction replay")
	}
}

func TestCodexCompactionRequestLogBodyRedactsArtifact(t *testing.T) {
	body := []byte(`{"input":[{"type":"compaction","encrypted_content":"secret"},{"type":"message","content":"history"}]}`)
	if got := codexCompactionRequestLogBody(body); got != nil {
		t.Fatalf("logged compaction body = %s", got)
	}
}
