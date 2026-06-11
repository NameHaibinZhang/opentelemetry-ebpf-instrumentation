// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package ebpfcommon

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseOpenAIStream_CompleteResponse(t *testing.T) {
	stream := "data: {\"id\":\"chatcmpl-abc123\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-4\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"chatcmpl-abc123\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-4\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"chatcmpl-abc123\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-4\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\" world\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\n" +
		"data: [DONE]\n"

	resp, toolCalls, err := parseOpenAIStream(strings.NewReader(stream))

	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, "chatcmpl-abc123", resp.ID)
	assert.Equal(t, "gpt-4", resp.ResponseModel)
	assert.Equal(t, 10, resp.Usage.PromptTokens)
	assert.Equal(t, 5, resp.Usage.CompletionTokens)
	assert.Equal(t, 15, resp.Usage.TotalTokens)
	assert.Empty(t, toolCalls)

	reasons := resp.GetFinishReasons()
	require.Len(t, reasons, 1)
	assert.Equal(t, "stop", reasons[0])
}

func TestParseOpenAIStream_TruncatedNoDone(t *testing.T) {
	// Simulates a buffer truncation where [DONE] is never received.
	stream := "data: {\"id\":\"chatcmpl-trunc\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"chatcmpl-trunc\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"},\"finish_reason\":null}]}\n\n"

	resp, toolCalls, err := parseOpenAIStream(strings.NewReader(stream))

	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, "chatcmpl-trunc", resp.ID)
	assert.Equal(t, "gpt-4o", resp.ResponseModel)
	// No usage in truncated stream.
	assert.Equal(t, 0, resp.Usage.PromptTokens)
	assert.Equal(t, 0, resp.Usage.CompletionTokens)
	// No finish_reason means no Choices JSON set.
	assert.Nil(t, resp.GetFinishReasons())
	assert.Empty(t, toolCalls)
}

func TestParseOpenAIStream_ToolCalls(t *testing.T) {
	stream := "data: {\"id\":\"chatcmpl-tc\",\"model\":\"gpt-4\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"call_abc\",\"type\":\"function\",\"function\":{\"name\":\"get_weather\",\"arguments\":\"\"}}]},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"chatcmpl-tc\",\"model\":\"gpt-4\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"lo\"}}]},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"chatcmpl-tc\",\"model\":\"gpt-4\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"cation\\\": \\\"NYC\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: [DONE]\n"

	resp, toolCalls, err := parseOpenAIStream(strings.NewReader(stream))

	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, "chatcmpl-tc", resp.ID)
	assert.Equal(t, "gpt-4", resp.ResponseModel)

	reasons := resp.GetFinishReasons()
	require.Len(t, reasons, 1)
	assert.Equal(t, "tool_calls", reasons[0])

	require.Len(t, toolCalls, 1)
	assert.Equal(t, "call_abc", toolCalls[0].ID)
	assert.Equal(t, "get_weather", toolCalls[0].Name)
}

func TestParseOpenAIStream_EmptyStream(t *testing.T) {
	// Only [DONE] is present — no actual data chunks.
	stream := "data: [DONE]\n"

	resp, toolCalls, err := parseOpenAIStream(strings.NewReader(stream))

	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, "", resp.ID)
	assert.Equal(t, "", resp.ResponseModel)
	assert.Equal(t, 0, resp.Usage.PromptTokens)
	assert.Equal(t, 0, resp.Usage.CompletionTokens)
	assert.Nil(t, resp.GetFinishReasons())
	assert.Empty(t, toolCalls)
}

func TestParseOpenAIStream_WithUsageInLastChunk(t *testing.T) {
	// When stream_options: {include_usage: true}, the final chunk includes usage.
	stream := "data: {\"id\":\"chatcmpl-usage\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-4-turbo\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"chatcmpl-usage\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-4-turbo\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hi\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"chatcmpl-usage\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-4-turbo\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\" there\"},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: {\"id\":\"chatcmpl-usage\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-4-turbo\",\"choices\":[],\"usage\":{\"prompt_tokens\":25,\"completion_tokens\":12,\"total_tokens\":37}}\n\n" +
		"data: [DONE]\n"

	resp, toolCalls, err := parseOpenAIStream(strings.NewReader(stream))

	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, "chatcmpl-usage", resp.ID)
	assert.Equal(t, "gpt-4-turbo", resp.ResponseModel)
	assert.Equal(t, 25, resp.Usage.PromptTokens)
	assert.Equal(t, 12, resp.Usage.CompletionTokens)
	assert.Equal(t, 37, resp.Usage.TotalTokens)
	assert.Empty(t, toolCalls)

	reasons := resp.GetFinishReasons()
	require.Len(t, reasons, 1)
	assert.Equal(t, "stop", reasons[0])
}
