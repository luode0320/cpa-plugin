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
func pumpUpstreamStream(httpReq *http.Request, cancel context.CancelFunc, streamID string, sseFramed bool, requestedModel, upstreamModel, authUID string, started time.Time, authID, reasoningEffort, accountLabel string) {
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

	stream, statusCode, _, err := hostHTTPDoStream(httpReq)
	if err != nil {
		publishUsage(requestedModel, upstreamModel, authUID, started, usage.Detail{}, true, 0, err.Error(), reasoningEffort, 0, accountLabel)
		streamEmitError(streamID, fmt.Sprintf("http_error: %v", err))
		return
	}
	defer stream.Close()
	if statusCode >= 400 {
		// Drain the error body via the same bridge so the message is complete.
		errPayload, _ := io.ReadAll(newHostStreamReader(stream))
		publishUsage(requestedModel, upstreamModel, authUID, started, usage.Detail{}, true, statusCode, string(errPayload), reasoningEffort, 0, accountLabel)
		if authUID != "" {
			go reconcileByUID(authUID, statusCode, string(errPayload))
		}
		streamEmitError(streamID, fmt.Sprintf("upstream %d: %s", statusCode, truncateRedacted(string(errPayload), 200)))
		return
	}
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
			publishUsage(requestedModel, upstreamModel, authUID, started, collector.detail(), true, 0, "stream_emit: "+err.Error(), reasoningEffort, collector.ttftNS(started), accountLabel)
			return
		}
	}
	// A mid-stream read failure means the client received a truncated stream:
	// surface it as an error frame and record the attempt as failed.
	if err := scanner.Err(); err != nil {
		publishUsage(requestedModel, upstreamModel, authUID, started, collector.detail(), true, 0, err.Error(), reasoningEffort, collector.ttftNS(started), accountLabel)
		streamEmitError(streamID, fmt.Sprintf("upstream stream read error: %v", err))
		return
	}
	publishUsage(requestedModel, upstreamModel, authUID, started, collector.detail(), false, 0, "", reasoningEffort, collector.ttftNS(started), accountLabel)
	invalidateAccountCredits(authID, authUID)
}

// collectUpstreamStream is the synchronous fallback (no async stream id): drain
// the upstream, clean each chunk, return them as a slice. The collector, when
// non-nil, observes raw upstream chunks for usage extraction. statusCode is the
// upstream HTTP status (0 for transport-level failures).
func collectUpstreamStream(body []byte, sa *storedAuth, sseFramed bool, collector *sseUsageCollector) ([]pluginapi.ExecutorStreamChunk, int, error) {
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
