// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package ebpfcommon // import "go.opentelemetry.io/obi/pkg/ebpf/common/http"

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"go.opentelemetry.io/obi/pkg/appolly/app/request"
	"go.opentelemetry.io/obi/pkg/config"
)

// modelFieldRegexp extracts the top-level "model" value from a (possibly
// truncated) JSON request body.  It is a best-effort fallback used only when
// json.Unmarshal cannot parse the body.  We limit the search window to
// modelSearchWindow bytes so that we don't accidentally match a "model"
// key buried inside a user prompt or message content.
var modelFieldRegexp = regexp.MustCompile(`"model"\s*:\s*"([^"]+)"`)
var streamFieldRegexp = regexp.MustCompile(`"stream"\s*:\s*true\b`)

const modelSearchWindow = 200
const qwenFallbackPayloadUnavailable = "[payload_unavailable]"
const qwenFallbackOutputUnavailableJSON = `[{"message":{"role":"assistant","content":"[payload_unavailable]"},"finish_reason":"unknown"}]`

func qwenRequestPath(req *http.Request) string {
	if req == nil {
		return ""
	}
	if req.URL != nil {
		if req.URL.Path != "" {
			return req.URL.Path
		}
		if req.URL.Opaque != "" {
			if parsed, err := url.Parse(req.URL.Opaque); err == nil && parsed.Path != "" {
				return parsed.Path
			}
			if strings.HasPrefix(req.URL.Opaque, "/") {
				return req.URL.Opaque
			}
		}
	}
	if req.RequestURI == "" {
		return ""
	}
	if parsed, err := url.ParseRequestURI(req.RequestURI); err == nil && parsed.Path != "" {
		return parsed.Path
	}
	return req.RequestURI
}

func isQwen(respHeader http.Header) bool {
	for _, header := range []string{"X-DashScope-Request-Id", "X-Dashscope-Call-Gateway"} {
		if val := respHeader.Get(header); val != "" {
			return true
		}
	}
	return false
}

