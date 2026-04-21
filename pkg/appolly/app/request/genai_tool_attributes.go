// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package request

import "encoding/json"

// GenAIToolSemanticStrings returns values for gen_ai.tool.name and gen_ai.tool.call.id.
// A single entry is returned as a plain string; multiple entries are encoded as JSON string arrays.
func GenAIToolSemanticStrings(calls []GenAIToolCall, results []GenAIToolResult) (name, id string) {
	n := len(calls) + len(results)
	if n == 0 {
		return "", ""
	}
	names := make([]string, 0, n)
	ids := make([]string, 0, n)
	for _, c := range calls {
		names = append(names, c.Name)
		ids = append(ids, c.CallID)
	}
	for _, r := range results {
		names = append(names, r.Name)
		ids = append(ids, r.CallID)
	}
	if n == 1 {
		return names[0], ids[0]
	}
	nb, _ := json.Marshal(names)
	ib, _ := json.Marshal(ids)
	return string(nb), string(ib)
}

// GenAIToolCallArgumentsJSON returns a JSON value for gen_ai.tool.call.arguments (opt-in).
// When both calls and results are present, entries are ordered with calls first; result slots use JSON null.
func GenAIToolCallArgumentsJSON(calls []GenAIToolCall, results []GenAIToolResult) string {
	if len(calls) == 0 {
		return ""
	}
	if len(results) == 0 && len(calls) == 1 {
		if len(calls[0].Arguments) == 0 {
			return ""
		}
		return string(calls[0].Arguments)
	}
	parts := make([]json.RawMessage, len(calls)+len(results))
	for i, c := range calls {
		if len(c.Arguments) == 0 {
			parts[i] = json.RawMessage("null")
		} else {
			parts[i] = c.Arguments
		}
	}
	for i := range results {
		parts[len(calls)+i] = json.RawMessage("null")
	}
	b, _ := json.Marshal(parts)
	return string(b)
}

// GenAIToolCallResultJSON returns a JSON value for gen_ai.tool.call.result (opt-in).
// When both calls and results are present, call slots use JSON null.
func GenAIToolCallResultJSON(calls []GenAIToolCall, results []GenAIToolResult) string {
	if len(results) == 0 {
		return ""
	}
	if len(calls) == 0 && len(results) == 1 {
		if len(results[0].Result) == 0 {
			return ""
		}
		return string(results[0].Result)
	}
	parts := make([]json.RawMessage, len(calls)+len(results))
	for i := range calls {
		parts[i] = json.RawMessage("null")
	}
	for i, r := range results {
		idx := len(calls) + i
		if len(r.Result) == 0 {
			parts[idx] = json.RawMessage("null")
		} else {
			parts[idx] = r.Result
		}
	}
	b, _ := json.Marshal(parts)
	return string(b)
}
