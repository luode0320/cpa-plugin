// stream.go owns the upstream SSE data plane: emitting cleaned chunks back to
// the host stream (streamEmit/close), pumping the upstream SSE in a goroutine
// (pumpUpstreamStream), collecting it synchronously (collectUpstreamStream),
// and the SSE-frame helpers that re-frame, filter, and aggregate chunks.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// streamEmit pushes one chunk payload to the host stream. Returns an error if
// the host rejected it (e.g. the client already disconnected and the stream
// was closed), which the pump uses to stop reading a dead upstream.
func streamEmit(streamID string, payload []byte) error {
	if streamID == "" {
		return fmt.Errorf("no stream id")
	}
	body, _ := json.Marshal(map[string]any{"stream_id": streamID, "payload": payload})
	_, err := hostCall(pluginabi.MethodHostStreamEmit, body)
	return err
}

func streamEmitError(streamID, message string) {
	if streamID == "" {
		return
	}
	// A-37: never emit raw upstream bodies that may contain Bearer/JWT.
	errJSON, _ := json.Marshal(map[string]any{"error": map[string]any{"message": redactSecrets(message)}})
	_ = streamEmit(streamID, errJSON)
}

func streamClose(streamID string) {
	if streamID == "" {
		return
	}
	body, _ := json.Marshal(map[string]any{"stream_id": streamID})
	_, _ = hostCall(pluginabi.MethodHostStreamClose, body)
}

func streamHeaders() http.Header {
	h := http.Header{}
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	return h
}