// QwenSpan enriches a span for DashScope / Qwen HTTP traffic. streamAccum holds
// incremental SSE state built from TCP large-buffer ringbuf chunks; pass nil if
// unavailable (tests and non-instrumented paths).
func QwenSpan(baseSpan *request.Span, req *http.Request, resp *http.Response, streamAccum *QwenStreamAccum) (request.Span, bool) {
	path := qwenRequestPath(req)
	if !isQwen(resp.Header) {
		slog.Debug(
			"Qwen response headers not detected",
			"path", path,
			"has_x_dashscope_request_id", resp.Header.Get("X-DashScope-Request-Id") != "",
			"has_x_dashscope_call_gateway", resp.Header.Get("X-Dashscope-Call-Gateway") != "",
			"content_type", resp.Header.Get("Content-Type"),
		)
		return *baseSpan, false
	}

	reqB, err := io.ReadAll(req.Body)
	if err != nil && len(reqB) == 0 {
		slog.Debug("Qwen parser rejected: request body unavailable", "path", path, "error", err)
		return *baseSpan, false
	}
	if err != nil {
		slog.Debug("failed to fully read Qwen request body", "error", err)
	}
	req.Body = io.NopCloser(bytes.NewBuffer(reqB))

	respB, err := getResponseBody(resp)
	if err != nil && len(respB) == 0 {
		slog.Debug("Qwen parser response body unavailable; trying strict request-only fallback", "path", path, "error", err)
		if fallbackSpan, ok := qwenStreamRequestSpan(baseSpan, req, isSSEContentType(resp.Header.Get("Content-Type"))); ok {
			if fallbackSpan.GenAI != nil && fallbackSpan.GenAI.Qwen != nil && fallbackSpan.GenAI.Qwen.ID == "" {
				for _, headerName := range []string{"X-DashScope-Request-Id", "X-Request-Id"} {
					if headerValue := strings.TrimSpace(resp.Header.Get(headerName)); headerValue != "" {
						fallbackSpan.GenAI.Qwen.ID = headerValue
						break
					}
				}
			}
			slog.Debug("Qwen parser recovered via strict request-only fallback", "path", path)
			return fallbackSpan, true
		}
		slog.Debug("Qwen parser rejected: response body unavailable", "path", path, "error", err)
		return *baseSpan, false
	}

	slog.Debug("Qwen", "request", string(reqB), "response", string(respB))

	var parsedRequest request.OpenAIInput
	if !unmarshalQwenOpenAIInput(reqB, &parsedRequest) {
		slog.Debug("failed to parse Qwen request")
	}
	if parsedRequest.Model == "" {
		window := reqB
		if len(window) > modelSearchWindow {
			window = window[:modelSearchWindow]
		}
		if matches := modelFieldRegexp.FindSubmatch(window); len(matches) == 2 {
			parsedRequest.Model = strings.TrimSpace(string(matches[1]))
		}
	}

	var parsedResponse request.VendorOpenAI
	if err := json.Unmarshal(respB, &parsedResponse); err != nil {
		if isQwenStreamResponse(resp, respB) {
			var fromAccum *request.VendorOpenAI
			if streamAccum != nil {
				fromAccum = streamAccum.Finalize()
			}
			fromBuf, streamErr := parseQwenStream(bytes.NewReader(respB))
			if streamErr != nil {
				slog.Debug("failed to parse Qwen stream response", "error", streamErr)
			}
			merged := mergeQwenVendorSnapshot(fromAccum, fromBuf)
			if merged != nil {
				parsedResponse = *merged
			}
		} else {
			slog.Debug("failed to parse Qwen response", "error", err)
		}
	}

	if parsedResponse.ID == "" {
		var responseID struct {
			RequestID string `json:"request_id"`
		}
		if err := json.Unmarshal(respB, &responseID); err == nil {
			parsedResponse.ID = responseID.RequestID
		}
	}

	if parsedResponse.ID == "" {
		// Fall back to response headers when body capture is partial/truncated.
		for _, headerName := range []string{"X-DashScope-Request-Id", "X-Request-Id"} {
			if headerValue := strings.TrimSpace(resp.Header.Get(headerName)); headerValue != "" {
				parsedResponse.ID = headerValue
				break
			}
		}
	}

	if parsedResponse.OperationName == "" {
		parsedResponse.OperationName = extractQwenOperation(req)
	}
	if parsedResponse.ResponseModel == "" {
		parsedResponse.ResponseModel = parsedRequest.Model
	}
	if parsedRequest.Model == "" {
		parsedRequest.Model = parsedResponse.ResponseModel
	}

	parsedResponse.Request = parsedRequest

	baseSpan.SubType = request.HTTPSubtypeQwen
	baseSpan.GenAI = &request.GenAI{
		Qwen: &parsedResponse,
	}

	return *baseSpan, true
}

type qwenRequestEnvelope struct {
	request.OpenAIInput
	Stream bool `json:"stream"`
}

// QwenStreamRequestSpan is a strict request-only fallback used when
// response payload is unavailable (e.g. missing large response buffer).
func QwenStreamRequestSpan(baseSpan *request.Span, req *http.Request) (request.Span, bool) {
	return qwenStreamRequestSpan(baseSpan, req, false)
}

