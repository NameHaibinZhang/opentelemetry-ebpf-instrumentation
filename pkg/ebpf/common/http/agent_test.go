// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package ebpfcommon

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.opentelemetry.io/obi/pkg/appolly/app/request"
)

const openAICreateRunRequest = `{
  "assistant_id": "asst_abc123",
  "model": "gpt-4o"
}`

const openAICreateRunResponse = `{
  "id": "run_xyz",
  "status": "completed",
  "usage": {
    "prompt_tokens": 100,
    "completion_tokens": 50
  }
}`

const openAICreateRunWithToolsRequest = `{
  "assistant_id": "asst_tools456",
  "model": "gpt-4o-mini",
  "tools": [
    {"type": "code_interpreter"},
    {"type": "file_search"}
  ]
}`

const openAICreateRunWithToolsResponse = `{
  "id": "run_tools789",
  "status": "in_progress",
  "usage": {
    "prompt_tokens": 200,
    "completion_tokens": 80
  }
}`

const openAIRunResponseNoUsage = `{
  "id": "run_nousage",
  "status": "queued"
}`

const bedrockInvokeRequest = `{
  "inputText": "What is the weather today?"
}`

const bedrockInvokeResponse = `{
  "usage": {
    "inputTokens": 150,
    "outputTokens": 75
  }
}`

const bedrockInvokeResponseNoUsage = `{
  "completion": "The weather is sunny."
}`

// Reuses makeRequest and makePlainResponse helpers from openai_test.go.

func TestAgentSpan_OpenAI_CreateRun(t *testing.T) {
	req := makeRequest(t, http.MethodPost,
		"http://api.openai.com/v1/threads/thread_abc789/runs", openAICreateRunRequest)
	resp := makePlainResponse(http.StatusOK, http.Header{
		"Content-Type": []string{"application/json"},
	}, openAICreateRunResponse)

	base := &request.Span{}
	span, ok := AgentSpan(base, req, resp)

	require.True(t, ok)
	require.NotNil(t, span.GenAI)
	require.NotNil(t, span.GenAI.Agent)
	assert.Equal(t, request.HTTPSubtypeAgent, span.SubType)

	agent := span.GenAI.Agent
	assert.Equal(t, "openai", agent.Provider)
	assert.Equal(t, "asst_abc123", agent.AgentID)
	assert.Equal(t, "thread_abc789", agent.SessionID)
	assert.Equal(t, "run_xyz", agent.RunID)
	assert.Equal(t, "gpt-4o", agent.Model)
	assert.Equal(t, "completed", agent.Status)
	assert.Equal(t, 100, agent.InputTokens)
	assert.Equal(t, 50, agent.OutputTokens)
}

func TestAgentSpan_OpenAI_CreateRunWithTools(t *testing.T) {
	req := makeRequest(t, http.MethodPost,
		"http://api.openai.com/v1/threads/thread_tools001/runs", openAICreateRunWithToolsRequest)
	resp := makePlainResponse(http.StatusOK, http.Header{
		"Content-Type": []string{"application/json"},
	}, openAICreateRunWithToolsResponse)

	base := &request.Span{}
	span, ok := AgentSpan(base, req, resp)

	require.True(t, ok)
	require.NotNil(t, span.GenAI)
	require.NotNil(t, span.GenAI.Agent)

	agent := span.GenAI.Agent
	assert.Equal(t, "openai", agent.Provider)
	assert.Equal(t, "asst_tools456", agent.AgentID)
	assert.Equal(t, "thread_tools001", agent.SessionID)
	assert.Equal(t, "run_tools789", agent.RunID)
	assert.Equal(t, "gpt-4o-mini", agent.Model)
	assert.Equal(t, "in_progress", agent.Status)
	assert.Equal(t, 200, agent.InputTokens)
	assert.Equal(t, 80, agent.OutputTokens)
}

func TestAgentSpan_Bedrock_InvokeAgent(t *testing.T) {
	req := makeRequest(t, http.MethodPost,
		"http://bedrock-agent-runtime.us-east-1.amazonaws.com/agents/AGENTID123/agentAliases/ALIAS1/sessions/SESSION456/invoke",
		bedrockInvokeRequest)
	resp := makePlainResponse(http.StatusOK, http.Header{
		"Content-Type": []string{"application/json"},
	}, bedrockInvokeResponse)

	base := &request.Span{}
	span, ok := AgentSpan(base, req, resp)

	require.True(t, ok)
	require.NotNil(t, span.GenAI)
	require.NotNil(t, span.GenAI.Agent)
	assert.Equal(t, request.HTTPSubtypeAgent, span.SubType)

	agent := span.GenAI.Agent
	assert.Equal(t, "aws.bedrock", agent.Provider)
	assert.Equal(t, "AGENTID123", agent.AgentID)
	assert.Equal(t, "SESSION456", agent.SessionID)
	assert.Equal(t, 150, agent.InputTokens)
	assert.Equal(t, 75, agent.OutputTokens)
}

func TestAgentSpan_NotAgent_OpenAIChatCompletions(t *testing.T) {
	req := makeRequest(t, http.MethodPost,
		"http://api.openai.com/v1/chat/completions",
		`{"model": "gpt-4o", "messages": [{"role": "user", "content": "Hello"}]}`)
	resp := makePlainResponse(http.StatusOK, http.Header{
		"Content-Type": []string{"application/json"},
	}, `{"id": "chatcmpl-123", "choices": []}`)

	base := &request.Span{}
	_, ok := AgentSpan(base, req, resp)

	assert.False(t, ok, "regular OpenAI chat completions should not be detected as agent")
}