// pumpUpstreamStream reads the upstream SSE response in the background and
// emits each cleaned chunk to the host stream. It closes the stream when done.
// An emit failure (client disconnected → host closed the stream) aborts the
// pump so we stop reading a dead upstream. cancel is invoked on every exit so
// the underlying http request context is released promptly.
//
// v0.7.0: requests now route via host.http.do_stream so request-log captures
// the outbound call and host transport policy applies. The host bridge emits
// arbitrary 32KB chunks, so we adapt to io.Reader and keep the bufio.Scanner
// SSE line framing unchanged.
func pumpUpstreamStream(httpReq *http.Request, cancel context.CancelFunc, streamID string, sseFramed bool, requestedModel, upstreamModel, authUID string, started time.Time, authID, reasoningEffort, accountLabel, sessionKey string) {
	// Always close the host stream exactly once on every exit path.
	closed := false
	closeOnce := func() {
		if closed {
			return
		}
		closed = true
		streamClose(streamID)
	}
	defer closeOnce()
	if cancel != nil {
		defer cancel()
	}

	// Per-request account-failover budget. 0 means no retry (kill switch
	// via `retry_on_4xx: 0` in plugin.yaml). Hot path is unchanged when
	// the budget is 0 — we still go through one attempt and propagate.
	budget := loadedRetryOn4xx()
	curReq := httpReq
	curAuthID := authID
	curAuthUID := authUID
	curAccountLabel := accountLabel

	for attempt := 0; attempt <= budget; attempt++ {
		stream, statusCode, _, err := hostHTTPDoStream(curReq)
		if err != nil {
			publishUsage(requestedModel, upstreamModel, curAuthUID, started, usage.Detail{}, true, 0, err.Error(), reasoningEffort, 0, curAccountLabel, sessionKey)
			noteAccountFailure(curAuthID, 0, err.Error())
			streamEmitError(streamID, fmt.Sprintf("http_error: %v", err))
			return
		}
		if statusCode >= 400 {
			// Drain the error body via the same bridge so the message is complete.
			errPayload := readAllUpstreamErr(newHostStreamReader(stream))
			stream.Close()
			publishUsage(requestedModel, upstreamModel, curAuthUID, started, usage.Detail{}, true, statusCode, errPayload, reasoningEffort, 0, curAccountLabel, sessionKey)
			if curAuthUID != "" {
				go reconcileByUID(curAuthUID, statusCode, errPayload)
			}
			noteAccountFailure(curAuthID, statusCode, errPayload)

			// Retry policy: only account-level 4xx (401/403/404/405)
			// benefit from switching accounts. 5xx/0/429/402 are already
			// surfaced as failures and recorded; rotating the account
			// on 5xx inside a single request gives no guarantee the
			// next upstream isn't also 5xx, so we don't burn budget
			// on it (the cooldown mechanism handles long-term 5xx).
			// Business 400 is request-shaped and would fail identically
			// on every account, so we propagate it immediately.
			if !isAccountLevel4xx(statusCode) {
				streamEmitError(streamID, fmt.Sprintf("upstream %d: %s", statusCode, truncateRedacted(errPayload, 200)))
				return
			}
			if attempt >= budget {
				streamEmitError(streamID, fmt.Sprintf("upstream %d: %s", statusCode, truncateRedacted(errPayload, 200)))
				return
			}
			// Pick the next account and rebuild the request against
			// its endpoint + auth headers. If nothing else is
			// usable, surface the original error.
			nextID, nextSA, hasNext := pickNextAuth(curAuthID)
			if !hasNext || nextSA == nil {
				streamEmitError(streamID, fmt.Sprintf("upstream %d: %s", statusCode, truncateRedacted(errPayload, 200)))
				return
			}
			nextReq, rebErr := rebuildRequestWithSA(curReq, nextSA)
			if rebErr != nil {
				streamEmitError(streamID, fmt.Sprintf("upstream %d: %s (retry rebuild failed: %v)", statusCode, truncateRedacted(errPayload, 200), rebErr))
				return
			}
			curReq = nextReq
			curAuthID = nextID
			curAuthUID = nextSA.Account.UID
			curAccountLabel = strings.TrimSpace(nextSA.Account.Nickname)
			if curAccountLabel == "" {
				curAccountLabel = curAuthUID
			}
			continue
		}
		// Success — pump chunks to the host stream. From here on this
		// attempt owns the response stream and must close it before
		// returning.
		collector := &sseUsageCollector{}
		scanner := bufio.NewScanner(newHostStreamReader(stream))
		scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
		for scanner.Scan() {
			content := stripDataPrefix(scanner.Text())
			if content == "" || content == "[DONE]" {
				continue
			}
			collector.feed(content)
			cleaned := cleanChunkJSON(content)
			if cleaned == "" {
				continue
			}
			if sseFramed {
				cleaned = "data: " + cleaned
			}
			if err := streamEmit(streamID, []byte(cleaned)); err != nil {
				// Client disconnected / host closed stream — abort; do not report success.
				stream.Close()
				publishUsage(requestedModel, upstreamModel, curAuthUID, started, collector.detail(), true, 0, "stream_emit: "+err.Error(), reasoningEffort, collector.ttftNS(started), curAccountLabel, sessionKey)
				return
			}
		}
		// A mid-stream read failure means the client received a truncated stream:
		// surface it as an error frame and record the attempt as failed.
		stream.Close()
		if err := scanner.Err(); err != nil {
			publishUsage(requestedModel, upstreamModel, curAuthUID, started, collector.detail(), true, 0, err.Error(), reasoningEffort, collector.ttftNS(started), curAccountLabel, sessionKey)
			noteAccountFailure(curAuthID, 0, err.Error())
			streamEmitError(streamID, fmt.Sprintf("upstream stream read error: %v", err))
			return
		}
		publishUsage(requestedModel, upstreamModel, curAuthUID, started, collector.detail(), false, 0, "", reasoningEffort, collector.ttftNS(started), curAccountLabel, sessionKey)
		invalidateAccountCredits(curAuthID, curAuthUID)
		resetAccountFailover(curAuthID)
		return
	}
}

// collectUpstreamStream is the synchronous fallback (no async stream id): drain
// the upstream, clean each chunk, return them as a slice. The collector, when
// non-nil, observes raw upstream chunks for usage extraction. statusCode is the
// upstream HTTP status (0 for transport-level failures).
//
// On an account-level 4xx (401/403/404/405) the request body is fine — only
// the chosen credential is broken for this endpoint. We try a
// configured-budget number of alternate accounts before giving up. Failures
// found by isAccountFailure (5xx, 0, 429, 402) are still surfaced as
// errors but do NOT trigger a same-request account rotation; the
// failover-cooldown layer handles those across requests.
func collectUpstreamStream(body []byte, sa *storedAuth, sseFramed bool, collector *sseUsageCollector) ([]pluginapi.ExecutorStreamChunk, int, error) {
	budget := loadedRetryOn4xx()
	curSA := sa
	var lastStatus int
	var lastErr error

	for attempt := 0; attempt <= budget; attempt++ {
		chunks, statusCode, errOnce := collectUpstreamStreamOnce(body, curSA, sseFramed, collector)
		if errOnce == nil {
			return chunks, statusCode, nil
		}
		lastStatus = statusCode
		lastErr = errOnce

		// Same retry policy as pumpUpstreamStream: only account-level
		// 4xx warrant a same-request account rotation.
		if !isAccountLevel4xx(statusCode) {
			return chunks, statusCode, errOnce
		}
		if attempt >= budget {
			return chunks, statusCode, errOnce
		}
		// curSA.Account.UID is the host auth id; for pickNextAuth we
		// key on the same scheduler-level ID that noteAccountFailure
		// uses, which is sa.Auth.AccessToken-derived. If we don't
		// have an auth-id-shaped handle, skip the swap (the original
		// error propagates).
		currentID := strings.TrimSpace(curSA.Auth.AccessToken)
		if currentID == "" {
			currentID = strings.TrimSpace(curSA.Account.UID)
		}
		if currentID == "" {
			return chunks, statusCode, errOnce
		}
		_, nextSA, hasNext := pickNextAuth(currentID)
		if !hasNext || nextSA == nil {
			return chunks, statusCode, errOnce
		}
		curSA = nextSA
	}
	return nil, lastStatus, lastErr
}