func qwenStreamRequestSpan(baseSpan *request.Span, req *http.Request, streamHint bool) (request.Span, bool) {
	if req == nil {
		slog.Debug("Qwen stream fallback rejected: nil request")
		return *baseSpan, false
	}
	path := qwenRequestPath(req)
	if !isQwenStreamRequestPath(req) {
		slog.Debug("Qwen stream fallback rejected: path not in strict allowlist", "path", path)
		return *baseSpan, false
	}

	reqB, err := io.ReadAll(req.Body)
	if err != nil && len(reqB) == 0 {
		slog.Debug("Qwen stream fallback rejected: request body unavailable", "path", path, "error", err)
		return *baseSpan, false
	}
	if err != nil {
		slog.Debug("Qwen stream fallback request body partially read", "path", path, "error", err, "bytes", len(reqB))
	}
	req.Body = io.NopCloser(bytes.NewBuffer(reqB))

	var parsedReq qwenRequestEnvelope
	if !unmarshalQwenRequestEnvelope(reqB, &parsedReq) {
		slog.Debug("failed to parse Qwen stream request fallback")
	}

	isStream := streamHint || parsedReq.Stream || streamFieldRegexp.Match(reqB) ||
		isSSEContentType(req.Header.Get("Accept"))
	if !isStream {
		slog.Debug("Qwen stream fallback rejected: request is not stream", "path", path, "model", parsedReq.Model)
		return *baseSpan, false
	}

	if parsedReq.Model == "" {
		window := reqB
		if len(window) > modelSearchWindow {
			window = window[:modelSearchWindow]
		}
		if matches := modelFieldRegexp.FindSubmatch(window); len(matches) == 2 {
			parsedReq.Model = strings.TrimSpace(string(matches[1]))
		}
	}

	if parsedReq.Model == "" && isDashScopeHost(req) {
		parsedReq.Model = "qwen"
		slog.Debug("Qwen stream fallback defaulted missing model", "path", path, "model", parsedReq.Model)
	}
	if parsedReq.Model != "" && !strings.HasPrefix(strings.ToLower(parsedReq.Model), "qwen") {
		slog.Debug("Qwen stream fallback rejected: model is not qwen", "path", path, "model", parsedReq.Model)
		return *baseSpan, false
	}
	if parsedReq.GetInput() == "" {
		parsedReq.Input = qwenFallbackPayloadUnavailable
		slog.Debug("Qwen stream fallback defaulted missing input", "path", path)
	}

	parsedResp := &request.VendorOpenAI{
		OperationName: extractQwenOperation(req),
		ResponseModel: parsedReq.Model,
		Request:       parsedReq.OpenAIInput,
	}
	if parsedResp.GetOutput() == "" {
		parsedResp.Choices = json.RawMessage(qwenFallbackOutputUnavailableJSON)
		slog.Debug("Qwen stream fallback defaulted missing output", "path", path)
	}

	baseSpan.SubType = request.HTTPSubtypeQwen
	baseSpan.GenAI = &request.GenAI{
		Qwen: parsedResp,
	}
	slog.Debug("Qwen stream fallback accepted", "path", path, "operation", parsedResp.OperationName, "model", parsedResp.ResponseModel)

	return *baseSpan, true
}

func isQwenStreamResponse(resp *http.Response, respBody []byte) bool {
	if resp != nil && strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		return true
	}
	return bytes.HasPrefix(bytes.TrimSpace(respBody), []byte("data:"))
}

