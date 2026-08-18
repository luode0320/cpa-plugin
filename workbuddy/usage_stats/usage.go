package usagestats

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

const maxDimensionRunes = 160

type normalizedUsage struct {
	Dimensions  Dimensions
	RequestedAt time.Time
	LatencyNS   uint64
	TTFTNS      uint64
	Counters    Counters
	authIndex   string
}

func decodeUsage(raw []byte, now time.Time) (normalizedUsage, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		return normalizedUsage{}, fmt.Errorf("decode usage record: %w", err)
	}

	requestedAt := firstTime(root, "RequestedAt", "requested_at")
	now = now.UTC()
	if requestedAt.IsZero() || requestedAt.After(now.Add(24*time.Hour)) {
		requestedAt = now
	} else {
		requestedAt = requestedAt.UTC()
	}

	failure := firstObject(root, "Failure", "failure")
	detail := firstObject(root, "Detail", "detail")
	inputTokens := firstInt64(detail, "InputTokens", "input_tokens")
	outputTokens := firstInt64(detail, "OutputTokens", "output_tokens")
	reasoningTokens := firstInt64(detail, "ReasoningTokens", "reasoning_tokens")
	cachedTokens := firstInt64(detail, "CachedTokens", "cached_tokens")
	cacheReadTokens := firstInt64(detail, "CacheReadTokens", "cache_read_tokens")
	cacheCreationTokens := firstInt64(detail, "CacheCreationTokens", "cache_creation_tokens")
	total := firstInt64(detail, "TotalTokens", "total_tokens")
	if total <= 0 {
		// The SDK always serializes TotalTokens, so providers that leave it at
		// zero still produce a present JSON field. Match the raw-record fallback
		// used by the companion statistics implementation: input + output +
		// reasoning, then cached tokens only when that sum is still zero.
		total = saturatingInt64Sum(saturatingInt64Sum(inputTokens, outputTokens), reasoningTokens)
		if total == 0 {
			total = cachedTokens
		}
	}

	failed := firstBool(root, "Failed", "failed")
	provider := normalizeDimension(firstString(root, "Provider", "provider"))
	executorType := normalizeDimension(firstString(root, "ExecutorType", "executor_type"))
	authType := normalizeDimension(firstString(root, "AuthType", "auth_type"))
	apiKey := firstString(root, "APIKey", "api_key")
	return normalizedUsage{
		Dimensions: Dimensions{
			Provider:        provider,
			ExecutorType:    executorType,
			Model:           normalizeDimension(firstString(root, "Model", "model")),
			Alias:           normalizeDimension(firstString(root, "Alias", "alias")),
			Source:          safeUsageSource(firstString(root, "Source", "source"), apiKey, provider, executorType, authType),
			APIKey:          normalizeDimension(apiKey),
			AuthType:        authType,
			ServiceTier:     normalizeDimension(firstString(root, "ServiceTier", "service_tier")),
			ReasoningEffort: normalizeDimension(firstString(root, "ReasoningEffort", "reasoning_effort")),
			Failed:          failed,
			FailureStatus:   clampStatus(firstInt64(failure, "StatusCode", "status_code")),
		},
		RequestedAt: requestedAt,
		LatencyNS:   positiveDurationNS(root, "Latency", "latency", "latency_ns"),
		TTFTNS:      positiveDurationNS(root, "TTFT", "ttft", "ttft_ns"),
		authIndex:   strings.TrimSpace(firstString(root, "AuthIndex", "auth_index")),
		Counters: Counters{
			Requests:            1,
			FailedRequests:      boolCount(failed),
			InputTokens:         positiveUint(inputTokens),
			OutputTokens:        positiveUint(outputTokens),
			ReasoningTokens:     positiveUint(reasoningTokens),
			CachedTokens:        positiveUint(cachedTokens),
			CacheReadTokens:     positiveUint(cacheReadTokens),
			CacheCreationTokens: positiveUint(cacheCreationTokens),
			TotalTokens:         positiveUint(total),
		},
	}, nil
}

