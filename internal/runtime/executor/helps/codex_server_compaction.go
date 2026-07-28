package helps

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	bolt "go.etcd.io/bbolt"
	"golang.org/x/sync/singleflight"
)

const (
	codexCompactionStateVersion                = 2
	codexCompactionBucket                      = "codex_server_compaction_v2"
	codexCompactionMaxSSELine                  = 52_428_800
	codexCompactionMaxLineagesPerCompatibility = 32
	codexCompactionMaxCompatibilityKeys        = 1024
)

var codexCompactionGroup singleflight.Group

// RemoveCodexImageGenerationForCompaction removes only the proxy-injected image tool from a preflight body.
func RemoveCodexImageGenerationForCompaction(body []byte) []byte {
	body = removeToolTypeFromPayloadWithRoot(body, "", "image_generation")
	return removeToolChoiceFromPayloadWithRoot(body, "", "image_generation")
}

// IsClaudeLocalCompactionRequest detects a known Claude Code compaction prompt only in the current user message.
func IsClaudeLocalCompactionRequest(original []byte) bool {
	messages := gjson.GetBytes(original, "messages")
	if !messages.IsArray() {
		return false
	}
	items := messages.Array()
	for index := len(items) - 1; index >= 0; index-- {
		message := items[index]
		if !strings.EqualFold(strings.TrimSpace(message.Get("role").String()), "user") {
			continue
		}
		lower := strings.ToLower(claudeMessageText(message.Get("content")))
		return strings.Contains(lower, "your task is to create a detailed summary of the conversation so far") ||
			strings.Contains(lower, "this session is being continued from a previous conversation that ran out of context")
	}
	return false
}

