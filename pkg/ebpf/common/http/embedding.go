// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package ebpfcommon // import "go.opentelemetry.io/obi/pkg/ebpf/common/http"

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"go.opentelemetry.io/obi/pkg/appolly/app/request"
)

// embeddingHostPattern pairs a known hostname suffix with a required URL path
// prefix and the provider name to assign when matched.
type embeddingHostPattern struct {
	hostSuffix string
	pathPrefix string
	provider   string
}

// embeddingHostPatterns lists known embedding API hosts and their required
// URL path prefixes. Matching is performed by hostname suffix and path prefix.
var embeddingHostPatterns = []embeddingHostPattern{
	{"api.voyageai.com", "/v1/embeddings", "voyage"},
	{"api.cohere.com", "/v2/embed", "cohere"},
	{"api.jina.ai", "/v1/embeddings", "jina"},
}

// isEmbeddingProvider checks whether the request targets a known embedding-only
// provider by matching the hostname and URL path against embeddingHostPatterns.
// Returns the provider name if matched, or empty string otherwise.
func isEmbeddingProvider(req *http.Request) string {
	if req == nil || req.URL == nil {
		return ""
	}

	host := extractHostname(req)
	path := req.URL.Path

	for _, hp := range embeddingHostPatterns {
		if strings.HasSuffix(host, hp.hostSuffix) &&
			strings.HasPrefix(path, hp.pathPrefix) {
			return hp.provider
		}
	}

	return ""
}

// EmbeddingSpan detects embedding API calls to Voyage AI, Cohere, and Jina AI
// based on hostname and URL path matching, and extracts embedding-specific
// fields into the span.
func EmbeddingSpan(baseSpan *request.Span, req *http.Request, resp *http.Response) (request.Span, bool) {
	provider := isEmbeddingProvider(req)
	if provider == "" {
		return *baseSpan, false
	}

	reqB, err := io.ReadAll(req.Body)
	if err != nil {
		return *baseSpan, false
	}
	req.Body = io.NopCloser(bytes.NewBuffer(reqB))

	respB, err := getResponseBody(resp)
	if err != nil && len(respB) == 0 {
		return *baseSpan, false
	}

	slog.Debug("Embedding", "provider", provider, "request", string(reqB), "response", string(respB))

	var parsedRequest request.EmbeddingRequest
	if err := json.Unmarshal(reqB, &parsedRequest); err != nil {
		slog.Debug("failed to parse embedding request", "provider", provider, "error", err)
	}

	var parsedResponse request.EmbeddingResponse
	if len(respB) > 0 {
		if err := json.Unmarshal(respB, &parsedResponse); err != nil {
			slog.Debug("failed to parse embedding response", "provider", provider, "error", err)
		}
	}

	model := parsedRequest.Model
	if model == "" {
		model = parsedResponse.Model
	}

	baseSpan.SubType = request.HTTPSubtypeEmbedding
	baseSpan.GenAI = &request.GenAI{
		Embedding: &request.VendorEmbedding{
			Provider: provider,
			Model:    model,
			Input:    parsedRequest,
			Output:   parsedResponse,
		},
	}

	return *baseSpan, true
}