func safeUsageSource(rawSource, apiKey, provider, executorType, authType string) string {
	source := strings.TrimSpace(rawSource)
	if safeURL := sanitizeServiceURL(source); safeURL != "" {
		return normalizeDimension(safeURL)
	}

	// CLIProxyAPI currently uses the selected upstream API key itself as Source
	// for API-key credentials. Never persist that value. UsageRecord does not
	// expose the configured base_url, so use the provider's public service URL
	// when it is known and a non-secret provider identifier otherwise.
	if isAPIKeyAuth(authType) || sameSecret(source, apiKey) || looksLikeCredential(source) {
		return normalizeDimension(providerServiceAddress(provider, executorType))
	}
	return normalizeDimension(source)
}

func canonicalUsageSource(dimensions Dimensions) string {
	return canonicalUsageSourceWithIdentity(dimensions, "", "")
}

func canonicalUsageSourceWithIdentity(dimensions Dimensions, authProvider, authAccount string) string {
	source := safeUsageSource(dimensions.Source, "", dimensions.Provider, dimensions.ExecutorType, dimensions.AuthType)
	if provider, account := displayAuthProvider(authProvider), safeAuthAccount(authAccount); provider != "" && account != "" {
		return normalizeDimension(provider + "-" + account)
	}
	if isOpenAICompatibleProvider(dimensions.Provider, dimensions.ExecutorType) {
		const prefix = "openai-compatible-"
		if strings.HasPrefix(strings.ToLower(source), prefix) {
			if name := normalizeDimension(source[len(prefix):]); name != "" {
				return name
			}
		}
	}
	return source
}

func isOpenAICompatibleProvider(provider, executorType string) bool {
	provider = strings.ToLower(strings.TrimSpace(provider))
	executorType = strings.ToLower(strings.TrimSpace(executorType))
	return strings.HasPrefix(provider, "openai-compatible-") || provider == "openai-compatibility" || executorType == "openaicompatexecutor"
}
func sanitizeDimensionsSource(dimensions Dimensions) Dimensions {
	dimensions.Source = canonicalUsageSource(dimensions)
	return dimensions
}

func sanitizeServiceURL(value string) string {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Hostname() == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return ""
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.ForceQuery = false
	parsed.Fragment = ""
	return strings.TrimRight(parsed.String(), "/")
}

func isAPIKeyAuth(value string) bool {
	normalized := strings.NewReplacer("_", "", "-", "", " ", "").Replace(strings.ToLower(strings.TrimSpace(value)))
	return normalized == "apikey"
}

func sameSecret(left, right string) bool {
	left = strings.TrimSpace(left)
	right = strings.TrimSpace(right)
	return left != "" && right != "" && left == right
}

func looksLikeCredential(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	lower := strings.ToLower(value)
	for _, prefix := range []string{
		"bearer ", "basic ", "token ", "apikey ", "api-key ", "api_key ",
		"sk-", "sk_", "xai-", "gsk_", "AIza", "key-", "sess-",
	} {
		if strings.HasPrefix(lower, strings.ToLower(prefix)) {
			return true
		}
	}
	if len(value) < 24 || strings.ContainsAny(value, " /\\:@") {
		return false
	}
	var letters, digits int
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
			letters++
		case r >= '0' && r <= '9':
			digits++
		case r == '-' || r == '_' || r == '.':
		default:
			return false
		}
	}
	return letters > 0 && digits > 0
}