func claudeMessageText(content gjson.Result) string {
	if content.Type == gjson.String {
		return content.String()
	}
	if !content.IsArray() {
		return ""
	}
	parts := make([]string, 0, len(content.Array()))
	for _, part := range content.Array() {
		if text := strings.TrimSpace(part.Get("text").String()); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n")
}

// CodexServerCompactionRequest contains the normalized Codex request and stable routing identity.
type CodexServerCompactionRequest struct {
	Body            []byte
	ExecutionScope  string
	Model           string
	AuthID          string
	AccountID       string
	BaseURL         string
	ContextWindow   int64
	CountTokens     func([]byte) (int64, error)
	CountItemTokens func([]byte) (int64, error)
}

// CodexCompactionArtifact is the opaque replay item and its server-reported token cost.
type CodexCompactionArtifact struct {
	Item         []byte
	OutputTokens int64
}

// CodexServerCompactionFunc sends a preflight request and returns its opaque artifact.
type CodexServerCompactionFunc func(context.Context, []byte) (CodexCompactionArtifact, error)

type codexCompactionCompatibility struct {
	ExecutionScope     string `json:"execution_scope"`
	Model              string `json:"model"`
	AuthID             string `json:"auth_id"`
	AccountID          string `json:"account_id"`
	BaseURL            string `json:"base_url"`
	RequestFingerprint string `json:"request_fingerprint"`
}

type codexCompactionLineage struct {
	Version                int             `json:"version"`
	PrefixHashes           []string        `json:"prefix_hashes"`
	CoveredItemCount       int             `json:"covered_item_count"`
	CompactionItem         json.RawMessage `json:"compaction_item"`
	CompactedTokenEstimate int64           `json:"compacted_token_estimate"`
	EstimatedInputTokens   int64           `json:"estimated_input_tokens"`
	EstimatedPrefixTokens  int64           `json:"estimated_prefix_tokens"`
	CreatedAt              time.Time       `json:"created_at"`
	UpdatedAt              time.Time       `json:"updated_at"`
}

type codexCompactionState struct {
	Version       int                          `json:"version"`
	Compatibility codexCompactionCompatibility `json:"compatibility"`
	Lineages      []codexCompactionLineage     `json:"lineages"`
}

type codexCompactionResult struct {
	lineage codexCompactionLineage
}

// ApplyCodexServerCompaction replays the longest compatible lineage and creates a newer generation when needed.
func ApplyCodexServerCompaction(ctx context.Context, cfg config.CodexServerCompactionConfig, req CodexServerCompactionRequest, compact CodexServerCompactionFunc) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if !cfg.Enabled || compact == nil || req.CountTokens == nil || req.CountItemTokens == nil || req.ContextWindow <= 0 {
		return req.Body, nil
	}
	compatibility, errCompatibility := normalizeCodexCompactionCompatibility(req)
	if errCompatibility != nil {
		return req.Body, errCompatibility
	}
	if compatibility.ExecutionScope == "" || compatibility.Model == "" || compatibility.AuthID == "" || compatibility.BaseURL == "" {
		return req.Body, nil
	}

	availableContext := req.ContextWindow - cfg.OutputReserveTokens - cfg.SafetyMarginTokens
	threshold := int64(float64(availableContext) * cfg.TriggerRatio)
	if threshold <= 0 {
		return req.Body, nil
	}
	estimatedTokens, errCount := req.CountTokens(req.Body)
	if errCount != nil {
		return req.Body, fmt.Errorf("codex server compaction: estimate input tokens: %w", errCount)
	}
	if estimatedTokens < threshold {
		return req.Body, nil
	}

	items, hashes, errItems := canonicalCodexInputItems(req.Body)
	if errItems != nil || len(items) == 0 {
		return req.Body, errItems
	}
	statePath, errPath := expandCodexCompactionStatePath(cfg.StatePath)
	if errPath != nil {
		return req.Body, errPath
	}
	ttl := parsePositiveDuration(cfg.StateTTL, 7*24*time.Hour)
	state, errLoad := loadCodexCompactionState(statePath, compatibility, ttl)
	if errLoad != nil {
		return req.Body, errLoad
	}
	matched := longestCodexCompactionLineage(state.Lineages, hashes)
	replayed := req.Body
	if matched != nil {
		replayed, errItems = replayCodexCompaction(req.Body, items, *matched)
		if errItems != nil {
			return req.Body, errItems
		}
		explicitTokens, errReplayCount := req.CountTokens(replayed)
		if errReplayCount != nil {
			return req.Body, fmt.Errorf("codex server compaction: estimate replay tokens: %w", errReplayCount)
		}
		estimatedTokens = explicitTokens + matched.CompactedTokenEstimate
		if estimatedTokens < threshold {
			return replayed, nil
		}
	}

	boundary, errBoundary := codexCompactionBoundary(items, cfg.RetainedUserTokens, req.CountItemTokens)
	if errBoundary != nil {
		return req.Body, errBoundary
	}
	if boundary <= 0 || matched != nil && boundary <= matched.CoveredItemCount {
		return replayed, nil
	}
	prefixHashes := append([]string(nil), hashes[:boundary]...)
	preflightBody, errPreflight := buildCodexCompactionPreflight(req.Body, items, boundary, matched)
	if errPreflight != nil {
		return req.Body, errPreflight
	}
	prefixTokens, errPrefixCount := req.CountTokens(preflightBody)
	if errPrefixCount != nil {
		return req.Body, fmt.Errorf("codex server compaction: estimate prefix tokens: %w", errPrefixCount)
	}
	if matched != nil {
		prefixTokens += matched.CompactedTokenEstimate
	}

	flightKey := codexCompactionCompatibilityKey(compatibility) + ":" + hashStrings(prefixHashes)
	startFlight := func(flightCtx context.Context) <-chan singleflight.Result {
		return codexCompactionGroup.DoChan(flightKey, func() (any, error) {
			latest, errLatest := loadCodexCompactionState(statePath, compatibility, ttl)
			if errLatest != nil {
				return nil, errLatest
			}
			if existing := exactCodexCompactionLineage(latest.Lineages, prefixHashes); existing != nil {
				return codexCompactionResult{lineage: *existing}, nil
			}
			artifact, errCompact := compact(flightCtx, preflightBody)
			if errCompact != nil {
				return nil, errCompact
			}
			if !validCodexCompactionArtifact(artifact.Item) {
				return nil, fmt.Errorf("codex server compaction: invalid compaction artifact")
			}
			if artifact.OutputTokens < 0 {
				return nil, fmt.Errorf("codex server compaction: invalid output token estimate")
			}
			now := time.Now().UTC()
			lineage := codexCompactionLineage{
				Version:                codexCompactionStateVersion,
				PrefixHashes:           prefixHashes,
				CoveredItemCount:       boundary,
				CompactionItem:         append(json.RawMessage(nil), artifact.Item...),
				CompactedTokenEstimate: artifact.OutputTokens,
				EstimatedInputTokens:   estimatedTokens,
				EstimatedPrefixTokens:  prefixTokens,
				CreatedAt:              now,
				UpdatedAt:              now,
			}
			latest.Version = codexCompactionStateVersion
			latest.Compatibility = compatibility
			latest.Lineages = append(latest.Lineages, lineage)
			if errSave := saveCodexCompactionState(statePath, latest, ttl); errSave != nil {
				return nil, errSave
			}
			return codexCompactionResult{lineage: lineage}, nil
		})
	}

	if errContext := ctx.Err(); errContext != nil {
		return req.Body, errContext
	}
	for attempt := 0; attempt < 2; attempt++ {
		var flightResult singleflight.Result
		select {
		case <-ctx.Done():
			return req.Body, ctx.Err()
		case flightResult = <-startFlight(ctx):
		}
		if flightResult.Err != nil {
			if attempt == 0 && ctx.Err() == nil && (errors.Is(flightResult.Err, context.Canceled) || errors.Is(flightResult.Err, context.DeadlineExceeded)) {
				codexCompactionGroup.Forget(flightKey)
				continue
			}
			return req.Body, flightResult.Err
		}
		result, ok := flightResult.Val.(codexCompactionResult)
		if !ok {
			return req.Body, fmt.Errorf("codex server compaction: invalid singleflight result")
		}
		return replayCodexCompaction(req.Body, items, result.lineage)
	}
	return req.Body, fmt.Errorf("codex server compaction: exhausted canceled singleflight retry")
}

