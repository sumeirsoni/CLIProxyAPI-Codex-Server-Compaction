package helps

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/tidwall/gjson"
)

func TestParseCodexCompactionSSEStrictSuccess(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"type":"response.output_item.done","item":{"type":"message","content":[]}}`,
		"",
		`data: {"type":"response.output_item.done","item":{"type":"compaction","encrypted_content":"opaque"}}`,
		"",
		`data: {"type":"response.completed","response":{"status":"completed","usage":{"output_tokens":37}}}`,
		"",
	}, "\n")
	artifact, err := ParseCodexCompactionSSE(strings.NewReader(stream))
	if err != nil {
		t.Fatalf("ParseCodexCompactionSSE() error = %v", err)
	}
	if got := gjson.GetBytes(artifact.Item, "encrypted_content").String(); got != "opaque" {
		t.Fatalf("artifact encrypted_content = %q", got)
	}
	if artifact.OutputTokens != 37 {
		t.Fatalf("artifact output tokens = %d", artifact.OutputTokens)
	}
}

func TestParseCodexCompactionSSERejectsInvalidArtifactsAndTerminals(t *testing.T) {
	tests := map[string]string{
		"empty encrypted content": sseCompaction(`{"type":"compaction","encrypted_content":""}`, 3),
		"missing usage": strings.Join([]string{
			`data: {"type":"response.output_item.done","item":{"type":"compaction","encrypted_content":"opaque"}}`, "",
			`data: {"type":"response.completed","response":{"status":"completed"}}`, "",
		}, "\n"),
		"multiple compaction items": strings.Join([]string{
			`data: {"type":"response.output_item.done","item":{"type":"compaction","encrypted_content":"one"}}`, "",
			`data: {"type":"response.output_item.done","item":{"type":"compaction","encrypted_content":"two"}}`, "",
			`data: {"type":"response.completed","response":{"status":"completed","usage":{"output_tokens":3}}}`, "",
		}, "\n"),
		"incomplete": `data: {"type":"response.incomplete","response":{"status":"incomplete"}}` + "\n\n",
		"malformed":  "data: {not-json}\n\n",
	}
	for name, stream := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseCodexCompactionSSE(strings.NewReader(stream)); err == nil {
				t.Fatal("ParseCodexCompactionSSE() error = nil")
			}
		})
	}
}

func TestApplyCodexServerCompactionPersistsNumericStateAndReplays(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state", "compaction.db")
	cfg := testCodexCompactionConfig(statePath)
	body := testCodexCompactionBody("branch-a")
	var calls atomic.Int32
	compact := func(_ context.Context, preflight []byte) (CodexCompactionArtifact, error) {
		calls.Add(1)
		input := gjson.GetBytes(preflight, "input").Array()
		if len(input) == 0 || input[len(input)-1].Get("type").String() != "compaction_trigger" {
			t.Fatalf("preflight does not end with compaction_trigger: %s", preflight)
		}
		return CodexCompactionArtifact{Item: []byte(`{"type":"compaction","encrypted_content":"opaque-a"}`), OutputTokens: 30}, nil
	}

	first, err := ApplyCodexServerCompaction(context.Background(), cfg, testCodexCompactionRequest(body), compact)
	if err != nil {
		t.Fatalf("first compaction: %v", err)
	}
	assertCodexReplay(t, first, "opaque-a", "branch-a")
	second, err := ApplyCodexServerCompaction(context.Background(), cfg, testCodexCompactionRequest(body), compact)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	assertCodexReplay(t, second, "opaque-a", "branch-a")
	if calls.Load() != 1 {
		t.Fatalf("compaction calls = %d, want 1", calls.Load())
	}

	compatibility, errCompatibility := normalizeCodexCompactionCompatibility(testCodexCompactionRequest(body))
	if errCompatibility != nil {
		t.Fatal(errCompatibility)
	}
	state, errLoad := loadCodexCompactionState(statePath, compatibility, time.Hour)
	if errLoad != nil {
		t.Fatal(errLoad)
	}
	if len(state.Lineages) != 1 || state.Lineages[0].CompactedTokenEstimate != 30 {
		t.Fatalf("state = %#v", state)
	}
	raw, _ := json.Marshal(state)
	if bytes.Contains(raw, []byte("old-user")) || bytes.Contains(raw, []byte("current-user-secret")) {
		t.Fatalf("durable state contains plaintext history: %s", raw)
	}
}

func TestApplyCodexServerCompactionUsesStoredArtifactCostForNextGeneration(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state", "compaction.db")
	cfg := testCodexCompactionConfig(statePath)
	bodyV1 := testCodexCompactionBody("generation")
	requestV1 := testCodexCompactionRequest(bodyV1)
	_, errFirst := ApplyCodexServerCompaction(context.Background(), cfg, requestV1, func(context.Context, []byte) (CodexCompactionArtifact, error) {
		return CodexCompactionArtifact{Item: []byte(`{"type":"compaction","encrypted_content":"gen-1"}`), OutputTokens: 100}, nil
	})
	if errFirst != nil {
		t.Fatal(errFirst)
	}
	bodyV2 := appendCodexHistoryTurn(t, bodyV1)
	requestV2 := testCodexCompactionRequest(bodyV2)
	var calls atomic.Int32
	got, errSecond := ApplyCodexServerCompaction(context.Background(), cfg, requestV2, func(context.Context, []byte) (CodexCompactionArtifact, error) {
		calls.Add(1)
		return CodexCompactionArtifact{Item: []byte(`{"type":"compaction","encrypted_content":"gen-2"}`), OutputTokens: 25}, nil
	})
	if errSecond != nil {
		t.Fatal(errSecond)
	}
	if calls.Load() != 1 || !strings.Contains(string(got), "gen-2") {
		t.Fatalf("next generation was not compacted: calls=%d body=%s", calls.Load(), got)
	}
}

func TestApplyCodexServerCompactionSingleflightWaiterHonorsCancellation(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state", "compaction.db")
	cfg := testCodexCompactionConfig(statePath)
	request := testCodexCompactionRequest(testCodexCompactionBody("same-prefix"))
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	compact := func(context.Context, []byte) (CodexCompactionArtifact, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		return CodexCompactionArtifact{Item: []byte(`{"type":"compaction","encrypted_content":"single"}`), OutputTokens: 20}, nil
	}
	leaderDone := make(chan error, 1)
	go func() {
		_, err := ApplyCodexServerCompaction(context.Background(), cfg, request, compact)
		leaderDone <- err
	}()
	<-started
	waiterCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ApplyCodexServerCompaction(waiterCtx, cfg, request, compact); !errors.Is(err, context.Canceled) {
		t.Fatalf("waiter error = %v", err)
	}
	close(release)
	if err := <-leaderDone; err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("compaction calls = %d, want 1", calls.Load())
	}
}

func TestCompatibilityFingerprintSeparatesRootSemantics(t *testing.T) {
	left := testCodexCompactionRequest(testCodexCompactionBody("same"))
	right := left
	right.Body = []byte(strings.Replace(string(left.Body), `"instructions":"keep"`, `"instructions":"different"`, 1))
	leftCompatibility, errLeft := normalizeCodexCompactionCompatibility(left)
	rightCompatibility, errRight := normalizeCodexCompactionCompatibility(right)
	if errLeft != nil || errRight != nil {
		t.Fatalf("fingerprint errors: %v %v", errLeft, errRight)
	}
	if leftCompatibility.RequestFingerprint == rightCompatibility.RequestFingerprint {
		t.Fatal("different instructions produced the same compatibility fingerprint")
	}
	volatile := left
	volatile.Body = []byte(strings.Replace(string(left.Body), `"stream":true`, `"stream":false`, 1))
	volatileCompatibility, errVolatile := normalizeCodexCompactionCompatibility(volatile)
	if errVolatile != nil {
		t.Fatal(errVolatile)
	}
	if leftCompatibility.RequestFingerprint != volatileCompatibility.RequestFingerprint {
		t.Fatal("volatile stream field changed compatibility fingerprint")
	}
}

func TestPendingCodexUserTurnBoundaryIncludesMixedFunctionOutputs(t *testing.T) {
	items := []json.RawMessage{
		json.RawMessage(`{"type":"message","role":"user"}`),
		json.RawMessage(`{"type":"function_call","call_id":"call-1"}`),
		json.RawMessage(`{"type":"function_call_output","call_id":"call-1","output":"done"}`),
		json.RawMessage(`{"type":"message","role":"user","content":[{"type":"input_text","text":"reminder"}]}`),
	}
	if got := pendingCodexUserTurnBoundary(items); got != 1 {
		t.Fatalf("boundary = %d, want 1", got)
	}
}

func TestIsClaudeLocalCompactionRequestOnlyChecksCurrentUserMessage(t *testing.T) {
	local := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"Your task is to create a detailed summary of the conversation so far"}]}]}`)
	if !IsClaudeLocalCompactionRequest(local) {
		t.Fatal("current local compaction prompt was not detected")
	}
	resumed := []byte(`{"messages":[{"role":"user","content":"This session is being continued from a previous conversation that ran out of context"},{"role":"assistant","content":"summary"},{"role":"user","content":"Now fix the build"}]}`)
	if IsClaudeLocalCompactionRequest(resumed) {
		t.Fatal("historical resumed marker was treated as the current compaction request")
	}
}