func providerServiceAddress(provider, executorType string) string {
	provider = strings.ToLower(strings.TrimSpace(provider))
	executorType = strings.ToLower(strings.TrimSpace(executorType))
	candidate := provider
	if candidate == "" {
		candidate = executorType
	}
	switch {
	case candidate == "openai", candidate == "codex", candidate == "openai-api":
		return "https://api.openai.com/v1"
	case candidate == "anthropic", candidate == "claude", candidate == "anthropic-api":
		return "https://api.anthropic.com"
	case candidate == "gemini", candidate == "gemini-interactions", candidate == "aistudio", candidate == "google":
		return "https://generativelanguage.googleapis.com"
	case candidate == "vertex", candidate == "gemini-vertex":
		return "https://aiplatform.googleapis.com"
	case candidate == "xai", candidate == "x-ai", candidate == "grok":
		return "https://api.x.ai/v1"
	case candidate == "kimi", candidate == "moonshot":
		return "https://api.kimi.com/coding"
	case candidate == "deepseek":
		return "https://api.deepseek.com"
	case candidate == "groq":
		return "https://api.groq.com/openai/v1"
	case candidate == "mistral":
		return "https://api.mistral.ai/v1"
	case candidate == "openrouter":
		return "https://openrouter.ai/api/v1"
	case candidate == "qwen", candidate == "dashscope", candidate == "alibaba":
		return "https://dashscope.aliyuncs.com/compatible-mode/v1"
	case candidate == "cohere":
		return "https://api.cohere.com"
	case candidate == "cerebras":
		return "https://api.cerebras.ai/v1"
	case candidate == "together", candidate == "togetherai":
		return "https://api.together.xyz/v1"
	case candidate == "siliconflow":
		return "https://api.siliconflow.cn/v1"
	}
	if provider != "" {
		return provider
	}
	if executorType != "" {
		return executorType
	}
	return ""
}

func firstObject(root map[string]json.RawMessage, keys ...string) map[string]json.RawMessage {
	for _, key := range keys {
		if value, ok := root[key]; ok {
			var result map[string]json.RawMessage
			if json.Unmarshal(value, &result) == nil {
				return result
			}
		}
	}
	return map[string]json.RawMessage{}
}

func firstString(root map[string]json.RawMessage, keys ...string) string {
	for _, key := range keys {
		if value, ok := root[key]; ok {
			var result string
			if json.Unmarshal(value, &result) == nil {
				return result
			}
		}
	}
	return ""
}

func firstBool(root map[string]json.RawMessage, keys ...string) bool {
	for _, key := range keys {
		if value, ok := root[key]; ok {
			var result bool
			if json.Unmarshal(value, &result) == nil {
				return result
			}
		}
	}
	return false
}

func firstInt64(root map[string]json.RawMessage, keys ...string) int64 {
	value, _ := firstInt64Present(root, keys...)
	return value
}

func firstInt64Present(root map[string]json.RawMessage, keys ...string) (int64, bool) {
	for _, key := range keys {
		value, ok := root[key]
		if !ok {
			continue
		}
		var result int64
		if json.Unmarshal(value, &result) == nil {
			return result, true
		}
	}
	return 0, false
}

func firstTime(root map[string]json.RawMessage, keys ...string) time.Time {
	for _, key := range keys {
		if value, ok := root[key]; ok {
			var result time.Time
			if json.Unmarshal(value, &result) == nil {
				return result
			}
		}
	}
	return time.Time{}
}

func positiveDurationNS(root map[string]json.RawMessage, keys ...string) uint64 {
	for _, key := range keys {
		value, ok := root[key]
		if !ok {
			continue
		}
		var numeric int64
		if json.Unmarshal(value, &numeric) == nil {
			return positiveUint(numeric)
		}
		var text string
		if json.Unmarshal(value, &text) == nil {
			if duration, err := time.ParseDuration(text); err == nil && duration > 0 {
				return uint64(duration)
			}
		}
	}
	return 0
}

func normalizeDimension(value string) string {
	value = strings.TrimSpace(value)
	if !utf8.ValidString(value) {
		value = strings.ToValidUTF8(value, "�")
	}
	runes := []rune(value)
	if len(runes) > maxDimensionRunes {
		value = string(runes[:maxDimensionRunes])
	}
	return value
}

func positiveUint(value int64) uint64 {
	if value <= 0 {
		return 0
	}
	return uint64(value)
}

func boolCount(value bool) uint64 {
	if value {
		return 1
	}
	return 0
}

func clampStatus(value int64) int {
	if value < 0 || value > 999 {
		return 0
	}
	return int(value)
}

func saturatingInt64Sum(left, right int64) int64 {
	if left <= 0 {
		left = 0
	}
	if right <= 0 {
		right = 0
	}
	if left > int64(^uint64(0)>>1)-right {
		return int64(^uint64(0) >> 1)
	}
	return left + right
}