type qwenStreamChunk struct {
	ID        string          `json:"id"`
	RequestID string          `json:"request_id"`
	Object    string          `json:"object"`
	Model     string          `json:"model"`
	Usage     json.RawMessage `json:"usage"`
	Output    struct {
		Text string `json:"text"`
	} `json:"output"`
	Choices []struct {
		Text  string `json:"text"`
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Error request.OpenAIError `json:"error"`
}

// QwenStreamAccum incrementally parses Server-Sent Events (data: lines) from
// arbitrary byte chunks so token usage and assistant deltas are merged as soon
// as each complete SSE event is available.
type QwenStreamAccum struct {
	carry         []byte
	currentData   strings.Builder
	response      request.VendorOpenAI
	outputBuilder strings.Builder
	finishReason  string
	finalized     bool
}

// NewQwenStreamAccum constructs an empty SSE accumulator.
func NewQwenStreamAccum() *QwenStreamAccum {
	return &QwenStreamAccum{}
}

// Feed ingests one TCP large-buffer chunk (plaintext HTTP response bytes).
func (a *QwenStreamAccum) Feed(chunk []byte) error {
	if a == nil || len(chunk) == 0 {
		return nil
	}
	buf := append(append([]byte{}, a.carry...), chunk...)
	lastNL := bytes.LastIndexByte(buf, '\n')
	if lastNL < 0 {
		a.carry = buf
		return nil
	}
	complete := buf[:lastNL+1]
	a.carry = append([]byte{}, buf[lastNL+1:]...)

	for _, line := range bytes.Split(complete, []byte("\n")) {
		line = bytes.TrimSuffix(line, []byte("\r"))
		a.feedLine(string(line))
	}
	return nil
}

// ReadFrom parses a full response body stream (same semantics as Feed).
func (a *QwenStreamAccum) ReadFrom(r io.Reader) error {
	if a == nil {
		return nil
	}
	br := bufio.NewReaderSize(r, 64*1024)
	maxLine := qwenStreamMaxLineBytes()
	for {
		line, err := readLimitedLine(br, maxLine)
		if err != nil && err != io.EOF {
			return err
		}
		line = strings.TrimSuffix(line, "\r")
		a.feedLine(line)
		if err == io.EOF {
			break
		}
	}
	return nil
}

func (a *QwenStreamAccum) feedLine(line string) {
	if line == "" {
		a.flushCurrentData()
		return
	}
	if !strings.HasPrefix(line, "data: ") {
		return
	}
	payload := strings.TrimSpace(strings.TrimPrefix(line, "data: "))
	if a.currentData.Len() > 0 {
		a.currentData.WriteByte('\n')
	}
	a.currentData.WriteString(payload)
}

func (a *QwenStreamAccum) flushCurrentData() {
	if a.currentData.Len() == 0 {
		return
	}
	data := strings.TrimSpace(a.currentData.String())
	a.currentData.Reset()
	_ = processQwenStreamData(data, &a.response, &a.outputBuilder, &a.finishReason)
}

// Finalize flushes partial lines and returns the merged VendorOpenAI snapshot.
// Call at most once per accumulator.
func (a *QwenStreamAccum) Finalize() *request.VendorOpenAI {
	if a == nil || a.finalized {
		if a == nil {
			return nil
		}
		out := a.response
		a.materializeChoices(&out)
		return &out
	}
	a.finalized = true
	if len(a.carry) > 0 {
		rest := strings.TrimSpace(string(bytes.TrimSuffix(a.carry, []byte("\r"))))
		a.carry = nil
		if rest != "" {
			a.feedLine(rest)
		}
	}
	a.flushCurrentData()
	out := a.response
	a.materializeChoices(&out)
	return &out
}

func (a *QwenStreamAccum) materializeChoices(out *request.VendorOpenAI) {
	if a.outputBuilder.Len() == 0 {
		return
	}
	choicePayload, _ := json.Marshal([]map[string]any{
		{
			"message": map[string]string{
				"role":    "assistant",
				"content": a.outputBuilder.String(),
			},
			"finish_reason": a.finishReason,
		},
	})
	out.Choices = choicePayload
}

func qwenStreamMaxLineBytes() int {
	const capMax = 1 << 22 // 4 MiB — SSE lines can exceed the capture window for huge tool payloads.
	n := config.MaxCapturedPayloadBytes
	if n <= 0 {
		return capMax
	}
	// Allow several times the configured capture limit per line so a single JSON
	// chunk is not truncated while the overall response is still bounded by eBPF.
	wide := n * 8
	if wide < 256*1024 {
		wide = 256 * 1024
	}
	if wide > capMax {
		return capMax
	}
	return wide
}

func readLimitedLine(br *bufio.Reader, max int) (string, error) {
	var b strings.Builder
	for {
		c, err := br.ReadByte()
		if err == io.EOF {
			if b.Len() == 0 {
				return "", io.EOF
			}
			return b.String(), io.EOF
		}
		if err != nil {
			return "", err
		}
		if c == '\n' {
			return b.String(), nil
		}
		if b.Len() >= max {
			// Skip remainder of line to recover; partial SSE events are dropped.
			for {
				c2, err2 := br.ReadByte()
				if err2 != nil {
					return b.String(), err2
				}
				if c2 == '\n' {
					slog.Debug("Qwen SSE line exceeded limit; skipped remainder", "max", max)
					return b.String(), nil
				}
			}
		}
		b.WriteByte(c)
	}
}

func parseQwenStream(reader io.Reader) (*request.VendorOpenAI, error) {
	acc := NewQwenStreamAccum()
	if err := acc.ReadFrom(reader); err != nil {
		return nil, fmt.Errorf("error reading stream: %w", err)
	}
	return acc.Finalize(), nil
}

func mergeQwenVendorSnapshot(fromAccum, fromBuf *request.VendorOpenAI) *request.VendorOpenAI {
	switch {
	case fromAccum == nil:
		return fromBuf
	case fromBuf == nil:
		return fromAccum
	}
	out := *fromBuf
	mergeQwenUsagePreferMax(&out.Usage, &fromAccum.Usage)
	if len(out.GetOutput()) < len(fromAccum.GetOutput()) {
		out.Choices = fromAccum.Choices
	}
	if out.ID == "" {
		out.ID = fromAccum.ID
	}
	if out.ResponseModel == "" {
		out.ResponseModel = fromAccum.ResponseModel
	}
	if out.OperationName == "" {
		out.OperationName = fromAccum.OperationName
	}
	if out.Error.Type == "" && fromAccum.Error.Type != "" {
		out.Error = fromAccum.Error
	}
	return &out
}

func processQwenStreamData(
	data string,
	response *request.VendorOpenAI,
	outputBuilder *strings.Builder,
	finishReason *string,
) error {
	if data == "" || data == "[DONE]" {
		return nil
	}

	var chunk qwenStreamChunk
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		slog.Debug("skipping unparsable Qwen SSE JSON payload", "error", err)
		return nil
	}

	if response.ID == "" {
		response.ID = chunk.ID
	}
	if response.ID == "" {
		response.ID = chunk.RequestID
	}
	if response.OperationName == "" {
		response.OperationName = normalizeQwenOperationName(chunk.Object)
	}
	if response.ResponseModel == "" {
		response.ResponseModel = chunk.Model
	}
	if response.Error.Type == "" && chunk.Error.Type != "" {
		response.Error = chunk.Error
	}

	mergeQwenUsageFromRaw(&response.Usage, chunk.Usage)

	if chunk.Output.Text != "" {
		outputBuilder.WriteString(chunk.Output.Text)
	}
	for _, choice := range chunk.Choices {
		switch {
		case choice.Delta.Content != "":
			outputBuilder.WriteString(choice.Delta.Content)
		case choice.Message.Content != "":
			outputBuilder.WriteString(choice.Message.Content)
		case choice.Text != "":
			outputBuilder.WriteString(choice.Text)
		}
		if choice.FinishReason != "" {
			*finishReason = choice.FinishReason
		}
	}

	return nil
}

