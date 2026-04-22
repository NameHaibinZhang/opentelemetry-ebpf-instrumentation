// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package ebpfcommon // import "go.opentelemetry.io/obi/pkg/ebpf/common/http"

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"go.opentelemetry.io/obi/pkg/appolly/app/request"
)

var modelFieldRegexp = regexp.MustCompile(`"model"\s*:\s*"([^"]+)"`)

func qwenRequestHost(req *http.Request) string {
	if req == nil {
		return ""
	}
	if req.URL != nil {
		if h := req.URL.Hostname(); h != "" {
			return h
		}
	}
	host := req.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}

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

func isQwen(respHeader http.Header, req *http.Request) bool {
	if respHeader.Get("X-DashScope-Request-Id") != "" {
		return true
	}
	if respHeader.Get("X-Dashscope-Call-Gateway") != "" {
		return true
	}

	if req == nil {
		return false
	}

	path := qwenRequestPath(req)
	if strings.Contains(path, "/compatible-mode/v1/") ||
		strings.Contains(path, "/api/v1/services/aigc/") {
		// Keep path-based fallback to handle truncated/missing headers.
		// Host can also be partially captured and become non-empty garbage,
		// so don't gate detection on host once a DashScope path is observed.
		return true
	}
	if strings.Contains(strings.ToLower(qwenRequestHost(req)), "dashscope") {
		// Host-based fallback for cases where URI path is not reconstructed.
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