func TestOpenCodexCompactionDBRejectsSymlinksAndUnsafePermissions(t *testing.T) {
	root := t.TempDir()
	realDir := filepath.Join(root, "real")
	if err := os.Mkdir(realDir, 0o700); err != nil {
		t.Fatal(err)
	}
	linkDir := filepath.Join(root, "link")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Fatal(err)
	}
	if _, err := openCodexCompactionDB(filepath.Join(linkDir, "state.db")); err == nil {
		t.Fatal("symlink state directory was accepted")
	}
	unsafeDir := filepath.Join(root, "unsafe")
	if err := os.Mkdir(unsafeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := openCodexCompactionDB(filepath.Join(unsafeDir, "state.db")); err == nil {
		t.Fatal("unsafe state directory permissions were accepted")
	}
	safeDir := filepath.Join(root, "safe")
	if err := os.Mkdir(safeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	unsafeFile := filepath.Join(safeDir, "state.db")
	if err := os.WriteFile(unsafeFile, []byte("not bolt"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := openCodexCompactionDB(unsafeFile); err == nil {
		t.Fatal("unsafe state file permissions were accepted")
	}
}

func TestPruneCodexCompactionLineagesBoundsAndDropsOldest(t *testing.T) {
	now := time.Now().UTC()
	lineages := make([]codexCompactionLineage, 0, codexCompactionMaxLineagesPerCompatibility+5)
	for index := 0; index < codexCompactionMaxLineagesPerCompatibility+5; index++ {
		lineages = append(lineages, codexCompactionLineage{
			Version: codexCompactionStateVersion, PrefixHashes: []string{string(rune('a' + index))}, CoveredItemCount: 1,
			CompactionItem: []byte(`{"type":"compaction","encrypted_content":"opaque"}`), UpdatedAt: now.Add(time.Duration(index) * time.Minute),
		})
	}
	got := pruneCodexCompactionLineages(lineages, 24*time.Hour, now.Add(time.Hour))
	if len(got) != codexCompactionMaxLineagesPerCompatibility {
		t.Fatalf("lineages = %d", len(got))
	}
	if !got[0].UpdatedAt.Equal(now.Add(time.Duration(len(lineages)-1) * time.Minute)) {
		t.Fatal("newest lineage was not retained first")
	}
}

func sseCompaction(item string, outputTokens int64) string {
	return strings.Join([]string{
		`data: {"type":"response.output_item.done","item":` + item + `}`, "",
		`data: {"type":"response.completed","response":{"status":"completed","usage":{"output_tokens":` + fmt.Sprint(outputTokens) + `}}}`, "",
	}, "\n")
}

func testCodexCompactionConfig(statePath string) config.CodexServerCompactionConfig {
	return config.CodexServerCompactionConfig{
		Enabled: true, StatePath: statePath, TriggerRatio: 0.8, OutputReserveTokens: 0,
		SafetyMarginTokens: 0, RetainedUserTokens: 5, StateTTL: "1h", Models: map[string]int64{"gpt-test": 150},
	}
}

func testCodexCompactionRequest(body []byte) CodexServerCompactionRequest {
	return CodexServerCompactionRequest{
		Body: body, ExecutionScope: "claude:session:agent:main", Model: "gpt-test", AuthID: "auth-1",
		AccountID: "account-1", BaseURL: "https://example.test/codex", ContextWindow: 150,
		CountTokens: func(payload []byte) (int64, error) {
			var tokens int64
			for _, item := range gjson.GetBytes(payload, "input").Array() {
				if item.Get("type").String() != "compaction" && item.Get("type").String() != "compaction_trigger" {
					tokens += 20
				}
			}
			return tokens, nil
		},
		CountItemTokens: func([]byte) (int64, error) { return 5, nil },
	}
}

func testCodexCompactionBody(branch string) []byte {
	return []byte(`{"model":"gpt-test","instructions":"keep","stream":true,"tools":[],"include":["custom.include"],"input":[` +
		`{"type":"message","role":"developer","content":[{"type":"input_text","text":"developer"}]},` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"old-user-` + branch + `"}]},` +
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"old-assistant"}]},` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"recent-user"}]},` +
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"recent-assistant"}]},` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"current-user-secret"}]}` + `]}`)
}

func appendCodexHistoryTurn(t *testing.T, body []byte) []byte {
	t.Helper()
	items := gjson.GetBytes(body, "input").Array()
	raw := make([]json.RawMessage, 0, len(items)+2)
	for _, item := range items {
		raw = append(raw, []byte(item.Raw))
	}
	raw = append(raw,
		json.RawMessage(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"more history"}]}`),
		json.RawMessage(`{"type":"message","role":"user","content":[{"type":"input_text","text":"new current"}]}`),
	)
	updated, err := setCodexInputItems(body, raw)
	if err != nil {
		t.Fatal(err)
	}
	return updated
}

func assertCodexReplay(t *testing.T, body []byte, artifact, branch string) {
	t.Helper()
	if got := gjson.GetBytes(body, `input.#(type=="compaction").encrypted_content`).String(); got != artifact {
		t.Fatalf("replay artifact = %q, want %q: %s", got, artifact, body)
	}
	if strings.Contains(string(body), "old-user-"+branch) || !strings.Contains(string(body), "current-user-secret") {
		t.Fatalf("unexpected replay body: %s", body)
	}
}
