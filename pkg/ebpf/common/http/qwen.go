// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package ebpfcommon // import "go.opentelemetry.io/obi/pkg/ebpf/common/http"

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"

	"go.opentelemetry.io/obi/pkg/appolly/app/request"
)

var modelFieldRegexp = regexp.MustCompile(`"model"\s*:\s*"([^"]+)"`)

func isQwen(respHeader http.Header, req *http.Request) bool {
	if respHeader.Get("X-DashScope-Request-Id") != "" {
		return true
	}
	if respHeader.Get("X-Dashscope-Call-Gateway") != "" {
		return true
	}

	if req == nil || req.URL == nil {
		return false
	}

	path := req.URL.Path
	if strings.Contains(path, "/compatible-mode/v1/") ||
		strings.Contains(path, "/api/v1/services/aigc/") {
		// Keep path-based fallback to handle truncated/missing headers.
		// Host can also be partially captured and become non-empty garbage,
		// so don't gate detection on host once a DashScope path is observed.
		return true
	}

	return false
}

func QwenSpan(baseSpan *request.Span, req *http.Request, resp *http.Response) (request.Span, bool) {
	if !isQwen(resp.Header, req) {
		return *baseSpan, false
	}

	reqB, err := io.ReadAll(req.Body)
	if err != nil && len(reqB) == 0 {
		return *baseSpan, false
	}
	if err != nil {
		slog.Debug("failed to fully read Qwen request body", "error", err)
	}
	req.Body = io.NopCloser(bytes.NewBuffer(reqB))

	respB, err := getResponseBody(resp)
	if err != nil && len(respB) == 0 {
		return *baseSpan, false
	}

	slog.Debug("Qwen", "request", string(reqB), "response", string(respB))

	var parsedRequest request.OpenAIInput
	if err := json.Unmarshal(reqB, &parsedRequest); err != nil {
		slog.Debug("failed to parse Qwen request", "error", err)
	}
	if parsedRequest.Model == "" {
		if matches := modelFieldRegexp.FindSubmatch(reqB); len(matches) == 2 {
			parsedRequest.Model = strings.TrimSpace(string(matches[1]))
		}
	}

	var parsedResponse request.VendorOpenAI
	if err := json.Unmarshal(respB, &parsedResponse); err != nil {
		slog.Debug("failed to parse Qwen response", "error", err)
	}

	if parsedResponse.ID == "" {
		// Prefer response headers when body capture is partial/truncated.
		for _, headerName := range []string{"X-DashScope-Request-Id", "X-Request-Id"} {
			if headerValue := strings.TrimSpace(resp.Header.Get(headerName)); headerValue != "" {
				parsedResponse.ID = headerValue
				break
			}
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

func extractQwenOperation(req *http.Request) string {
	if req == nil || req.URL == nil {
		return "generation"
	}

	path := req.URL.Path
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