func normalizeCodexCompactionCompatibility(req CodexServerCompactionRequest) (codexCompactionCompatibility, error) {
	fingerprint, errFingerprint := codexCompactionRequestFingerprint(req.Body)
	if errFingerprint != nil {
		return codexCompactionCompatibility{}, errFingerprint
	}
	return codexCompactionCompatibility{
		ExecutionScope:     strings.TrimSpace(req.ExecutionScope),
		Model:              strings.TrimSpace(req.Model),
		AuthID:             strings.TrimSpace(req.AuthID),
		AccountID:          strings.TrimSpace(req.AccountID),
		BaseURL:            strings.TrimRight(strings.TrimSpace(req.BaseURL), "/"),
		RequestFingerprint: fingerprint,
	}, nil
}

func codexCompactionRequestFingerprint(body []byte) (string, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var root map[string]any
	if errDecode := decoder.Decode(&root); errDecode != nil {
		return "", fmt.Errorf("codex server compaction: decode request semantics: %w", errDecode)
	}
	for _, field := range []string{
		"input", "stream", "store", "previous_response_id", "prompt_cache_key", "prompt_cache_retention",
		"safety_identifier", "stream_options", "client_metadata", "metadata", "session_id", "conversation_id", "user",
	} {
		delete(root, field)
	}
	canonical, errMarshal := json.Marshal(root)
	if errMarshal != nil {
		return "", fmt.Errorf("codex server compaction: encode request semantics: %w", errMarshal)
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

func canonicalCodexInputItems(body []byte) ([]json.RawMessage, []string, error) {
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return nil, nil, fmt.Errorf("codex server compaction: input is not an array")
	}
	results := input.Array()
	items := make([]json.RawMessage, 0, len(results))
	hashes := make([]string, 0, len(results))
	for _, result := range results {
		if result.Type != gjson.JSON || strings.TrimSpace(result.Raw) == "" {
			return nil, nil, fmt.Errorf("codex server compaction: input item is not an object")
		}
		canonical, errCanonical := canonicalJSON([]byte(result.Raw))
		if errCanonical != nil {
			return nil, nil, fmt.Errorf("codex server compaction: canonicalize input item: %w", errCanonical)
		}
		sum := sha256.Sum256(canonical)
		items = append(items, append(json.RawMessage(nil), result.Raw...))
		hashes = append(hashes, hex.EncodeToString(sum[:]))
	}
	return items, hashes, nil
}

func canonicalJSON(raw []byte) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if errDecode := decoder.Decode(&value); errDecode != nil {
		return nil, errDecode
	}
	return json.Marshal(value)
}

func longestCodexCompactionLineage(lineages []codexCompactionLineage, hashes []string) *codexCompactionLineage {
	var best *codexCompactionLineage
	for index := range lineages {
		lineage := &lineages[index]
		if lineage.Version != codexCompactionStateVersion || !prefixMatches(hashes, lineage.PrefixHashes) || !validCodexCompactionArtifact(lineage.CompactionItem) {
			continue
		}
		if best == nil || len(lineage.PrefixHashes) > len(best.PrefixHashes) {
			best = lineage
		}
	}
	return best
}

func exactCodexCompactionLineage(lineages []codexCompactionLineage, hashes []string) *codexCompactionLineage {
	for index := range lineages {
		if equalStrings(lineages[index].PrefixHashes, hashes) && validCodexCompactionArtifact(lineages[index].CompactionItem) {
			return &lineages[index]
		}
	}
	return nil
}

