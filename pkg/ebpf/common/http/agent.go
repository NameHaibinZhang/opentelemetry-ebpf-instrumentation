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

// OpenAI Assistants API constants.
const (
	openAIAssistantsHost    = "api.openai.com"
	openAIThreadsPathPrefix = "/v1/threads/"
	openAIRunsSegment       = "/runs"
)

// AWS Bedrock Agents constants.
const (
	bedrockAgentHostPrefix = "bedrock-agent-runtime."
	bedrockAgentHostSuffix = ".amazonaws.com"
	bedrockAgentsSegment   = "/agents/"
	bedrockSessionsSegment = "/sessions/"
)

// agentOpenAIRequest captures fields from the OpenAI Assistants run request body.
type agentOpenAIRequest struct {
	AssistantID  string `json:"assistant_id"`
	Model        string `json:"model"`
	Instructions string `json:"instructions"`
}

// agentOpenAIResponse captures fields from the OpenAI Assistants run response.
type agentOpenAIResponse struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Usage  *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage,omitempty"`
}

// AgentSpan detects AI agent framework API calls (OpenAI Assistants, AWS Bedrock
// Agents) and extracts agent-specific fields into the span.
func AgentSpan(baseSpan *request.Span, req *http.Request, resp *http.Response) (request.Span, bool) {
	if req == nil || req.URL == nil {
		return *baseSpan, false
	}

	if agent, ok := parseOpenAIAssistant(req, resp, baseSpan); ok {
		baseSpan.SubType = request.HTTPSubtypeAgent
		baseSpan.GenAI = &request.GenAI{Agent: agent}
		return *baseSpan, true
	}

	if agent, ok := parseBedrockAgent(req, resp, baseSpan); ok {
		baseSpan.SubType = request.HTTPSubtypeAgent
		baseSpan.GenAI = &request.GenAI{Agent: agent}
		return *baseSpan, true
	}

	return *baseSpan, false
}

// parseOpenAIAssistant detects OpenAI Assistants API calls by matching
// the host and URL path pattern: POST /v1/threads/{thread_id}/runs.
func parseOpenAIAssistant(req *http.Request, resp *http.Response, baseSpan *request.Span) (*request.VendorAgent, bool) {
	host := extractHostname(req)
	if host != openAIAssistantsHost {
		return nil, false
	}

	path := requestPath(req)
	if !isOpenAIAssistantsPath(path) {
		return nil, false
	}

	if req.Method != http.MethodPost {
		return nil, false
	}

	threadID := extractThreadID(path)

	agent := &request.VendorAgent{
		Provider:  "openai",
		SessionID: threadID,
	}

	reqB := readHTTPRequestBodyLenient("AgentSpan", req, baseSpan, "provider", "openai")
	if len(reqB) > 0 {
		var parsed agentOpenAIRequest
		if json.Unmarshal(reqB, &parsed) == nil {
			agent.AgentID = parsed.AssistantID
			agent.Model = parsed.Model
		}
	}

	respB := readHTTPResponseBodyLenient("AgentSpan", resp, baseSpan, "provider", "openai")
	if len(respB) > 0 {
		var parsed agentOpenAIResponse
		if json.Unmarshal(respB, &parsed) == nil {
			agent.RunID = parsed.ID
			agent.Status = parsed.Status
			if parsed.Usage != nil {
				agent.InputTokens = parsed.Usage.PromptTokens
				agent.OutputTokens = parsed.Usage.CompletionTokens
			}
		}
	}

	slog.Debug("Agent", "provider", "openai", "thread", threadID, "agent_id", agent.AgentID)

	return agent, true
}

// isOpenAIAssistantsPath returns true if the path matches
// /v1/threads/{thread_id}/runs or /v1/threads/{thread_id}/runs/{run_id}...
func isOpenAIAssistantsPath(path string) bool {
	if !strings.HasPrefix(path, openAIThreadsPathPrefix) {
		return false
	}

	rest := path[len(openAIThreadsPathPrefix):]
	runsIdx := strings.Index(rest, openAIRunsSegment)
	return runsIdx > 0
}

// extractThreadID extracts the thread_id from a path like /v1/threads/{thread_id}/runs...
func extractThreadID(path string) string {
	rest := path[len(openAIThreadsPathPrefix):]
	slashIdx := strings.Index(rest, "/")
	if slashIdx <= 0 {
		return rest
	}
	return rest[:slashIdx]
}

// parseBedrockAgent detects AWS Bedrock Agents API calls by matching
// the host pattern (bedrock-agent-runtime.*.amazonaws.com) and path
// containing /agents/ and /sessions/.
func parseBedrockAgent(req *http.Request, resp *http.Response, baseSpan *request.Span) (*request.VendorAgent, bool) {
	host := extractHostname(req)
	if !isBedrockAgentHost(host) {
		return nil, false
	}

	path := requestPath(req)
	if !isBedrockAgentPath(path) {
		return nil, false
	}

	agentID := extractPathSegmentAfter(path, bedrockAgentsSegment)
	sessionID := extractPathSegmentAfter(path, bedrockSessionsSegment)

	agent := &request.VendorAgent{
		Provider:  "aws.bedrock",
		AgentID:   agentID,
		SessionID: sessionID,
	}

	reqB := readHTTPRequestBodyLenient("AgentSpan", req, baseSpan, "provider", "aws.bedrock")
	if len(reqB) > 0 {
		var parsed struct {
			InputText string `json:"inputText"`
		}
		if json.Unmarshal(reqB, &parsed) == nil {
			// inputText is informational; we do not store it in the span
			// but its presence confirms this is a genuine agent request.
			_ = parsed.InputText
		}
	}

	respB := readHTTPResponseBodyLenient("AgentSpan", resp, baseSpan, "provider", "aws.bedrock")
	if len(respB) > 0 {
		var parsed struct {
			Usage *struct {
				InputTokens  int `json:"inputTokens"`
				OutputTokens int `json:"outputTokens"`
			} `json:"usage,omitempty"`
		}
		if json.Unmarshal(respB, &parsed) == nil && parsed.Usage != nil {
			agent.InputTokens = parsed.Usage.InputTokens
			agent.OutputTokens = parsed.Usage.OutputTokens
		}
	}

	slog.Debug("Agent", "provider", "aws.bedrock", "agentId", agentID, "sessionId", sessionID)

	return agent, true
}

// isBedrockAgentHost returns true when the host matches
// bedrock-agent-runtime.{region}.amazonaws.com.
func isBedrockAgentHost(host string) bool {
	if !strings.HasPrefix(host, bedrockAgentHostPrefix) {
		return false
	}
	return strings.HasSuffix(host, bedrockAgentHostSuffix)
}

// isBedrockAgentPath returns true when the path contains both /agents/ and
// /sessions/ segments, indicating an agent invocation endpoint.
func isBedrockAgentPath(path string) bool {
	return strings.Contains(path, bedrockAgentsSegment) &&
		strings.Contains(path, bedrockSessionsSegment)
}

// extractPathSegmentAfter extracts the path segment immediately following
// the given prefix. For example, given path "/agents/ABC123/sessions/XYZ"
// and prefix "/agents/", it returns "ABC123".
func extractPathSegmentAfter(path, prefix string) string {
	idx := strings.Index(path, prefix)
	if idx < 0 {
		return ""
	}

	rest := path[idx+len(prefix):]
	slashIdx := strings.Index(rest, "/")
	if slashIdx <= 0 {
		return rest
	}
	return rest[:slashIdx]
}
