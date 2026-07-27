// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package ebpfcommon // import "go.opentelemetry.io/obi/pkg/ebpf/common/http"

import (
	"bytes"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"

	"go.opentelemetry.io/obi/pkg/appolly/app/request"
	"go.opentelemetry.io/obi/pkg/config"
)

// diagTailStr returns the last n bytes of b as a string, for OBI_DIAG logging.
func diagTailStr(b []byte, n int) string {
	if len(b) > n {
		b = b[len(b)-n:]
	}
	return string(b)
}

func OpenAICompatibleSpan(baseSpan *request.Span, req *http.Request, resp *http.Response, gateways []config.OpenAICompatibleGateway) (request.Span, bool) {
	var reqHost string
	if req.URL != nil {
		reqHost = req.URL.Host
	}
	if reqHost == "" {
		reqHost = req.Host
	}

	hostOnly := reqHost
	port := 0
	if h, p, err := net.SplitHostPort(reqHost); err == nil {
		hostOnly = h
		if pInt, err := strconv.Atoi(p); err == nil {
			port = pInt
		}
	}

	var matchedGateway *config.OpenAICompatibleGateway
	for i := range gateways {
		gw := &gateways[i]
		if !strings.EqualFold(hostOnly, gw.Host) {
			continue
		}
		if gw.Port > 0 && port > 0 && gw.Port != port {
			continue
		}
		matchedGateway = gw
		break
	}

	if matchedGateway == nil {
		return *baseSpan, false
	}

	reqB, ok := readHTTPRequestBody("OpenAICompatibleSpan", req, baseSpan)
	if !ok {
		return *baseSpan, false
	}

	respB, ok := readHTTPResponseBody("OpenAICompatibleSpan", resp, baseSpan)
	if !ok {
		return *baseSpan, false
	}

	parsedRequest := parseOpenAIInput(reqB)
	parsedResponse, toolCalls := parseOpenAICompatibleResponse(respB)

	// OBI_DIAG: what the parser received (the de-chunked response body) and what
	// it extracted. Compare against the raw-capture OBI_DIAG in HTTPInfoEventToSpan
	// to localize the usage=0 cause: capture-missing vs de-chunk-lost vs parse-fail.
	{
		inTok, inOK := parsedResponse.Usage.InputTokenCount()
		outTok, outOK := parsedResponse.Usage.OutputTokenCount()
		slog.Info("OBI_DIAG openai_compat",
			"host", hostOnly,
			"respLen", len(respB),
			"bodyHasUsage", bytes.Contains(respB, []byte("usage")),
			"bodyHasDONE", bytes.Contains(respB, []byte("[DONE]")),
			"bodyHasPromptTok", bytes.Contains(respB, []byte("prompt_tokens")),
			"inTok", inTok, "inOK", inOK, "outTok", outTok, "outOK", outOK,
			"respTail", diagTailStr(respB, 220))
	}

	if parsedResponse.ResponseModel == "" && len(parsedResponse.Choices) == 0 &&
		!hasOpenAIUsage(parsedResponse.Usage) && len(parsedResponse.Data) == 0 &&
		len(parsedResponse.Output) == 0 && parsedRequest.Model == "" {
		return *baseSpan, false
	}

	if parsedResponse.ResponseModel == "" {
		parsedResponse.ResponseModel = parsedRequest.Model
	}
	if parsedRequest.Model == "" {
		parsedRequest.Model = parsedResponse.ResponseModel
	}

	parsedResponse.Request = parsedRequest
	parsedResponse.ToolCalls = toolCalls

	// Use strings.Contains instead of exact path matching to support
	// gateways mounted under a path prefix (e.g. /litellm/v1/chat/completions).
	if req.URL != nil {
		switch {
		case strings.Contains(req.URL.Path, "/v1/chat/completions"):
			parsedResponse.OperationName = request.ChatOperationName
			parsedResponse.APIType = "chat_completions"
		case strings.Contains(req.URL.Path, "/v1/completions"):
			parsedResponse.OperationName = request.CompletionOperationName
			parsedResponse.APIType = "text_completions"
		case strings.Contains(req.URL.Path, "/v1/embeddings"):
			parsedResponse.OperationName = request.EmbeddingOperationName
			parsedResponse.APIType = "embeddings"
		case strings.Contains(req.URL.Path, "/v1/responses"):
			parsedResponse.APIType = "responses"
		}
	}

	parsedResponse.ProviderName = matchedGateway.Provider
	baseSpan.SubType = request.HTTPSubtypeOpenAICompatible
	baseSpan.GenAI = &request.GenAI{
		OpenAICompatible: parsedResponse,
	}

	return *baseSpan, true
}

func hasOpenAIUsage(usage request.OpenAIUsage) bool {
	counts := []request.TokenCount{
		usage.InputTokens,
		usage.OutputTokens,
		usage.TotalTokens,
		usage.PromptTokens,
		usage.CompletionTokens,
	}
	if usage.InputDetails != nil {
		counts = append(counts,
			usage.InputDetails.CachedTokens,
			usage.InputDetails.CacheCreationTokens,
			usage.InputDetails.AudioTokens,
		)
	}
	if usage.OutputDetails != nil {
		counts = append(counts,
			usage.OutputDetails.ReasoningTokens,
			usage.OutputDetails.AudioTokens,
			usage.OutputDetails.AcceptedPredictionTokens,
			usage.OutputDetails.RejectedPredictionTokens,
		)
	}
	for _, count := range counts {
		if _, reported := count.Get(); reported {
			return true
		}
	}
	return false
}
