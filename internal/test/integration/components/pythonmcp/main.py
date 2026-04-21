"""
MCP (Model Context Protocol) server for integration testing.

Implements a subset of MCP methods over JSON-RPC 2.0 / HTTP
using only the Python standard library.
"""

import json
import uuid
from http.server import BaseHTTPRequestHandler, HTTPServer

SESSION_ID = str(uuid.uuid4())

KNOWN_TOOLS = {
    "get-weather": "Sunny, 72\u00b0F in the requested location",
    "calculator": "42",
}

KNOWN_RESOURCES = {
    "file:///home/user/documents/report.pdf": {
        "uri": "file:///home/user/documents/report.pdf",
        "mimeType": "application/pdf",
        "text": "Sample report content",
    },
}

KNOWN_PROMPTS = {
    "analyze-code": {
        "description": "Analyzes code for potential issues",
        "messages": [
            {
                "role": "user",
                "content": {"type": "text", "text": "Analyze this code"},
            }
        ],
    },
}


def make_response(result, req_id):
    return {"jsonrpc": "2.0", "result": result, "id": req_id}


def make_error(code, message, req_id):
    return {
        "jsonrpc": "2.0",
        "error": {"code": code, "message": message},
        "id": req_id,
    }


def handle_initialize(params, req_id):
    return make_response(
        {
            "protocolVersion": "2025-03-26",
            "capabilities": {"tools": {}, "resources": {}, "prompts": {}},
            "serverInfo": {"name": "test-mcp-server", "version": "1.0"},
        },
        req_id,
    )


def handle_tools_list(_params, req_id):
    tools = [{"name": name} for name in KNOWN_TOOLS]
    return make_response({"tools": tools}, req_id)


def handle_tools_call(params, req_id):
    name = params.get("name", "") if params else ""
    if name not in KNOWN_TOOLS:
        return make_error(-32602, f"Unknown tool: {name}", req_id)
    content = [{"type": "text", "text": KNOWN_TOOLS[name]}]
    return make_response({"content": content}, req_id)


def handle_resources_read(params, req_id):
    uri = params.get("uri", "") if params else ""
    if uri not in KNOWN_RESOURCES:
        return make_error(-32602, f"Unknown resource: {uri}", req_id)
    return make_response({"contents": [KNOWN_RESOURCES[uri]]}, req_id)


def handle_prompts_get(params, req_id):
    name = params.get("name", "") if params else ""
    if name not in KNOWN_PROMPTS:
        return make_error(-32602, f"Unknown prompt: {name}", req_id)
    return make_response(KNOWN_PROMPTS[name], req_id)


def handle_ping(_params, req_id):
    return make_response({}, req_id)


DISPATCH = {
    "initialize": handle_initialize,
    "tools/list": handle_tools_list,
    "tools/call": handle_tools_call,
    "resources/read": handle_resources_read,
    "prompts/get": handle_prompts_get,
    "ping": handle_ping,
}


class Handler(BaseHTTPRequestHandler):
    def do_POST(self):
        content_length = int(self.headers.get("Content-Length", 0))
        body = self.rfile.read(content_length).decode("utf-8")

        try:
            req = json.loads(body)
        except json.JSONDecodeError:
            self._send_json(
                make_error(-32700, "Parse error", None), 200
            )
            return

        if req.get("jsonrpc") != "2.0":
            self._send_json(
                make_error(-32600, "Invalid Request: missing jsonrpc 2.0",
                           req.get("id")),
                200,
            )
            return

        method = req.get("method", "")
        params = req.get("params")
        req_id = req.get("id")

        handler = DISPATCH.get(method)
        if handler is None:
            self._send_json(
                make_error(-32601, f"Method not found: {method}", req_id), 200
            )
            return

        resp = handler(params, req_id)
        self._send_json(resp, 200)

    def _send_json(self, obj, status):
        resp_body = json.dumps(obj).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(resp_body)))
        self.send_header("Mcp-Session-Id", SESSION_ID)
        self.end_headers()
        self.wfile.write(resp_body)

    def do_GET(self):
        if self.path == "/smoke":
            self.send_response(200)
            self.end_headers()
        else:
            self.send_response(404)
            self.end_headers()

    def log_message(self, format, *args):
        print(f"[mcp-server] {format % args}")


if __name__ == "__main__":
    server = HTTPServer(("0.0.0.0", 8080), Handler)
    print("MCP server running on port 8080")
    server.serve_forever()
