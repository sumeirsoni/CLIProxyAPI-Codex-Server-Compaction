package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

const codexRemoteCompactionBeta = "remote_compaction_v2"

func (e *CodexExecutor) maybeApplyCodexServerCompaction(ctx context.Context, from sdktranslator.Format, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, baseModel, baseURL, apiKey string, body []byte, imageGenerationInjected bool) []byte {
	if e == nil || e.cfg == nil || !e.cfg.CodexServerCompaction.Enabled || !sourceFormatEqual(from, sdktranslator.FormatClaude) {
		return body
	}
	contextWindow, ok := e.cfg.CodexServerCompaction.Models[baseModel]
	if !ok || contextWindow <= 0 || auth == nil || strings.TrimSpace(auth.ID) == "" {
		return body
	}
	original := opts.OriginalRequest
	if len(original) == 0 {
		original = req.Payload
	}
	if helps.IsClaudeLocalCompactionRequest(original) {
		return body
	}
	accountID := codexCompactionAccountID(auth)
	if accountID == "" || !codexCompactionOAuthAuth(auth) {
		return body
	}
	executionScope, ok := helps.ClaudeCodeExecutionScope(ctx, original, opts.Headers)
	if !ok {
		if fallback := helps.ProviderSessionUUID("codex-compaction", opts.Metadata, req.Metadata); fallback != "" {
			executionScope = "claude-execution:" + fallback
		} else {
			return body
		}
	}
	encoder, errTokenizer := tokenizerForCodexModel(baseModel)
	if errTokenizer != nil {
		log.WithError(errTokenizer).Warn("codex server compaction: tokenizer unavailable; sending original request")
		return body
	}
	countTokens := func(payload []byte) (int64, error) {
		return countCodexInputTokens(encoder, payload)
	}
	countItemTokens := func(item []byte) (int64, error) {
		return countCodexInputItemTokens(encoder, item)
	}
	compactionBody := body
	if imageGenerationInjected {
		compactionBody = helps.RemoveCodexImageGenerationForCompaction(compactionBody)
	}
	request := helps.CodexServerCompactionRequest{
		Body:            compactionBody,
		ExecutionScope:  executionScope,
		Model:           baseModel,
		AuthID:          auth.ID,
		AccountID:       accountID,
		BaseURL:         baseURL,
		ContextWindow:   contextWindow,
		CountTokens:     countTokens,
		CountItemTokens: countItemTokens,
	}
	compacted, errCompaction := helps.ApplyCodexServerCompaction(ctx, e.cfg.CodexServerCompaction, request, func(callCtx context.Context, preflightBody []byte) (helps.CodexCompactionArtifact, error) {
		return e.executeCodexServerCompaction(callCtx, from, auth, req, opts, baseModel, baseURL, apiKey, preflightBody)
	})
	if errCompaction != nil {
		log.WithError(errCompaction).Warn("codex server compaction failed open; sending original request")
		return body
	}
	if imageGenerationInjected {
		compacted = ensureImageGenerationTool(compacted, baseModel, auth, opts.Headers)
	}
	return compacted
}

func (e *CodexExecutor) executeCodexServerCompaction(ctx context.Context, from sdktranslator.Format, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, baseModel, baseURL, apiKey string, body []byte) (helps.CodexCompactionArtifact, error) {
	url := strings.TrimSuffix(baseURL, "/") + "/responses"
	original := opts.OriginalRequest
	if len(original) == 0 {
		original = req.Payload
	}
	httpReq, _, identityState, errRequest := e.cacheHelper(ctx, from, url, auth, req, original, body, opts.Headers)
	if errRequest != nil {
		return helps.CodexCompactionArtifact{}, fmt.Errorf("codex server compaction: build request: %w", errRequest)
	}
	applyCodexHeaders(httpReq, auth, apiKey, true, e.cfg)
	applyModelHeaderOverrides(httpReq.Header, baseModel)
	applyCodexIdentityConfuseHeaders(httpReq.Header, &identityState)
	mergeCodexBetaFeature(httpReq.Header, opts.Headers.Get("X-Codex-Beta-Features"), codexRemoteCompactionBeta)

	httpClient := helps.NewUtlsHTTPClient(ctx, e.cfg, auth, 0)
	httpResp, errDo := httpClient.Do(httpReq)
	if errDo != nil {
		return helps.CodexCompactionArtifact{}, fmt.Errorf("codex server compaction: request failed: %w", errDo)
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("codex server compaction: close response body error: %v", errClose)
		}
	}()
	if httpResp.StatusCode < http.StatusOK || httpResp.StatusCode >= http.StatusMultipleChoices {
		_, _ = io.Copy(io.Discard, io.LimitReader(httpResp.Body, 1<<20))
		return helps.CodexCompactionArtifact{}, fmt.Errorf("codex server compaction: upstream status %d", httpResp.StatusCode)
	}
	artifact, errParse := helps.ParseCodexCompactionSSE(httpResp.Body)
	if errParse != nil {
		return helps.CodexCompactionArtifact{}, errParse
	}
	return artifact, nil
}

func codexRequestContainsCompaction(body []byte) bool {
	for _, item := range gjson.GetBytes(body, "input").Array() {
		if item.Get("type").String() == "compaction" {
			return true
		}
	}
	return false
}

func codexCompactionRequestLogBody(body []byte) []byte {
	if codexRequestContainsCompaction(body) {
		return nil
	}
	return body
}

func codexCompactionAccountID(auth *cliproxyauth.Auth) string {
	if auth == nil || auth.Metadata == nil {
		return ""
	}
	accountID, _ := auth.Metadata["account_id"].(string)
	return strings.TrimSpace(accountID)
}

func codexCompactionOAuthAuth(auth *cliproxyauth.Auth) bool {
	if auth == nil || auth.Metadata == nil {
		return false
	}
	accessToken, _ := auth.Metadata["access_token"].(string)
	return strings.TrimSpace(accessToken) != ""
}

func mergeCodexBetaFeature(headers http.Header, features ...string) {
	if headers == nil {
		return
	}
	values := strings.Split(headers.Get("X-Codex-Beta-Features"), ",")
	for _, feature := range features {
		values = append(values, strings.Split(feature, ",")...)
	}
	merged := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		key := strings.ToLower(value)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		merged = append(merged, value)
	}
	if len(merged) > 0 {
		headers.Set("X-Codex-Beta-Features", strings.Join(merged, ","))
	}
}