func normalizeQwenOperationName(object string) string {
	switch object {
	case "chat.completion.chunk":
		return "chat.completion"
	default:
		return object
	}
}

func mergeQwenUsageFromRaw(dst *request.OpenAIUsage, raw json.RawMessage) {
	if dst == nil || len(raw) == 0 || string(raw) == "null" {
		return
	}
	var m map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		slog.Debug("Qwen stream usage object not decoded", "error", err)
		return
	}
	merge := func(key string, target *int) {
		v, ok := m[key]
		if !ok {
			return
		}
		n := anyToInt(v)
		if n > *target {
			*target = n
		}
	}
	merge("input_tokens", &dst.InputTokens)
	merge("output_tokens", &dst.OutputTokens)
	merge("total_tokens", &dst.TotalTokens)
	merge("prompt_tokens", &dst.PromptTokens)
	merge("completion_tokens", &dst.CompletionTokens)
}

func mergeQwenUsagePreferMax(dst, src *request.OpenAIUsage) {
	if dst == nil || src == nil {
		return
	}
	if src.InputTokens > dst.InputTokens {
		dst.InputTokens = src.InputTokens
	}
	if src.OutputTokens > dst.OutputTokens {
		dst.OutputTokens = src.OutputTokens
	}
	if src.TotalTokens > dst.TotalTokens {
		dst.TotalTokens = src.TotalTokens
	}
	if src.PromptTokens > dst.PromptTokens {
		dst.PromptTokens = src.PromptTokens
	}
	if src.CompletionTokens > dst.CompletionTokens {
		dst.CompletionTokens = src.CompletionTokens
	}
}

