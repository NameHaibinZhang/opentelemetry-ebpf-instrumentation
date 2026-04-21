// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package request

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGenAIToolSemanticStrings(t *testing.T) {
	n, id := GenAIToolSemanticStrings(
		[]GenAIToolCall{{Name: "get_weather", CallID: "call_1"}},
		nil,
	)
	assert.Equal(t, "get_weather", n)
	assert.Equal(t, "call_1", id)

	n2, id2 := GenAIToolSemanticStrings(
		[]GenAIToolCall{
			{Name: "a", CallID: "1"},
			{Name: "b", CallID: "2"},
		},
		nil,
	)
	assert.JSONEq(t, `["a","b"]`, n2)
	assert.JSONEq(t, `["1","2"]`, id2)
}

func TestGenAIToolCallArgumentsJSON(t *testing.T) {
	s := GenAIToolCallArgumentsJSON([]GenAIToolCall{
		{Arguments: json.RawMessage(`{"x":1}`)},
	}, []GenAIToolResult{{Result: json.RawMessage(`"done"`)}})
	assert.JSONEq(t, `[{"x":1},null]`, s)
}

func TestGenAIToolCallResultJSON(t *testing.T) {
	s := GenAIToolCallResultJSON(
		[]GenAIToolCall{{Name: "n", CallID: "c"}},
		[]GenAIToolResult{{Result: json.RawMessage(`"ok"`)}},
	)
	assert.JSONEq(t, `[null,"ok"]`, s)
}
