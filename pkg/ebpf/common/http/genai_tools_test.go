// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package ebpfcommon

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.opentelemetry.io/obi/pkg/appolly/app/request"
)

func TestExtractOpenAIToolCallsFromResponse_ChatCompletions(t *testing.T) {
	body := `{
  "choices": [{
    "message": {
      "tool_calls": [{
        "id": "call_abc",
        "type": "function",
        "function": {"name": "get_weather", "arguments": "{\"city\":\"NYC\"}"}
      }]
    }
  }]
}`
	calls := extractOpenAIToolCallsFromResponse([]byte(body))
	require.Len(t, calls, 1)
	assert.Equal(t, "call_abc", calls[0].CallID)
	assert.Equal(t, "get_weather", calls[0].Name)
	assert.JSONEq(t, `{"city":"NYC"}`, string(calls[0].Arguments))
}

func TestExtractOpenAIToolCallsFromResponse_ResponsesAPI(t *testing.T) {
	body := `{
  "output": [
    {
      "type": "function_call",
      "id": "fc_1",
      "call_id": "call_xyz",
      "name": "lookup",
      "arguments": "{\"q\":\"hi\"}"
    }
  ]
}`
	calls := extractOpenAIToolCallsFromResponse([]byte(body))
	require.Len(t, calls, 1)
	assert.Equal(t, "call_xyz", calls[0].CallID)
	assert.Equal(t, "lookup", calls[0].Name)
}

func TestExtractOpenAIToolResultsFromRequest(t *testing.T) {
	body := `{
  "messages": [
    {"role": "tool", "tool_call_id": "call_1", "content": "sunny"}
  ],
  "input": [
    {"type": "function_call_output", "call_id": "call_2", "output": "{\"ok\":true}"}
  ]
}`
	res := extractOpenAIToolResultsFromRequest([]byte(body))
	require.Len(t, res, 2)
	assert.Equal(t, "call_1", res[0].CallID)
	assert.Equal(t, "call_2", res[1].CallID)
}

func TestExtractAnthropicToolCallsAndResults(t *testing.T) {
	content := json.RawMessage(`[{"type":"tool_use","id":"toolu_01","name":"weather","input":{"city":"NYC"}}]`)
	calls := extractAnthropicToolCallsFromContent(content)
	require.Len(t, calls, 1)
	assert.Equal(t, "toolu_01", calls[0].CallID)
	assert.Equal(t, "weather", calls[0].Name)

	msgs := json.RawMessage(`[{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_01","content":"72"}]}]`)
	results := extractAnthropicToolResultsFromMessages(msgs)
	require.Len(t, results, 1)
	assert.Equal(t, "toolu_01", results[0].CallID)
}

func TestExtractGeminiToolCallsAndResults(t *testing.T) {
	vg := &request.VendorGemini{
		Output: request.GeminiResponse{
			Candidates: []request.GeminiCandidate{
				{
					Content: &request.GeminiContent{
						Parts: json.RawMessage(`[{"functionCall":{"name":"fn","args":{"a":1}}}]`),
					},
				},
			},
		},
	}
	calls := extractGeminiToolCallsFromResponse(vg)
	require.Len(t, calls, 1)
	assert.Equal(t, "fn", calls[0].Name)

	contents := json.RawMessage(`[{"parts":[{"functionResponse":{"name":"fn","response":{"r":2}}}]}]`)
	results := extractGeminiToolResultsFromContents(contents)
	require.Len(t, results, 1)
	assert.Equal(t, "fn", results[0].Name)
}