func prefixMatches(items, prefix []string) bool {
	return len(prefix) > 0 && len(items) >= len(prefix) && equalStrings(items[:len(prefix)], prefix)
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func replayCodexCompaction(body []byte, items []json.RawMessage, lineage codexCompactionLineage) ([]byte, error) {
	if lineage.CoveredItemCount <= 0 || lineage.CoveredItemCount > len(items) || !validCodexCompactionArtifact(lineage.CompactionItem) {
		return nil, fmt.Errorf("codex server compaction: invalid replay lineage")
	}
	leading := leadingCodexDeveloperItems(items, lineage.CoveredItemCount)
	replay := make([]json.RawMessage, 0, leading+1+len(items)-lineage.CoveredItemCount)
	replay = append(replay, items[:leading]...)
	replay = append(replay, append(json.RawMessage(nil), lineage.CompactionItem...))
	replay = append(replay, items[lineage.CoveredItemCount:]...)
	return setCodexInputItems(body, replay)
}

func leadingCodexDeveloperItems(items []json.RawMessage, limit int) int {
	if limit > len(items) {
		limit = len(items)
	}
	count := 0
	for count < limit {
		item := gjson.ParseBytes(items[count])
		if item.Get("type").String() != "message" {
			break
		}
		role := strings.ToLower(strings.TrimSpace(item.Get("role").String()))
		if role != "developer" && role != "system" {
			break
		}
		count++
	}
	return count
}

func codexCompactionBoundary(items []json.RawMessage, retainedUserTokens int64, countItemTokens func([]byte) (int64, error)) (int, error) {
	boundary := pendingCodexUserTurnBoundary(items)
	if boundary <= 0 {
		return 0, nil
	}
	var retainedTokens int64
	for index := boundary - 1; index >= 0; index-- {
		item := gjson.ParseBytes(items[index])
		if item.Get("type").String() != "message" || !strings.EqualFold(strings.TrimSpace(item.Get("role").String()), "user") {
			continue
		}
		tokens, errCount := countItemTokens(items[index])
		if errCount != nil {
			return 0, fmt.Errorf("codex server compaction: estimate retained user message: %w", errCount)
		}
		if retainedTokens+tokens > retainedUserTokens && retainedTokens > 0 {
			break
		}
		retainedTokens += tokens
		boundary = index
	}
	boundary = avoidCodexFunctionPairSplit(items, boundary)
	if boundary <= leadingCodexDeveloperItems(items, len(items)) {
		return 0, nil
	}
	return boundary, nil
}

func pendingCodexUserTurnBoundary(items []json.RawMessage) int {
	lastUser := -1
	for index := len(items) - 1; index >= 0; index-- {
		item := gjson.ParseBytes(items[index])
		if item.Get("type").String() == "message" && strings.EqualFold(strings.TrimSpace(item.Get("role").String()), "user") {
			lastUser = index
			break
		}
	}
	if lastUser < 0 {
		return -1
	}
	boundary := lastUser
	for outputIndex := lastUser - 1; outputIndex >= 0; outputIndex-- {
		output := gjson.ParseBytes(items[outputIndex])
		if output.Get("type").String() != "function_call_output" {
			break
		}
		callID := strings.TrimSpace(output.Get("call_id").String())
		if callID == "" {
			continue
		}
		for callIndex := outputIndex - 1; callIndex >= 0; callIndex-- {
			call := gjson.ParseBytes(items[callIndex])
			if call.Get("type").String() == "function_call" && strings.TrimSpace(call.Get("call_id").String()) == callID {
				if callIndex < boundary {
					boundary = callIndex
				}
				break
			}
		}
	}
	return boundary
}

func avoidCodexFunctionPairSplit(items []json.RawMessage, boundary int) int {
	for {
		adjusted := boundary
		outputs := make(map[string]struct{})
		for index := boundary; index < len(items); index++ {
			item := gjson.ParseBytes(items[index])
			if item.Get("type").String() == "function_call_output" {
				if callID := strings.TrimSpace(item.Get("call_id").String()); callID != "" {
					outputs[callID] = struct{}{}
				}
			}
		}
		for index := 0; index < boundary; index++ {
			item := gjson.ParseBytes(items[index])
			if item.Get("type").String() != "function_call" {
				continue
			}
			if _, ok := outputs[strings.TrimSpace(item.Get("call_id").String())]; ok && index < adjusted {
				adjusted = index
			}
		}
		if adjusted == boundary {
			return boundary
		}
		boundary = adjusted
	}
}

func buildCodexCompactionPreflight(body []byte, items []json.RawMessage, boundary int, previous *codexCompactionLineage) ([]byte, error) {
	var prefix []json.RawMessage
	if previous != nil {
		if previous.CoveredItemCount <= 0 || previous.CoveredItemCount >= boundary || !validCodexCompactionArtifact(previous.CompactionItem) {
			return nil, fmt.Errorf("codex server compaction: invalid prior lineage")
		}
		leading := leadingCodexDeveloperItems(items, previous.CoveredItemCount)
		prefix = append(prefix, items[:leading]...)
		prefix = append(prefix, append(json.RawMessage(nil), previous.CompactionItem...))
		prefix = append(prefix, items[previous.CoveredItemCount:boundary]...)
	} else {
		prefix = append(prefix, items[:boundary]...)
	}
	prefix = append(prefix, json.RawMessage(`{"type":"compaction_trigger"}`))
	preflight, errSet := setCodexInputItems(body, prefix)
	if errSet != nil {
		return nil, errSet
	}
	preflight = SetBoolIfDifferent(preflight, "stream", true)
	preflight = SetBoolIfDifferent(preflight, "store", false)
	preflight, _ = sjson.DeleteBytes(preflight, "previous_response_id")
	preflight = ensureCodexCompactionInclude(preflight)
	return preflight, nil
}

func ensureCodexCompactionInclude(body []byte) []byte {
	const required = "reasoning.encrypted_content"
	include := gjson.GetBytes(body, "include")
	values := make([]string, 0, len(include.Array())+1)
	seen := make(map[string]struct{}, len(include.Array())+1)
	if include.IsArray() {
		for _, item := range include.Array() {
			if item.Type != gjson.String {
				continue
			}
			value := strings.TrimSpace(item.String())
			if value == "" {
				continue
			}
			if _, exists := seen[value]; exists {
				continue
			}
			seen[value] = struct{}{}
			values = append(values, value)
		}
	}
	if _, exists := seen[required]; !exists {
		values = append(values, required)
	}
	raw, errMarshal := json.Marshal(values)
	if errMarshal != nil {
		return body
	}
	updated, errSet := sjson.SetRawBytes(body, "include", raw)
	if errSet != nil {
		return body
	}
	return updated
}

func setCodexInputItems(body []byte, items []json.RawMessage) ([]byte, error) {
	raw, errMarshal := json.Marshal(items)
	if errMarshal != nil {
		return nil, fmt.Errorf("codex server compaction: encode input: %w", errMarshal)
	}
	updated, errSet := sjson.SetRawBytes(body, "input", raw)
	if errSet != nil {
		return nil, fmt.Errorf("codex server compaction: set input: %w", errSet)
	}
	return updated, nil
}

// ParseCodexCompactionSSE strictly validates a remote compaction stream.
func ParseCodexCompactionSSE(reader io.Reader) (CodexCompactionArtifact, error) {
	if reader == nil {
		return CodexCompactionArtifact{}, fmt.Errorf("codex server compaction: response body is nil")
	}
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(nil, codexCompactionMaxSSELine)
	var dataLines [][]byte
	var artifact []byte
	var outputTokens int64
	usageFound := false
	completed := false
	process := func() error {
		if len(dataLines) == 0 {
			return nil
		}
		data := bytes.Join(dataLines, []byte("\n"))
		dataLines = nil
		if bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
			return nil
		}
		if !json.Valid(data) {
			return fmt.Errorf("codex server compaction: malformed SSE data")
		}
		event := gjson.ParseBytes(data)
		eventType := event.Get("type").String()
		if completed {
			return fmt.Errorf("codex server compaction: event %q after response.completed", eventType)
		}
		switch eventType {
		case "response.output_item.done":
			item := event.Get("item")
			if item.Get("type").String() != "compaction" {
				return nil
			}
			if !validCodexCompactionArtifact([]byte(item.Raw)) {
				return fmt.Errorf("codex server compaction: invalid compaction item")
			}
			if artifact != nil {
				return fmt.Errorf("codex server compaction: multiple compaction items")
			}
			artifact = append([]byte(nil), item.Raw...)
		case "response.completed":
			if !validCodexCompactionArtifact(artifact) {
				return fmt.Errorf("codex server compaction: response.completed before compaction artifact")
			}
			if status := strings.TrimSpace(event.Get("response.status").String()); status != "" && status != "completed" {
				return fmt.Errorf("codex server compaction: response completed with status %q", status)
			}
			if responseError := event.Get("response.error"); responseError.Exists() && responseError.Type != gjson.Null {
				return fmt.Errorf("codex server compaction: response.completed contains an error")
			}
			usage := event.Get("response.usage.output_tokens")
			if !usage.Exists() || usage.Type != gjson.Number || usage.Int() < 0 {
				return fmt.Errorf("codex server compaction: missing output token usage")
			}
			outputTokens = usage.Int()
			usageFound = true
			completed = true
		case "response.failed", "response.incomplete", "error", "response.error":
			return fmt.Errorf("codex server compaction: terminal event %q", eventType)
		}
		return nil
	}
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			if errProcess := process(); errProcess != nil {
				return CodexCompactionArtifact{}, errProcess
			}
			continue
		}
		if bytes.HasPrefix(line, []byte("data:")) {
			dataLines = append(dataLines, bytes.TrimSpace(line[len("data:"):]))
		}
	}
	if errScan := scanner.Err(); errScan != nil {
		return CodexCompactionArtifact{}, fmt.Errorf("codex server compaction: read SSE: %w", errScan)
	}
	if errProcess := process(); errProcess != nil {
		return CodexCompactionArtifact{}, errProcess
	}
	if !completed || !usageFound {
		return CodexCompactionArtifact{}, fmt.Errorf("codex server compaction: missing response.completed")
	}
	if !validCodexCompactionArtifact(artifact) {
		return CodexCompactionArtifact{}, fmt.Errorf("codex server compaction: missing compaction artifact")
	}
	return CodexCompactionArtifact{Item: artifact, OutputTokens: outputTokens}, nil
}