func TestAgentSpan_NotAgent_BedrockInvokeModel(t *testing.T) {
	req := makeRequest(t, http.MethodPost,
		"http://bedrock-runtime.us-east-1.amazonaws.com/model/anthropic.claude-v2/invoke",
		`{"prompt": "Hello"}`)
	resp := makePlainResponse(http.StatusOK, http.Header{
		"Content-Type": []string{"application/json"},
	}, `{"completion": "Hi there"}`)

	base := &request.Span{}
	_, ok := AgentSpan(base, req, resp)

	assert.False(t, ok, "bedrock-runtime invoke_model should not be detected as agent")
}

func TestAgentSpan_NotAgent_NonPostMethod(t *testing.T) {
	req := makeRequest(t, http.MethodGet,
		"http://api.openai.com/v1/threads/thread_abc/runs", "")
	resp := makePlainResponse(http.StatusOK, http.Header{
		"Content-Type": []string{"application/json"},
	}, `{"data": []}`)

	base := &request.Span{}
	_, ok := AgentSpan(base, req, resp)

	assert.False(t, ok, "non-POST request should not be detected as agent")
}

func TestAgentSpan_EmptyBody(t *testing.T) {
	req := makeRequest(t, http.MethodPost,
		"http://api.openai.com/v1/threads/thread_empty/runs", "")
	resp := makePlainResponse(http.StatusOK, http.Header{
		"Content-Type": []string{"application/json"},
	}, "")

	base := &request.Span{}
	span, ok := AgentSpan(base, req, resp)

	// Path matches OpenAI Assistants so it should still be detected,
	// but fields from body parsing will be empty.
	require.True(t, ok)
	agent := span.GenAI.Agent
	assert.Equal(t, "openai", agent.Provider)
	assert.Equal(t, "thread_empty", agent.SessionID)
	assert.Empty(t, agent.AgentID)
	assert.Empty(t, agent.Model)
	assert.Empty(t, agent.RunID)
	assert.Equal(t, 0, agent.InputTokens)
	assert.Equal(t, 0, agent.OutputTokens)
}

func TestAgentSpan_InvalidJSON(t *testing.T) {
	req := makeRequest(t, http.MethodPost,
		"http://api.openai.com/v1/threads/thread_badjson/runs", "not-valid-json{{{")
	resp := makePlainResponse(http.StatusOK, http.Header{
		"Content-Type": []string{"application/json"},
	}, "also-not-json!!!")

	base := &request.Span{}
	span, ok := AgentSpan(base, req, resp)

	// Path matches so detected, but body parsing fails gracefully.
	require.True(t, ok)
	agent := span.GenAI.Agent
	assert.Equal(t, "openai", agent.Provider)
	assert.Equal(t, "thread_badjson", agent.SessionID)
	assert.Empty(t, agent.AgentID)
	assert.Empty(t, agent.RunID)
}

func TestAgentSpan_ResponseMissingUsage(t *testing.T) {
	req := makeRequest(t, http.MethodPost,
		"http://api.openai.com/v1/threads/thread_nouse/runs", openAICreateRunRequest)
	resp := makePlainResponse(http.StatusOK, http.Header{
		"Content-Type": []string{"application/json"},
	}, openAIRunResponseNoUsage)

	base := &request.Span{}
	span, ok := AgentSpan(base, req, resp)

	require.True(t, ok)
	agent := span.GenAI.Agent
	assert.Equal(t, "openai", agent.Provider)
	assert.Equal(t, "asst_abc123", agent.AgentID)
	assert.Equal(t, "run_nousage", agent.RunID)
	assert.Equal(t, "queued", agent.Status)
	assert.Equal(t, 0, agent.InputTokens)
	assert.Equal(t, 0, agent.OutputTokens)
}

func TestAgentSpan_Bedrock_ResponseMissingUsage(t *testing.T) {
	req := makeRequest(t, http.MethodPost,
		"http://bedrock-agent-runtime.eu-west-1.amazonaws.com/agents/AGT1/agentAliases/AL1/sessions/SESS1/invoke",
		bedrockInvokeRequest)
	resp := makePlainResponse(http.StatusOK, http.Header{
		"Content-Type": []string{"application/json"},
	}, bedrockInvokeResponseNoUsage)

	base := &request.Span{}
	span, ok := AgentSpan(base, req, resp)

	require.True(t, ok)
	agent := span.GenAI.Agent
	assert.Equal(t, "aws.bedrock", agent.Provider)
	assert.Equal(t, "AGT1", agent.AgentID)
	assert.Equal(t, "SESS1", agent.SessionID)
	assert.Equal(t, 0, agent.InputTokens)
	assert.Equal(t, 0, agent.OutputTokens)
}

func TestAgentSpan_NilRequest(t *testing.T) {
	base := &request.Span{}
	_, ok := AgentSpan(base, nil, nil)
	assert.False(t, ok)
}

func TestAgentSpan_NilURL(t *testing.T) {
	req := &http.Request{Method: http.MethodPost}
	base := &request.Span{}
	_, ok := AgentSpan(base, req, nil)
	assert.False(t, ok)
}

func TestIsGenAISubtype_Agent(t *testing.T) {
	assert.True(t, request.IsGenAISubtype(request.HTTPSubtypeAgent))
}
