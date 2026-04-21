// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package ebpfcommon

import (
	"encoding/json"

	"go.opentelemetry.io/obi/pkg/appolly/app/request"
)

func populateOpenAIToolData(ai *request.VendorOpenAI, reqBody, respBody []byte) {
	if ai == nil {
		return
	}
	ai.ToolCalls = extractOpenAIToolCallsFromResponse(respBody)
	ai.ToolResults = extractOpenAIToolResultsFromRequest(reqBody)
}

func extractOpenAIToolCallsFromResponse(respBody []byte) []request.GenAIToolCall {
	var root struct {
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Output []json.RawMessage `json:"output"`
	}
	if err := json.Unmarshal(respBody, &root); err != nil {
		return nil
	}
	var out []request.GenAIToolCall
	for _, ch := range root.Choices {
		for _, tc := range ch.Message.ToolCalls {
			args := json.RawMessage(nil)
			if tc.Function.Arguments != "" {
				args = json.RawMessage(tc.Function.Arguments)
			}
			out = append(out, request.GenAIToolCall{
				CallID:    tc.ID,
				Name:      tc.Function.Name,
				Arguments: args,
			})
		}
	}
	for _, raw := range root.Output {
		call := extractOpenAIFunctionCallOutputItem(raw)
		if call != nil {
			out = append(out, *call)
		}
	}
	return out
}

func extractOpenAIFunctionCallOutputItem(raw json.RawMessage) *request.GenAIToolCall {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	var typ string
	if t, ok := m["type"]; ok {
		_ = json.Unmarshal(t, &typ)
	}
	if typ != "function_call" {
		return nil
	}
	call := request.GenAIToolCall{}
	if v, ok := m["name"]; ok {
		_ = json.Unmarshal(v, &call.Name)
	}
	if v, ok := m["call_id"]; ok {
		_ = json.Unmarshal(v, &call.CallID)
	}
	if call.CallID == "" {
		if v, ok := m["id"]; ok {
			_ = json.Unmarshal(v, &call.CallID)
		}
	}
	if v, ok := m["arguments"]; ok {
		call.Arguments = normalizeOpenAIArgumentsRaw(v)
	}
	return &call
}

func normalizeOpenAIArgumentsRaw(v json.RawMessage) json.RawMessage {
	if len(v) == 0 {
		return nil
	}
	var s string
	if err := json.Unmarshal(v, &s); err == nil {
		return json.RawMessage(s)
	}
	return v
}

func extractOpenAIToolResultsFromRequest(reqBody []byte) []request.GenAIToolResult {
	var root struct {
		Messages []struct {
			Role       string          `json:"role"`
			ToolCallID string          `json:"tool_call_id"`
			Content    json.RawMessage `json:"content"`
		} `json:"messages"`
		Input []json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(reqBody, &root); err != nil {
		return nil
	}
	var out []request.GenAIToolResult
	for _, m := range root.Messages {
		if m.Role != "tool" {
			continue
		}
		out = append(out, request.GenAIToolResult{
			CallID: m.ToolCallID,
			Result: m.Content,
		})
	}
	for _, raw := range root.Input {
		res := extractOpenAIFunctionCallOutputItemRequest(raw)
		if res != nil {
			out = append(out, *res)
		}
	}
	return out
}

func extractOpenAIFunctionCallOutputItemRequest(raw json.RawMessage) *request.GenAIToolResult {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	var typ string
	if t, ok := m["type"]; ok {
		_ = json.Unmarshal(t, &typ)
	}
	if typ != "function_call_output" {
		return nil
	}
	res := request.GenAIToolResult{}
	if v, ok := m["call_id"]; ok {
		_ = json.Unmarshal(v, &res.CallID)
	}
	if v, ok := m["output"]; ok {
		res.Result = v
	}
	return &res
}

func populateAnthropicToolData(ai *request.VendorAnthropic) {
	if ai == nil {
		return
	}
	ai.ToolCalls = extractAnthropicToolCallsFromContent(ai.Output.Content)
	ai.ToolResults = extractAnthropicToolResultsFromMessages(ai.Input.Messages)
}

func extractAnthropicToolCallsFromContent(content json.RawMessage) []request.GenAIToolCall {
	var blocks []struct {
		Type  string          `json:"type"`
		ID    string          `json:"id"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(content, &blocks); err != nil {
		return nil
	}
	var out []request.GenAIToolCall
	for _, b := range blocks {
		if b.Type != "tool_use" {
			continue
		}
		out = append(out, request.GenAIToolCall{
			CallID:    b.ID,
			Name:      b.Name,
			Arguments: b.Input,
		})
	}
	return out
}

func extractAnthropicToolResultsFromMessages(messages json.RawMessage) []request.GenAIToolResult {
	var msgs []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(messages, &msgs); err != nil {
		return nil
	}
	var out []request.GenAIToolResult
	for _, m := range msgs {
		if m.Role != "user" {
			continue
		}
		var blocks []struct {
			Type      string          `json:"type"`
			ToolUseID string          `json:"tool_use_id"`
			Content   json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(m.Content, &blocks); err != nil {
			continue
		}
		for _, b := range blocks {
			if b.Type != "tool_result" {
				continue
			}
			out = append(out, request.GenAIToolResult{
				CallID: b.ToolUseID,
				Result: b.Content,
			})
		}
	}
	return out
}

func populateGeminiToolData(vg *request.VendorGemini) {
	if vg == nil {
		return
	}
	vg.ToolCalls = extractGeminiToolCallsFromResponse(vg)
	vg.ToolResults = extractGeminiToolResultsFromContents(vg.Input.Contents)
}

func extractGeminiToolCallsFromResponse(vg *request.VendorGemini) []request.GenAIToolCall {
	var out []request.GenAIToolCall
	for _, ch := range vg.Output.Candidates {
		if ch.Content == nil {
			continue
		}
		var parts []struct {
			FunctionCall *struct {
				Name string          `json:"name"`
				Args json.RawMessage `json:"args"`
			} `json:"functionCall"`
		}
		if err := json.Unmarshal(ch.Content.Parts, &parts); err != nil {
			continue
		}
		for _, p := range parts {
			if p.FunctionCall == nil {
				continue
			}
			out = append(out, request.GenAIToolCall{
				Name:      p.FunctionCall.Name,
				Arguments: p.FunctionCall.Args,
			})
		}
	}
	return out
}

func extractGeminiToolResultsFromContents(contents json.RawMessage) []request.GenAIToolResult {
	var msgs []struct {
		Parts []struct {
			FunctionResponse *struct {
				Name     string          `json:"name"`
				Response json.RawMessage `json:"response"`
			} `json:"functionResponse"`
		} `json:"parts"`
	}
	if err := json.Unmarshal(contents, &msgs); err != nil {
		return nil
	}
	var out []request.GenAIToolResult
	for _, m := range msgs {
		for _, p := range m.Parts {
			if p.FunctionResponse == nil {
				continue
			}
			fr := p.FunctionResponse
			out = append(out, request.GenAIToolResult{
				Name:   fr.Name,
				Result: fr.Response,
			})
		}
	}
	return out
}