func validCodexCompactionArtifact(raw []byte) bool {
	item := gjson.ParseBytes(raw)
	if item.Type != gjson.JSON || item.Get("type").String() != "compaction" {
		return false
	}
	encrypted := item.Get("encrypted_content")
	return encrypted.Type == gjson.String && strings.TrimSpace(encrypted.String()) != ""
}

func expandCodexCompactionStatePath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		path = config.DefaultCodexServerCompactionStatePath
	}
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, errHome := os.UserHomeDir()
		if errHome != nil {
			return "", fmt.Errorf("codex server compaction: resolve home directory: %w", errHome)
		}
		path = filepath.Join(home, strings.TrimPrefix(path, "~/"))
	}
	absolute, errAbs := filepath.Abs(path)
	if errAbs != nil {
		return "", fmt.Errorf("codex server compaction: resolve state path: %w", errAbs)
	}
	return filepath.Clean(absolute), nil
}

func loadCodexCompactionState(path string, compatibility codexCompactionCompatibility, ttl time.Duration) (codexCompactionState, error) {
	state := codexCompactionState{Version: codexCompactionStateVersion, Compatibility: compatibility}
	if _, errStat := os.Lstat(path); errStat != nil {
		if os.IsNotExist(errStat) {
			return state, nil
		}
		return state, fmt.Errorf("codex server compaction: stat state: %w", errStat)
	}
	db, errOpen := openCodexCompactionDB(path)
	if errOpen != nil {
		return state, errOpen
	}
	key := []byte(codexCompactionCompatibilityKey(compatibility))
	errView := db.View(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte(codexCompactionBucket))
		if bucket == nil {
			return nil
		}
		raw := bucket.Get(key)
		if len(raw) == 0 {
			return nil
		}
		if errUnmarshal := json.Unmarshal(raw, &state); errUnmarshal != nil {
			return fmt.Errorf("decode state: %w", errUnmarshal)
		}
		return nil
	})
	errClose := db.Close()
	if errView != nil {
		return codexCompactionState{}, fmt.Errorf("codex server compaction: load state: %w", errView)
	}
	if errClose != nil {
		return codexCompactionState{}, fmt.Errorf("codex server compaction: close state after load: %w", errClose)
	}
	if state.Version != codexCompactionStateVersion || state.Compatibility != compatibility {
		return codexCompactionState{Version: codexCompactionStateVersion, Compatibility: compatibility}, nil
	}
	state.Lineages = pruneCodexCompactionLineages(state.Lineages, ttl, time.Now().UTC())
	return state, nil
}