func anyToInt(v any) int {
	switch x := v.(type) {
	case json.Number:
		i, err := x.Int64()
		if err != nil {
			f, err2 := x.Float64()
			if err2 != nil {
				return 0
			}
			return int(f)
		}
		return int(i)
	case float64:
		return int(x)
	case int:
		return x
	case int64:
		return int(x)
	default:
		return 0
	}
}

func isQwenStreamRequestPath(req *http.Request) bool {
	op := extractQwenOperation(req)
	if op != "chat.completion" && op != "completion" && op != "generation" {
		return false
	}
	path := qwenRequestPath(req)
	return strings.Contains(path, "/compatible-mode/") ||
		strings.Contains(path, "/services/aigc/")
}

func isDashScopeHost(req *http.Request) bool {
	if req == nil {
		return false
	}
	host := req.Host
	if req.URL != nil && req.URL.Host != "" {
		host = req.URL.Host
	}
	return strings.Contains(strings.ToLower(host), "dashscope.aliyuncs.com")
}

func isSSEContentType(contentType string) bool {
	return strings.Contains(strings.ToLower(contentType), "text/event-stream")
}

func unmarshalQwenOpenAIInput(raw []byte, out *request.OpenAIInput) bool {
	if err := json.Unmarshal(raw, out); err == nil {
		return true
	}
	candidate, ok := extractLikelyJSONPayload(raw)
	if !ok {
		return false
	}
	if err := json.Unmarshal(candidate, out); err == nil {
		slog.Debug("Qwen request parse recovered from wrapped bytes", "bytes", len(candidate))
		return true
	}
	return false
}

func unmarshalQwenRequestEnvelope(raw []byte, out *qwenRequestEnvelope) bool {
	if err := json.Unmarshal(raw, out); err == nil {
		return true
	}
	candidate, ok := extractLikelyJSONPayload(raw)
	if !ok {
		return false
	}
	if err := json.Unmarshal(candidate, out); err == nil {
		slog.Debug("Qwen stream request parse recovered from wrapped bytes", "bytes", len(candidate))
		return true
	}
	return false
}

func extractLikelyJSONPayload(raw []byte) ([]byte, bool) {
	start := bytes.IndexByte(raw, '{')
	if start < 0 {
		return nil, false
	}
	end := bytes.LastIndexByte(raw, '}')
	if end <= start {
		return nil, false
	}
	payload := bytes.TrimSpace(raw[start : end+1])
	if len(payload) == 0 {
		return nil, false
	}
	return payload, true
}

func extractQwenOperation(req *http.Request) string {
	if req == nil {
		return "generation"
	}

	path := qwenRequestPath(req)
	switch {
	case strings.Contains(path, "/chat/completions"):
		return "chat.completion"
	case strings.Contains(path, "/completions"):
		return "completion"
	case strings.Contains(path, "/embeddings"):
		return "embedding"
	case strings.Contains(path, "/generation"):
		return "generation"
	default:
		return "generation"
	}
}
