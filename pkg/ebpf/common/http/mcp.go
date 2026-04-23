// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package ebpfcommon // import "go.opentelemetry.io/obi/pkg/ebpf/common/http"

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"

	"go.opentelemetry.io/obi/pkg/appolly/app/request"
)

// mcpMethods enumerates known MCP JSON-RPC method names.
var mcpMethods = map[string]struct{}{
	"initialize":                         {},
	"notifications/initialized":          {},
	"tools/call":                         {},
	"tools/list":                         {},
	"resources/read":                     {},
	"resources/list":                     {},
	"resources/subscribe":                {},
	"resources/unsubscribe":              {},
	"resources/templates/list":           {},
	"prompts/get":                        {},
	"prompts/list":                       {},
	"completion/complete":                {},
	"logging/setLevel":                   {},
	"notifications/cancelled":            {},
	"notifications/resources/updated":    {},
	"notifications/tools/list_changed":   {},
	"notifications/prompts/list_changed": {},
	"ping":                               {},
}

// ambiguousMethods lists JSON-RPC method names shared with other protocols
// (e.g. LSP). Each entry maps to a disambiguator function that returns true
// when the request carries an MCP-specific signal beyond the method name.
// The Mcp-Session-Id header is checked before consulting this map; entries
// here only need to handle the no-session-header case.
var ambiguousMethods = map[string]func(json.RawMessage) bool{
	"initialize": hasMCPProtocolVersion,
	"ping":       func(json.RawMessage) bool { return false },
}

// mcpSessionHeader is the HTTP header that carries the MCP session identifier.
const mcpSessionHeader = "Mcp-Session-Id"

// mcpRequest is the JSON-RPC 2.0 request envelope used by MCP.
type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	ID      json.RawMessage `json:"id,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// mcpResponse is the JSON-RPC 2.0 response envelope used by MCP.
type mcpResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *jsonRPCError   `json:"error,omitempty"`
}

// Param structures for extracting method-specific fields.

type mcpToolCallParams struct {
	Name string `json:"name"`
}

type mcpResourceParams struct {
	URI string `json:"uri"`
}

type mcpPromptParams struct {
	Name string `json:"name"`
}

type mcpInitializeParams struct {
	ProtocolVersion string `json:"protocolVersion"`
}

type mcpInitializeResult struct {
	ProtocolVersion string `json:"protocolVersion"`
}

// MCPSpan detects and parses an MCP JSON-RPC request/response pair.
// It returns the enriched span and true when the request is a valid MCP call,
// or the original span and false otherwise.
func MCPSpan(baseSpan *request.Span, req *http.Request, resp *http.Response) (request.Span, bool) {
	if req.Method != http.MethodPost {
		return *baseSpan, false
	}

	sessionID := req.Header.Get(mcpSessionHeader)
	if sessionID == "" && resp != nil && resp.Header != nil {
		sessionID = resp.Header.Get(mcpSessionHeader)
	}

	reqB, err := io.ReadAll(req.Body)
	if err != nil {
		return *baseSpan, false
	}
	req.Body = io.NopCloser(bytes.NewBuffer(reqB))

	reqB = bytes.TrimSpace(reqB)
	// NOTE: JSON-RPC 2.0 also permits batch requests (arrays), but MCP
	// batch support is not implemented yet. Only single-object requests
	// are handled here; batch requests fall through to the generic
	// JSON-RPC parser in jsonrpc.go.
	if len(reqB) == 0 || reqB[0] != '{' {
		return *baseSpan, false
	}

	var rpcReq mcpRequest
	if err := json.Unmarshal(reqB, &rpcReq); err != nil {
		return *baseSpan, false
	}

	// MCP requires JSON-RPC 2.0.
	if rpcReq.JSONRPC != "2.0" {
		return *baseSpan, false
	}

	if _, known := mcpMethods[rpcReq.Method]; !known {
		// Not a recognized MCP method. Check whether the session header
		// was present — that still qualifies the request as MCP even if
		// the method is unknown (e.g. a custom extension method).
		if sessionID == "" {
			return *baseSpan, false
		}
	} else if disambiguate, ambiguous := ambiguousMethods[rpcReq.Method]; ambiguous && sessionID == "" {
		// Generic method names like "initialize" and "ping" are shared
		// with other JSON-RPC protocols (e.g. LSP). Without the MCP
		// session header, consult the per-method disambiguator.
		if !disambiguate(rpcReq.Params) {
			return *baseSpan, false
		}
	}

	slog.Debug("MCP", "method", rpcReq.Method, "session", sessionID)

	result := &request.MCPCall{
		Method:    rpcReq.Method,
		SessionID: sessionID,
	}

	if len(rpcReq.ID) > 0 && string(rpcReq.ID) != "null" {
		result.RequestID = rawIDString(rpcReq.ID)
	}

	parseMCPParams(rpcReq, result)

	// Parse response for error and protocol version.
	if resp != nil && resp.Body != nil {
		respB, err := getResponseBody(resp)
		if err == nil && len(respB) > 0 {
			parseMCPResponse(respB, result)
		}
	}

	baseSpan.SubType = request.HTTPSubtypeMCP
	baseSpan.GenAI = &request.GenAI{
		MCP: result,
	}

	return *baseSpan, true
}

// hasMCPProtocolVersion checks whether the params contain a protocolVersion
// field, which is specific to MCP's initialize method.
func hasMCPProtocolVersion(params json.RawMessage) bool {
	if len(params) == 0 {
		return false
	}
	var p mcpInitializeParams
	return json.Unmarshal(params, &p) == nil && p.ProtocolVersion != ""
}

// parseMCPParams extracts method-specific fields from the request params.
func parseMCPParams(rpcReq mcpRequest, result *request.MCPCall) {
	if len(rpcReq.Params) == 0 {
		return
	}

	switch rpcReq.Method {
	case "tools/call":
		var p mcpToolCallParams
		if json.Unmarshal(rpcReq.Params, &p) == nil {
			result.ToolName = p.Name
		}
	case "resources/read", "resources/subscribe", "resources/unsubscribe":
		var p mcpResourceParams
		if json.Unmarshal(rpcReq.Params, &p) == nil {
			result.ResourceURI = p.URI
		}
	case "prompts/get":
		var p mcpPromptParams
		if json.Unmarshal(rpcReq.Params, &p) == nil {
			result.PromptName = p.Name
		}
	case "initialize":
		var p mcpInitializeParams
		if json.Unmarshal(rpcReq.Params, &p) == nil {
			result.ProtocolVer = p.ProtocolVersion
		}
	}
}

// parseMCPResponse extracts error information and protocol version from the response.
func parseMCPResponse(data []byte, result *request.MCPCall) {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || data[0] != '{' {
		return
	}

	var resp mcpResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return
	}

	if resp.JSONRPC != "2.0" {
		return
	}

	if resp.Error != nil {
		result.ErrorCode = resp.Error.Code
		result.ErrorMessage = resp.Error.Message
	}

	// For initialize responses, extract the negotiated protocol version.
	if result.Method == "initialize" && len(resp.Result) > 0 {
		var initResult mcpInitializeResult
		if json.Unmarshal(resp.Result, &initResult) == nil && initResult.ProtocolVersion != "" {
			result.ProtocolVer = initResult.ProtocolVersion
		}
	}
}
