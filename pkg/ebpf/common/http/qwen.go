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
)

// modelFieldRegexp extracts the top-level "model" value from a (possibly
// truncated) JSON request body.  It is a best-effort fallback used only when
// json.Unmarshal cannot parse the body.  We limit the search window to
// modelSearchWindow bytes so that we don't accidentally match a "model"
// key buried inside a user prompt or message content.
var modelFieldRegexp = regexp.MustCompile(`"model"\s*:\s*"([^"]+)"`)
var streamFieldRegexp = regexp.MustCompile(`"stream"\s*:\s*true\b`)

const modelSearchWindow = 200

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

func QwenSpan(baseSpan *request.Span, req *http.Request, resp *http.Response) (request.Span, bool) {
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
		slog.Debug("Qwen parser rejected: response body unavailable", "path", path, "error", err)
		return *baseSpan, false
	}

	slog.Debug("Qwen", "request", string(reqB), "response", string(respB))

	var parsedRequest request.OpenAIInput
	if err := json.Unmarshal(reqB, &parsedRequest); err != nil {
		slog.Debug("failed to parse Qwen request", "error", err)
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
			if streamResponse, streamErr := parseQwenStream(bytes.NewReader(respB)); streamErr == nil {
				parsedResponse = *streamResponse
			} else {
				slog.Debug("failed to parse Qwen stream response", "error", streamErr)
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
	if err := json.Unmarshal(reqB, &parsedReq); err != nil {
		slog.Debug("failed to parse Qwen stream request fallback", "error", err)
	}

	isStream := parsedReq.Stream || streamFieldRegexp.Match(reqB)
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

	if !strings.HasPrefix(strings.ToLower(parsedReq.Model), "qwen") {
		slog.Debug("Qwen stream fallback rejected: model is not qwen", "path", path, "model", parsedReq.Model)
		return *baseSpan, false
	}

	parsedResp := &request.VendorOpenAI{
		OperationName: extractQwenOperation(req),
		ResponseModel: parsedReq.Model,
		Request:       parsedReq.OpenAIInput,
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
	ID        string `json:"id"`
	RequestID string `json:"request_id"`
	Object    string `json:"object"`
	Model     string `json:"model"`
	Usage     struct {
		InputTokens      int `json:"input_tokens"`
		OutputTokens     int `json:"output_tokens"`
		TotalTokens      int `json:"total_tokens"`
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Output struct {
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

func parseQwenStream(reader io.Reader) (*request.VendorOpenAI, error) {
	scanner := bufio.NewScanner(reader)
	response := &request.VendorOpenAI{}
	var outputBuilder strings.Builder
	var finishReason string
	var currentData strings.Builder

	processCurrentData := func() error {
		if currentData.Len() == 0 {
			return nil
		}
		defer currentData.Reset()
		return processQwenStreamData(strings.TrimSpace(currentData.String()), response, &outputBuilder, &finishReason)
	}

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := processCurrentData(); err != nil {
				return nil, err
			}
			continue
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		if currentData.Len() > 0 {
			currentData.WriteByte('\n')
		}
		currentData.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data: ")))
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("error reading stream: %w", err)
	}
	if err := processCurrentData(); err != nil {
		return nil, err
	}

	if outputBuilder.Len() > 0 {
		choicePayload, _ := json.Marshal([]map[string]any{
			{
				"message": map[string]string{
					"role":    "assistant",
					"content": outputBuilder.String(),
				},
				"finish_reason": finishReason,
			},
		})
		response.Choices = choicePayload
	}

	return response, nil
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
		return fmt.Errorf("error parsing stream data: %w", err)
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

	mergeQwenUsage(&response.Usage, chunk.Usage)

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

func mergeQwenUsage(dst *request.OpenAIUsage, src struct {
	InputTokens      int `json:"input_tokens"`
	OutputTokens     int `json:"output_tokens"`
	TotalTokens      int `json:"total_tokens"`
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}) {
	if src.InputTokens > 0 {
		dst.InputTokens = src.InputTokens
	}
	if src.OutputTokens > 0 {
		dst.OutputTokens = src.OutputTokens
	}
	if src.TotalTokens > 0 {
		dst.TotalTokens = src.TotalTokens
	}
	if src.PromptTokens > 0 {
		dst.PromptTokens = src.PromptTokens
	}
	if src.CompletionTokens > 0 {
		dst.CompletionTokens = src.CompletionTokens
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
