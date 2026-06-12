// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package ebpfcommon // import "go.opentelemetry.io/obi/pkg/ebpf/common/http"

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"go.opentelemetry.io/obi/pkg/appolly/app/request"
)

func isQwen(respHeader http.Header) bool {
	for _, header := range []string{"X-DashScope-Request-Id", "X-Dashscope-Call-Gateway"} {
		if val := respHeader.Get(header); val != "" {
			return true
		}
	}
	return false
}

func QwenSpan(baseSpan *request.Span, req *http.Request, resp *http.Response) (request.Span, bool) {
	headerDetected := isQwen(resp.Header)

	// Fast exit: not detected by headers and URL doesn't match
	if !headerDetected && !isQwenCompatibleURL(req) {
		return *baseSpan, false
	}

	reqB, ok := readHTTPRequestBody("QwenSpan", req, baseSpan)
	if !ok {
		return *baseSpan, false
	}

	// If not detected by headers, verify model name starts with "qwen"
	if !headerDetected {
		model := extractModelField(reqB)
		if !strings.HasPrefix(strings.ToLower(model), "qwen") {
			return *baseSpan, false
		}
	}

	respB, ok := readHTTPResponseBody("QwenSpan", resp, baseSpan)
	if !ok {
		return *baseSpan, false
	}

	slog.Debug("Qwen",
		"reqBodyLen", len(reqB),
		"respBodyLen", len(respB),
		"request", string(reqB),
	)

	parsedRequest := parseOpenAIInput(reqB)
	slog.Debug("Qwen parsed request",
		"model", parsedRequest.Model,
		"hasMessages", len(parsedRequest.Messages) > 0,
		"stream", parsedRequest.Stream,
	)
	var parsedResponse request.VendorOpenAI
	var toolCalls []request.ToolCall

	if len(respB) > 0 && respB[0] == '{' {
		parsedResponse = parseVendorOpenAI(respB)
		toolCalls = extractToolCalls(parsedResponse.Choices)

		// Qwen-specific: try to extract request_id from response body
		if parsedResponse.ID == "" {
			var responseID struct {
				RequestID string `json:"request_id"`
			}
			if err := json.Unmarshal(respB, &responseID); err == nil {
				parsedResponse.ID = responseID.RequestID
			}
		}
	} else {
		// SSE stream response (Qwen uses OpenAI-compatible SSE format)
		reader := bytes.NewReader(respB)
		if streamResponse, tc, err := parseOpenAIStream(reader); err == nil {
			parsedResponse = *streamResponse
			toolCalls = tc
		}
	}

	// Fallback: try to get request ID from response headers
	if parsedResponse.ID == "" {
		for _, headerName := range []string{"X-DashScope-Request-Id", "X-Request-Id"} {
			if headerValue := strings.TrimSpace(resp.Header.Get(headerName)); headerValue != "" {
				parsedResponse.ID = headerValue
				break
			}
		}
	}

	parsedResponse.OperationName = extractQwenOperation(req)
	if parsedResponse.ResponseModel == "" {
		parsedResponse.ResponseModel = parsedRequest.Model
	}
	if parsedRequest.Model == "" {
		parsedRequest.Model = parsedResponse.ResponseModel
	}

	slog.Debug("Qwen parsed response",
		"id", parsedResponse.ID,
		"model", parsedResponse.ResponseModel,
		"inputTokens", parsedResponse.Usage.GetInputTokens(),
		"outputTokens", parsedResponse.Usage.GetOutputTokens(),
		"hasChoices", len(parsedResponse.Choices) > 0,
		"hasMessages", len(parsedRequest.Messages) > 0,
	)

	parsedResponse.Request = parsedRequest
	parsedResponse.ToolCalls = toolCalls

	baseSpan.SubType = request.HTTPSubtypeQwen
	baseSpan.GenAI = &request.GenAI{
		Qwen: &parsedResponse,
	}

	return *baseSpan, true
}

// isQwenCompatibleURL checks if the request targets a standard
// OpenAI-compatible endpoint that might serve Qwen models.
func isQwenCompatibleURL(req *http.Request) bool {
	if req == nil {
		return false
	}
	path := requestPath(req)
	return strings.Contains(path, "/chat/completions") ||
		strings.Contains(path, "/completions") ||
		strings.Contains(path, "/generation")
}

func extractQwenOperation(req *http.Request) string {
	if req == nil {
		return request.GenerationOperationName
	}

	path := requestPath(req)
	switch {
	case strings.Contains(path, "/chat/completions"):
		return request.ChatOperationName
	case strings.Contains(path, "/completions"):
		return request.CompletionOperationName
	case strings.Contains(path, "/embeddings"):
		return request.EmbeddingOperationName
	case strings.Contains(path, "/generation"):
		return request.GenerationOperationName
	default:
		return request.GenerationOperationName
	}
}
