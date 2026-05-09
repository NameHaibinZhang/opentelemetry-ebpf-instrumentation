// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package ebpfcommon // import "go.opentelemetry.io/obi/pkg/ebpf/common/http"

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"go.opentelemetry.io/obi/pkg/appolly/app/request"
)

func OpenAISpan(baseSpan *request.Span, req *http.Request, resp *http.Response) (request.Span, bool) {
	// Check any of the well known response headers that OpenAI would use
	isOpenAI := false
	for _, header := range []string{"Openai-Version", "Openai-Organization", "Openai-Project", "Openai-Processing-Ms"} {
		if val := resp.Header.Get(header); val != "" {
			isOpenAI = true
			break
		}
	}

	if !isOpenAI {
		return *baseSpan, false
	}

	reqB, err := getRequestBody(req)
	if err != nil && len(reqB) == 0 {
		return *baseSpan, false
	}
	if err != nil {
		slog.Debug("failed to fully read OpenAI request body", "error", err)
	}

	respB, err := getResponseBody(resp)
	if err != nil && len(respB) == 0 {
		return *baseSpan, false
	}

	slog.Debug("OpenAI", "request", string(reqB), "response", string(respB))

	var parsedRequest request.OpenAIInput
	if err := json.Unmarshal(reqB, &parsedRequest); err != nil {
		slog.Debug("failed to parse OpenAI request", "error", err)
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
		slog.Debug("failed to parse OpenAI response", "error", err)
	}

	if operationName := extractOpenAIOperation(req); operationName != "" &&
		(parsedResponse.OperationName == "" || operationName == request.EmbeddingOperationName) {
		parsedResponse.OperationName = operationName
	}
	if parsedResponse.ResponseModel == "" {
		parsedResponse.ResponseModel = parsedRequest.Model
	}
	if parsedRequest.Model == "" {
		parsedRequest.Model = parsedResponse.ResponseModel
	}

	parsedResponse.Request = parsedRequest

	baseSpan.SubType = request.HTTPSubtypeOpenAI
	baseSpan.GenAI = &request.GenAI{
		OpenAI: &parsedResponse,
	}

	return *baseSpan, true
}

func extractOpenAIOperation(req *http.Request) string {
	path := strings.TrimSuffix(requestPath(req), "/")
	switch {
	case strings.HasSuffix(path, "/chat/completions"):
		return "chat.completion"
	case strings.HasSuffix(path, "/responses"):
		return "response"
	case strings.HasSuffix(path, "/embeddings"):
		return request.EmbeddingOperationName
	case strings.HasSuffix(path, "/conversations"):
		return "conversation"
	default:
		return ""
	}
}