func saveCodexCompactionState(path string, state codexCompactionState, ttl time.Duration) error {
	state.Version = codexCompactionStateVersion
	state.Lineages = pruneCodexCompactionLineages(state.Lineages, ttl, time.Now().UTC())
	db, errOpen := openCodexCompactionDB(path)
	if errOpen != nil {
		return errOpen
	}
	errUpdate := db.Update(func(tx *bolt.Tx) error {
		bucket, errBucket := tx.CreateBucketIfNotExists([]byte(codexCompactionBucket))
		if errBucket != nil {
			return errBucket
		}
		now := time.Now().UTC()
		if errCleanup := cleanupCodexCompactionBucket(bucket, ttl, now); errCleanup != nil {
			return errCleanup
		}
		key := []byte(codexCompactionCompatibilityKey(state.Compatibility))
		merged := state
		if currentRaw := bucket.Get(key); len(currentRaw) > 0 {
			var current codexCompactionState
			if errUnmarshal := json.Unmarshal(currentRaw, &current); errUnmarshal != nil {
				return fmt.Errorf("decode current state: %w", errUnmarshal)
			}
			if current.Version == codexCompactionStateVersion && current.Compatibility == state.Compatibility {
				merged.Lineages = mergeCodexCompactionLineages(current.Lineages, state.Lineages)
			}
		}
		merged.Lineages = pruneCodexCompactionLineages(merged.Lineages, ttl, now)
		raw, errMarshal := json.Marshal(merged)
		if errMarshal != nil {
			return fmt.Errorf("encode state: %w", errMarshal)
		}
		if errPut := bucket.Put(key, raw); errPut != nil {
			return errPut
		}
		return enforceCodexCompactionCompatibilityCap(bucket)
	})
	errClose := db.Close()
	if errUpdate != nil {
		return fmt.Errorf("codex server compaction: save state: %w", errUpdate)
	}
	if errClose != nil {
		return fmt.Errorf("codex server compaction: close state after save: %w", errClose)
	}
	return nil
}