// collectUpstreamStreamOnce performs exactly one synchronous upstream call
// against sa: build the request, route via host.http.do_stream, decide
// success vs 4xx/5xx. Pulled out of collectUpstreamStream so the retry
// loop can rebuild the request against a different account on failure.
func collectUpstreamStreamOnce(body []byte, sa *storedAuth, sseFramed bool, collector *sseUsageCollector) ([]pluginapi.ExecutorStreamChunk, int, error) {
	httpReq, err := http.NewRequest(http.MethodPost, endpointChatFor(sa), bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	backendHeaders(httpReq, sa)
	// Compliance: route via host.http.do_stream so request-log captures the call.
	stream, statusCode, _, err := hostHTTPDoStream(httpReq)
	if err != nil {
		return nil, 0, fmt.Errorf("http_error: %w", err)
	}
	defer stream.Close()
	reader := newHostStreamReader(stream)
	if statusCode >= 400 {
		errPayload, _ := io.ReadAll(reader)
		if sa != nil && sa.Account.UID != "" {
			go reconcileByUID(sa.Account.UID, statusCode, string(errPayload))
		}
		return nil, statusCode, fmt.Errorf("upstream %d: %s", statusCode, truncateRedacted(string(errPayload), 200))
	}
	chunks, errAgg := aggregateSSEWithCollector(reader, sseFramed, collector)
	if errAgg != nil {
		return chunks, statusCode, errAgg
	}
	return chunks, statusCode, nil
}

// doExecuteOnce performs exactly one synchronous execute against sa: build
// the request, route via host.http.do_stream, fold SSE into a chat.completion
// payload. Pulled out of handleExecExecute so the retry loop can rebuild the
// request against a different account on failure. Returns a non-nil error
// in the canonical "upstream N: ..." or "http_error: ..." shape that
// parseUpstreamStatusFromErr understands.
func doExecuteOnce(body []byte, sa *storedAuth, requestedModel string) ([]byte, error) {
	httpReq, err := http.NewRequest(http.MethodPost, endpointChatFor(sa), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	backendHeaders(httpReq, sa)
	stream, statusCode, _, err := hostHTTPDoStream(httpReq)
	if err != nil {
		return nil, fmt.Errorf("http_error: %w", err)
	}
	defer stream.Close()
	reader := newHostStreamReader(stream)
	if statusCode >= 400 {
		payload, _ := io.ReadAll(reader)
		if sa != nil && sa.Account.UID != "" {
			go reconcileByUID(sa.Account.UID, statusCode, string(payload))
		}
		return nil, fmt.Errorf("upstream %d: %s", statusCode, truncateRedacted(string(payload), 200))
	}
	var firstByteAt time.Time
	completion, err := aggregateCompletion(reader, requestedModel, &firstByteAt)
	if err != nil {
		return nil, err
	}
	return completion, nil
}

// parseUpstreamStatusFromErr recovers the upstream HTTP status code from a
// "upstream N: ..." sentinel error (or returns 0 for any other shape /
// transport-level errors like http_error / http_error: ...). Used by the
// execute-retry loop in handleExecExecute to decide whether to switch
// accounts.
func parseUpstreamStatusFromErr(err error) int {
	if err == nil {
		return 0
	}
	msg := err.Error()
	if !strings.HasPrefix(msg, "upstream ") {
		return 0
	}
	rest := strings.TrimPrefix(msg, "upstream ")
	idx := strings.Index(rest, ":")
	if idx <= 0 {
		return 0
	}
	n, perr := strconv.Atoi(strings.TrimSpace(rest[:idx]))
	if perr != nil {
		return 0
	}
	return n
}

// clientNeedsSSEFrame reports whether chunk payloads must carry their own
// "data: " SSE framing. CPA's chat-completions passthrough adds the prefix
// itself, but every cross-format response translator (claude/gemini/codex/...)
// only consumes payloads already framed as "data: " lines. The host hands the
// plugin the inbound request path in Metadata, so we frame chunks ourselves for
// any entry path other than the native OpenAI chat-completions one.
func clientNeedsSSEFrame(metadata map[string]any) bool {
	path, _ := metadata["request_path"].(string)
	switch strings.ToLower(strings.TrimSpace(path)) {
	case "/v1/chat/completions", "/v1/completions":
		return false
	default:
		return true
	}
}

// aggregateSSEWithCollector reads an upstream SSE stream and emits one chunk
// per data event. Empty tool-call shells are stripped and the trailing [DONE]
// is dropped (the host appends its own stream terminator). When sseFramed is
// true each payload is emitted as a "data: " line for cross-format
// translators; otherwise the payload is the raw JSON object and the host
// chat-completions writer adds the framing itself. A mid-stream read error
// aborts collection and is returned so the caller records the attempt as
// failed. The collector, when non-nil, observes raw upstream chunks for usage
// extraction.
func aggregateSSEWithCollector(r io.Reader, sseFramed bool, collector *sseUsageCollector) ([]pluginapi.ExecutorStreamChunk, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	var chunks []pluginapi.ExecutorStreamChunk
	for scanner.Scan() {
		content := stripDataPrefix(scanner.Text())
		if content == "" || content == "[DONE]" {
			continue
		}
		if collector != nil {
			collector.feed(content)
		}
		cleaned := cleanChunkJSON(content)
		if cleaned == "" {
			continue
		}
		if sseFramed {
			cleaned = "data: " + cleaned
		}
		chunks = append(chunks, pluginapi.ExecutorStreamChunk{Payload: []byte(cleaned)})
	}
	if err := scanner.Err(); err != nil {
		return chunks, fmt.Errorf("upstream stream read error: %w", err)
	}
	return chunks, nil
}

// cleanChunkJSON strips only the known-problematic empty tool-call shells
// from choice deltas: a null/empty function_call and an empty tool_calls array
// (CodeBuddy emits these on the terminal chunk, and strict clients interpret
// them as a truncated tool call). Other empty-but-legal values are preserved:
// content:"" is a valid delta (pure tool-call chunk) and the role-only first
// chunk must survive so clients can establish the message role.
func cleanChunkJSON(s string) string {
	var obj map[string]any
	if json.Unmarshal([]byte(s), &obj) != nil {
		return s
	}
	changed := false
	if choices, ok := obj["choices"].([]any); ok {
		for _, c := range choices {
			choice, ok := c.(map[string]any)
			if !ok {
				continue
			}
			delta, ok := choice["delta"].(map[string]any)
			if !ok {
				continue
			}
			if v, present := delta["function_call"]; present && isEmptyValue(v) {
				delete(delta, "function_call")
				changed = true
			}
			if v, present := delta["tool_calls"]; present {
				if arr, isArr := v.([]any); isArr && len(arr) == 0 {
					delete(delta, "tool_calls")
					changed = true
				}
			}
			// Upstream often pads terminal/noop deltas with empty noise fields
			// that clients ignore but pollute wire size / some parsers.
			for _, noise := range []string{"extra_fields", "refusal", "reasoning_content"} {
				if v, present := delta[noise]; present && isEmptyValue(v) {
					delete(delta, noise)
					changed = true
				}
			}
			// Drop a fully-empty delta ONLY when the choice carries no other
			// signal (no finish_reason): e.g. {"delta":{"function_call":null}}
			// reduced to {}. A delta with role/content:"" is meaningful and
			// never reaches this branch (those fields are preserved above).
			if len(delta) == 0 {
				if fr, _ := choice["finish_reason"].(string); fr == "" {
					return ""
				}
			}
		}
	}
	if !changed {
		return s
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return s
	}
	return string(out)
}

func aggregateCompletion(r io.Reader, model string, firstByteAt *time.Time) ([]byte, error) {
	var content, reasoning, role, respModel, respID, finish string
	var created int64
	var usage map[string]any
	// tool_calls arrive as streaming deltas: each chunk carries an index plus a
	// partial call (id/type/function.name on the first delta, argument text
	// fragments afterwards). Merge by index instead of appending raw fragments
	// so the folded completion holds whole calls.
	toolCalls := map[int]map[string]any{}
	var toolOrder []int
	var scanErr error

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		data := stripDataPrefix(scanner.Text())
		if data == "" || data == "[DONE]" {
			continue
		}
		var chunk map[string]any
		if json.Unmarshal([]byte(data), &chunk) != nil {
			continue
		}
		if firstByteAt != nil && firstByteAt.IsZero() {
			*firstByteAt = time.Now()
		}
		if v, ok := chunk["id"].(string); ok && v != "" {
			respID = v
		}
		if v, ok := chunk["model"].(string); ok && v != "" {
			respModel = v
		}
		if v, ok := chunk["created"].(float64); ok {
			created = int64(v)
		}
		if v, ok := chunk["usage"].(map[string]any); ok {
			usage = v
		}
		choices, _ := chunk["choices"].([]any)
		for _, c := range choices {
			choice, _ := c.(map[string]any)
			if delta, ok := choice["delta"].(map[string]any); ok {
				if v, ok := delta["role"].(string); ok && v != "" {
					role = v
				}
				if v, ok := delta["content"].(string); ok {
					content += v
				}
				if v, ok := delta["reasoning_content"].(string); ok {
					reasoning += v
				}
				if tcs, ok := delta["tool_calls"].([]any); ok {
					for _, tc := range tcs {
						call, ok := tc.(map[string]any)
						if !ok {
							continue
						}
						idx := 0
						if v, ok := call["index"].(float64); ok {
							idx = int(v)
						}
						merged, seen := toolCalls[idx]
						if !seen {
							merged = map[string]any{"index": idx}
							toolCalls[idx] = merged
							toolOrder = append(toolOrder, idx)
						}
						mergeToolCallDelta(merged, call)
					}
				}
			}
			if v, ok := choice["finish_reason"].(string); ok && v != "" {
				finish = v
			}
		}
	}
	if err := scanner.Err(); err != nil {
		scanErr = err
	}
	// A mid-stream read failure means the folded completion is truncated. The
	// host discards the payload entirely when the plugin returns an error
	// (sdk/api/handlers executeWithPluginExecutor), so fail fast here instead
	// of assembling a partial completion nobody can safely consume.
	if scanErr != nil {
		return nil, fmt.Errorf("upstream stream read error: %w", scanErr)
	}

	message := map[string]any{"role": firstNonEmpty(role, "assistant"), "content": content}
	if reasoning != "" {
		message["reasoning_content"] = reasoning
	}
	if len(toolOrder) > 0 {
		sort.Ints(toolOrder)
		calls := make([]map[string]any, 0, len(toolOrder))
		for _, idx := range toolOrder {
			calls = append(calls, toolCalls[idx])
		}
		message["tool_calls"] = calls
	}
	if created == 0 {
		created = time.Now().Unix()
	}
	result := map[string]any{
		"id":      firstNonEmpty(respID, "chatcmpl-workbuddy"),
		"object":  "chat.completion",
		"created": created,
		"model":   firstNonEmpty(respModel, model),
		"choices": []map[string]any{{
			"index":         0,
			"message":       message,
			"finish_reason": firstNonEmpty(finish, "stop"),
		}},
	}
	if usage != nil {
		result["usage"] = usage
	}
	out, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// mergeToolCallDelta folds one streaming tool_call fragment into the merged
// call: scalar fields (id/type) are taken when first seen, function.name is
// concatenated (upstream may split it), and function.arguments text fragments
// are appended in arrival order.
func mergeToolCallDelta(merged, delta map[string]any) {
	for _, k := range []string{"id", "type"} {
		if _, present := merged[k]; !present {
			if v, ok := delta[k].(string); ok && v != "" {
				merged[k] = v
			}
		}
	}
	dfn, _ := delta["function"].(map[string]any)
	if dfn == nil {
		return
	}
	mfn, _ := merged["function"].(map[string]any)
	if mfn == nil {
		mfn = map[string]any{}
		merged["function"] = mfn
	}
	if v, ok := dfn["name"].(string); ok && v != "" {
		cur, _ := mfn["name"].(string)
		mfn["name"] = cur + v
	}
	if v, ok := dfn["arguments"].(string); ok && v != "" {
		cur, _ := mfn["arguments"].(string)
		mfn["arguments"] = cur + v
	}
}

func stripDataPrefix(s string) string {
	s = strings.TrimSpace(s)
	for strings.HasPrefix(s, "data:") {
		s = strings.TrimSpace(strings.TrimPrefix(s, "data:"))
	}
	return s
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
