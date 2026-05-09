// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package tracesgen

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.opentelemetry.io/otel/attribute"
	semconv "go.opentelemetry.io/otel/semconv/v1.38.0"

	"go.opentelemetry.io/obi/pkg/appolly/app/request"
	attr "go.opentelemetry.io/obi/pkg/export/attributes/names"
)

func TestTraceAttributesSelector_OpenAIChatCompletionsAddsMaxTokensAndFinishReasons(t *testing.T) {
	span := &request.Span{
		Type:    request.EventTypeHTTPClient,
		SubType: request.HTTPSubtypeOpenAI,
		Method:  "POST",
		Path:    "https://api.openai.com/v1/chat/completions",
		Status:  200,
		GenAI: &request.GenAI{
			OpenAI: &request.VendorOpenAI{
				ID:            "chatcmpl-123",
				OperationName: "chat.completion",
				ResponseModel: "gpt-4o-mini-2024-07-18",
				Choices:       []byte(`[{"finish_reason":"stop"}]`),
				Usage:         request.OpenAIUsage{PromptTokens: 5, CompletionTokens: 7},
				Request: request.OpenAIInput{
					Model:               "gpt-4o-mini",
					MaxCompletionTokens: 128,
				},
			},
		},
	}

	attrs := TraceAttributesSelector(span, map[attr.Name]struct{}{attr.GenAIOutput: {}})

	require.Contains(t, attrs, semconv.GenAIRequestMaxTokens(128))

	for _, kv := range attrs {
		if kv.Key == semconv.GenAIResponseFinishReasonsKey {
			assert.Equal(t, attribute.STRINGSLICE, kv.Value.Type())
			assert.Equal(t, []string{"stop"}, kv.Value.AsStringSlice())
			return
		}
	}

	t.Fatalf("missing %s attribute", semconv.GenAIResponseFinishReasonsKey)
}