func mergeCodexCompactionLineages(current, incoming []codexCompactionLineage) []codexCompactionLineage {
	merged := make([]codexCompactionLineage, 0, len(current)+len(incoming))
	indexByPrefix := make(map[string]int, len(current)+len(incoming))
	appendLineage := func(lineage codexCompactionLineage) {
		key := hashStrings(lineage.PrefixHashes)
		if index, exists := indexByPrefix[key]; exists {
			if lineage.UpdatedAt.After(merged[index].UpdatedAt) {
				merged[index] = lineage
			}
			return
		}
		indexByPrefix[key] = len(merged)
		merged = append(merged, lineage)
	}
	for _, lineage := range current {
		appendLineage(lineage)
	}
	for _, lineage := range incoming {
		appendLineage(lineage)
	}
	return merged
}

func pruneCodexCompactionLineages(lineages []codexCompactionLineage, ttl time.Duration, now time.Time) []codexCompactionLineage {
	live := make([]codexCompactionLineage, 0, len(lineages))
	for _, lineage := range lineages {
		if lineage.Version != codexCompactionStateVersion || !validCodexCompactionArtifact(lineage.CompactionItem) {
			continue
		}
		if ttl > 0 && now.Sub(lineage.UpdatedAt) > ttl {
			continue
		}
		live = append(live, lineage)
	}
	sort.SliceStable(live, func(left, right int) bool {
		return live[left].UpdatedAt.After(live[right].UpdatedAt)
	})
	if len(live) > codexCompactionMaxLineagesPerCompatibility {
		live = live[:codexCompactionMaxLineagesPerCompatibility]
	}
	return live
}

func cleanupCodexCompactionBucket(bucket *bolt.Bucket, ttl time.Duration, now time.Time) error {
	var deleteKeys [][]byte
	var updates = make(map[string][]byte)
	errWalk := bucket.ForEach(func(key, value []byte) error {
		var state codexCompactionState
		if errUnmarshal := json.Unmarshal(value, &state); errUnmarshal != nil || state.Version != codexCompactionStateVersion {
			deleteKeys = append(deleteKeys, append([]byte(nil), key...))
			return nil
		}
		state.Lineages = pruneCodexCompactionLineages(state.Lineages, ttl, now)
		if len(state.Lineages) == 0 {
			deleteKeys = append(deleteKeys, append([]byte(nil), key...))
			return nil
		}
		raw, errMarshal := json.Marshal(state)
		if errMarshal != nil {
			return errMarshal
		}
		updates[string(key)] = raw
		return nil
	})
	if errWalk != nil {
		return errWalk
	}
	for _, key := range deleteKeys {
		if errDelete := bucket.Delete(key); errDelete != nil {
			return errDelete
		}
	}
	for key, value := range updates {
		if errPut := bucket.Put([]byte(key), value); errPut != nil {
			return errPut
		}
	}
	return nil
}

