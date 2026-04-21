// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package integration // import "go.opentelemetry.io/obi/internal/test/integration"

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	neturl "net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/obi/internal/test/integration/components/jaeger"
)

// mcpCall sends a JSON-RPC 2.0 MCP request over HTTP and returns the response.
func mcpCall(url, method string, id int, params any) (*http.Response, error) {
	req := map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"id":      id,
	}
	if params != nil {
		req["params"] = params
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	return http.Post(url, "application/json", bytes.NewReader(body)) //nolint:noctx
}

func testPythonMCPServer(t *testing.T) {
	const (
		comm    = "python3.14"
		address = "http://localhost:8381/mcp"
	)

	var tq jaeger.TracesQuery
	params := neturl.Values{}
	params.Add("service", comm)
	fullJaegerURL := fmt.Sprintf("%s?%s", jaegerQueryURL, params.Encode())

	// Test 1: tools/call with a known tool — verify MCP span attributes.
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		resp, err := mcpCall(address, "tools/call", 1, map[string]any{"name": "get-weather"})
		require.NoError(ct, err)
		require.Equal(ct, http.StatusOK, resp.StatusCode)

		resp, err = http.Get(fullJaegerURL) //nolint:noctx
		require.NoError(ct, err)
		if resp == nil {
			return
		}
		require.Equal(ct, http.StatusOK, resp.StatusCode)

		require.NoError(ct, json.NewDecoder(resp.Body).Decode(&tq))

		// Find traces with MCP method attribute
		traces := tq.FindBySpan(jaeger.Tag{Key: "mcp.method.name", Type: "string", Value: "tools/call"})
		require.GreaterOrEqual(ct, len(traces), 1)

		lastTrace := traces[len(traces)-1]
		require.GreaterOrEqual(ct, len(lastTrace.Spans), 1)
		span := lastTrace.Spans[0]

		assert.Equal(ct, "execute_tool get-weather", span.OperationName)

		tag, found := jaeger.FindIn(span.Tags, "mcp.method.name")
		assert.True(ct, found, "mcp.method.name tag not found")
		assert.Equal(ct, "tools/call", tag.Value)

		tag, found = jaeger.FindIn(span.Tags, "gen_ai.operation.name")
		assert.True(ct, found, "gen_ai.operation.name tag not found")
		assert.Equal(ct, "execute_tool", tag.Value)

		tag, found = jaeger.FindIn(span.Tags, "gen_ai.tool.name")
		assert.True(ct, found, "gen_ai.tool.name tag not found")
		assert.Equal(ct, "get-weather", tag.Value)

		tag, found = jaeger.FindIn(span.Tags, "mcp.session.id")
		assert.True(ct, found, "mcp.session.id tag not found")
		assert.NotEmpty(ct, tag.Value)

		tag, found = jaeger.FindIn(span.Tags, "jsonrpc.request.id")
		assert.True(ct, found, "jsonrpc.request.id tag not found")
		assert.Equal(ct, "1", tag.Value)
	}, testTimeout, 100*time.Millisecond)

	// Test 2: tools/call with an unknown tool — verify MCP error span attributes.
	var tqErr jaeger.TracesQuery
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		resp, err := mcpCall(address, "tools/call", 2, map[string]any{"name": "nonexistent"})
		require.NoError(ct, err)
		require.Equal(ct, http.StatusOK, resp.StatusCode)

		resp, err = http.Get(fullJaegerURL) //nolint:noctx
		require.NoError(ct, err)
		if resp == nil {
			return
		}
		require.Equal(ct, http.StatusOK, resp.StatusCode)

		require.NoError(ct, json.NewDecoder(resp.Body).Decode(&tqErr))

		// Find traces with the error tool call
		traces := tqErr.FindBySpan(
			jaeger.Tag{Key: "mcp.method.name", Type: "string", Value: "tools/call"},
			jaeger.Tag{Key: "gen_ai.tool.name", Type: "string", Value: "nonexistent"},
		)
		require.GreaterOrEqual(ct, len(traces), 1)

		lastTrace := traces[len(traces)-1]
		require.GreaterOrEqual(ct, len(lastTrace.Spans), 1)
		span := lastTrace.Spans[0]

		assert.Equal(ct, "execute_tool nonexistent", span.OperationName)

		// Span status should be error (MCP JSON-RPC error)
		tag, found := jaeger.FindIn(span.Tags, "otel.status_code")
		assert.True(ct, found, "otel.status_code tag not found")
		assert.Equal(ct, "ERROR", tag.Value)

		tag, found = jaeger.FindIn(span.Tags, "rpc.response.status_code")
		assert.True(ct, found, "rpc.response.status_code tag not found")
		assert.Equal(ct, "-32602", tag.Value)

		tag, found = jaeger.FindIn(span.Tags, "error.message")
		assert.True(ct, found, "error.message tag not found")
		assert.Contains(ct, tag.Value.(string), "Unknown tool")
	}, testTimeout, 100*time.Millisecond)
}

func testPythonMCPInitialize(t *testing.T) {
	const (
		comm    = "python3.14"
		address = "http://localhost:8381/mcp"
	)

	var tq jaeger.TracesQuery
	params := neturl.Values{}
	params.Add("service", comm)
	fullJaegerURL := fmt.Sprintf("%s?%s", jaegerQueryURL, params.Encode())

	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		resp, err := mcpCall(address, "initialize", 10, map[string]any{
			"protocolVersion": "2025-03-26",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "TestClient", "version": "1.0"},
		})
		require.NoError(ct, err)
		require.Equal(ct, http.StatusOK, resp.StatusCode)

		resp, err = http.Get(fullJaegerURL) //nolint:noctx
		require.NoError(ct, err)
		if resp == nil {
			return
		}
		require.Equal(ct, http.StatusOK, resp.StatusCode)

		require.NoError(ct, json.NewDecoder(resp.Body).Decode(&tq))

		traces := tq.FindBySpan(jaeger.Tag{Key: "mcp.method.name", Type: "string", Value: "initialize"})
		require.GreaterOrEqual(ct, len(traces), 1)

		lastTrace := traces[len(traces)-1]
		require.GreaterOrEqual(ct, len(lastTrace.Spans), 1)
		span := lastTrace.Spans[0]

		assert.Equal(ct, "initialize", span.OperationName)

		tag, found := jaeger.FindIn(span.Tags, "mcp.method.name")
		assert.True(ct, found, "mcp.method.name tag not found")
		assert.Equal(ct, "initialize", tag.Value)

		tag, found = jaeger.FindIn(span.Tags, "gen_ai.operation.name")
		assert.True(ct, found, "gen_ai.operation.name tag not found")
		assert.Equal(ct, "initialize", tag.Value)

		tag, found = jaeger.FindIn(span.Tags, "mcp.protocol.version")
		assert.True(ct, found, "mcp.protocol.version tag not found")
		assert.Equal(ct, "2025-03-26", tag.Value)

		tag, found = jaeger.FindIn(span.Tags, "jsonrpc.request.id")
		assert.True(ct, found, "jsonrpc.request.id tag not found")
		assert.Equal(ct, "10", tag.Value)
	}, testTimeout, 100*time.Millisecond)
}