func enforceCodexCompactionCompatibilityCap(bucket *bolt.Bucket) error {
	type candidate struct {
		key       []byte
		updatedAt time.Time
	}
	candidates := make([]candidate, 0, bucket.Stats().KeyN)
	errWalk := bucket.ForEach(func(key, value []byte) error {
		var state codexCompactionState
		if errUnmarshal := json.Unmarshal(value, &state); errUnmarshal != nil {
			return errUnmarshal
		}
		var latest time.Time
		for _, lineage := range state.Lineages {
			if lineage.UpdatedAt.After(latest) {
				latest = lineage.UpdatedAt
			}
		}
		candidates = append(candidates, candidate{key: append([]byte(nil), key...), updatedAt: latest})
		return nil
	})
	if errWalk != nil || len(candidates) <= codexCompactionMaxCompatibilityKeys {
		return errWalk
	}
	sort.Slice(candidates, func(left, right int) bool {
		return candidates[left].updatedAt.Before(candidates[right].updatedAt)
	})
	for _, candidate := range candidates[:len(candidates)-codexCompactionMaxCompatibilityKeys] {
		if errDelete := bucket.Delete(candidate.key); errDelete != nil {
			return errDelete
		}
	}
	return nil
}

func openCodexCompactionDB(path string) (*bolt.DB, error) {
	if errSecure := validateCodexCompactionStatePath(path); errSecure != nil {
		return nil, errSecure
	}
	directory := filepath.Dir(path)
	if _, errStat := os.Lstat(directory); os.IsNotExist(errStat) {
		if errMkdir := os.MkdirAll(directory, 0o700); errMkdir != nil {
			return nil, fmt.Errorf("codex server compaction: create state directory: %w", errMkdir)
		}
	}
	if errSecure := validateCodexCompactionStatePath(path); errSecure != nil {
		return nil, errSecure
	}
	db, errOpen := bolt.Open(path, 0o600, &bolt.Options{Timeout: time.Second})
	if errOpen != nil {
		return nil, fmt.Errorf("codex server compaction: open state: %w", errOpen)
	}
	if errSecure := validateCodexCompactionStatePath(path); errSecure != nil {
		if errClose := db.Close(); errClose != nil {
			return nil, fmt.Errorf("%v (close state: %w)", errSecure, errClose)
		}
		return nil, errSecure
	}
	return db, nil
}

func validateCodexCompactionStatePath(path string) error {
	if errSymlink := rejectCodexCompactionSymlinkComponents(path); errSymlink != nil {
		return errSymlink
	}
	directory := filepath.Dir(path)
	if info, errStat := os.Lstat(directory); errStat == nil {
		if !info.IsDir() {
			return fmt.Errorf("codex server compaction: state parent is not a directory")
		}
		if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
			return fmt.Errorf("codex server compaction: unsafe state directory permissions %o", info.Mode().Perm())
		}
	} else if !os.IsNotExist(errStat) {
		return fmt.Errorf("codex server compaction: stat state directory: %w", errStat)
	}
	if info, errStat := os.Lstat(path); errStat == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("codex server compaction: state path is not a regular file")
		}
		if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
			return fmt.Errorf("codex server compaction: unsafe state file permissions %o", info.Mode().Perm())
		}
	} else if !os.IsNotExist(errStat) {
		return fmt.Errorf("codex server compaction: stat state file: %w", errStat)
	}
	return nil
}

func rejectCodexCompactionSymlinkComponents(path string) error {
	for _, candidate := range []string{filepath.Dir(path), path} {
		info, errStat := os.Lstat(candidate)
		if os.IsNotExist(errStat) {
			continue
		}
		if errStat != nil {
			return fmt.Errorf("codex server compaction: inspect state path component: %w", errStat)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("codex server compaction: symlink state path component rejected")
		}
	}
	return nil
}

func codexCompactionCompatibilityKey(compatibility codexCompactionCompatibility) string {
	raw, _ := json.Marshal(compatibility)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func hashStrings(values []string) string {
	hash := sha256.New()
	for _, value := range values {
		_, _ = io.WriteString(hash, value)
		_, _ = hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func parsePositiveDuration(raw string, fallback time.Duration) time.Duration {
	parsed, errParse := time.ParseDuration(strings.TrimSpace(raw))
	if errParse != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}
