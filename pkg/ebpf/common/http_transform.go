// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package ebpfcommon // import "go.opentelemetry.io/obi/pkg/ebpf/common"

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"

	"go.opentelemetry.io/obi/pkg/appolly/app"
	"go.opentelemetry.io/obi/pkg/appolly/app/request"
	ebpfhttp "go.opentelemetry.io/obi/pkg/ebpf/common/http"
	"go.opentelemetry.io/obi/pkg/internal/ebpf/ringbuf"
	"go.opentelemetry.io/obi/pkg/internal/largebuf"
)

func removeQuery(url string) string {
	idx := strings.IndexByte(url, '?')
	if idx > 0 {
		return url[:idx]
	}
	return url
}

type HTTPInfo struct {
	BPFHTTPInfo
	Method     string
	URL        string
	Host       string
	Peer       string
	HeaderHost string
	Body       string
}

// misses serviceID
func httpInfoToSpanLegacy(info *HTTPInfo) request.Span {
	scheme := "http"
	if info.Ssl == 1 {
		scheme = "https"
	}

	return request.Span{
		Type:           request.EventType(info.Type),
		Method:         info.Method,
		Path:           removeQuery(info.URL),
		FullPath:       info.URL,
		Peer:           info.Peer,
		PeerPort:       int(info.ConnInfo.S_port),
		Host:           info.Host,
		HostPort:       int(info.ConnInfo.D_port),
		ContentLength:  int64(info.Len),
		ResponseLength: int64(info.RespLen),
		RequestStart:   int64(info.ReqMonotimeNs),
		Start:          int64(info.StartMonotimeNs),
		End:            int64(info.EndMonotimeNs),
		Status:         int(info.Status),
		TraceID:        info.Tp.TraceId,
		SpanID:         info.Tp.SpanId,
		ParentSpanID:   info.Tp.ParentId,
		TraceFlags:     info.Tp.Flags,
		Pid: request.PidInfo{
			HostPID:   app.PID(info.Pid.HostPid),
			UserPID:   app.PID(info.Pid.UserPid),
			Namespace: info.Pid.Ns,
		},
		Statement: scheme + request.SchemeHostSeparator + info.HeaderHost,
	}
}

func httpRequestResponseToSpan(parseCtx *EBPFParseContext, event *BPFHTTPInfo, req *http.Request, resp *http.Response) request.Span {
	defer req.Body.Close()
	defer resp.Body.Close()
	slog.Debug(
		"Entering httpRequestResponseToSpan",
		"traceID", event.Tp.TraceId,
		"method", req.Method,
		"path", req.URL.String(),
		"status", resp.StatusCode,
	)

	peer, host := (*BPFConnInfo)(&event.ConnInfo).reqHostInfo()

	scheme := req.URL.Scheme
	if scheme == "" {
		if event.Ssl == 1 {
			scheme = "https"
		} else {
			scheme = "http"
		}
	}

	// Make sure the content length is non-zero
	reqContentLen := req.ContentLength
	if reqContentLen <= 0 {
		reqContentLen = int64(event.Len)
	}

	// The response len can be -1 if we use chunked
	// responses
	respContentLen := resp.ContentLength
	if respContentLen <= 0 {
		respContentLen = int64(event.RespLen)
	}

	reqType := request.EventType(event.Type)
	headerHost := req.Host
	if headerHost == "" && reqType == request.EventTypeHTTPClient {
		headerHost, _ = httpHostFromBuf(event.Buf[:])
	}

	// FullPath matches net/url.URL.String() (full URL or request-target), not RequestURI().
	httpSpan := request.Span{
		Type:           reqType,
		Method:         req.Method,
		Path:           removeQuery(req.URL.String()),
		FullPath:       req.URL.String(),
		Peer:           peer,
		PeerPort:       int(event.ConnInfo.S_port),
		Host:           host,
		HostPort:       int(event.ConnInfo.D_port),
		ContentLength:  reqContentLen,
		ResponseLength: respContentLen,
		RequestStart:   int64(event.ReqMonotimeNs),
		Start:          int64(event.StartMonotimeNs),
		End:            int64(event.EndMonotimeNs),
		Status:         resp.StatusCode,
		TraceID:        event.Tp.TraceId,
		SpanID:         event.Tp.SpanId,
		ParentSpanID:   event.Tp.ParentId,
		TraceFlags:     event.Tp.Flags,
		Pid: request.PidInfo{
			HostPID:   app.PID(event.Pid.HostPid),
			UserPID:   app.PID(event.Pid.UserPid),
			Namespace: event.Pid.Ns,
		},
		Statement: scheme + request.SchemeHostSeparator + headerHost,
	}

	if isClientEvent(event.Type) && parseCtx != nil && parseCtx.payloadExtraction.HTTP.AWS.Enabled {
		slog.Debug("Evaluating AWS client parser", "traceID", event.Tp.TraceId, "path", httpSpan.Path)
		span, ok := ebpfhttp.AWSS3Span(&httpSpan, req, resp)
		if ok {
			return span
		}

		span, ok = ebpfhttp.AWSSQSSpan(&httpSpan, req, resp)
		if ok {
			return span
		}
	}

	if !isClientEvent(event.Type) && parseCtx != nil && parseCtx.payloadExtraction.HTTP.GraphQL.Enabled {
		span, ok := ebpfhttp.GraphQLSpan(&httpSpan, req, resp)
		if ok {
			return span
		}
	}

	if isClientEvent(event.Type) && parseCtx != nil && parseCtx.payloadExtraction.HTTP.Elasticsearch.Enabled {
		slog.Debug("Evaluating Elasticsearch client parser", "traceID", event.Tp.TraceId, "path", httpSpan.Path)
		span, ok := ebpfhttp.ElasticsearchSpan(&httpSpan, req, resp)
		if ok {
			return span
		}
	}

	if isClientEvent(event.Type) && parseCtx != nil && parseCtx.payloadExtraction.HTTP.SQLPP.Enabled {
		slog.Debug("Evaluating SQLPP client parser", "traceID", event.Tp.TraceId, "path", httpSpan.Path)
		span, ok := ebpfhttp.SQLPPSpan(&httpSpan, req, resp, parseCtx.payloadExtraction.HTTP.SQLPP.EndpointPatterns)
		if ok {
			return span
		}
	}

	if isClientEvent(event.Type) && parseCtx != nil && parseCtx.payloadExtraction.HTTP.GenAI.OpenAI.Enabled {
		slog.Debug("Evaluating OpenAI parser", "traceID", event.Tp.TraceId, "path", httpSpan.Path)
		span, ok := ebpfhttp.OpenAISpan(&httpSpan, req, resp)
		if ok {
			slog.Debug("OpenAI parser matched", "traceID", event.Tp.TraceId, "path", span.Path)
			return span
		}
		slog.Debug("OpenAI parser did not match", "traceID", event.Tp.TraceId, "path", httpSpan.Path)
	}

	if isClientEvent(event.Type) && parseCtx != nil && parseCtx.payloadExtraction.HTTP.GenAI.Anthropic.Enabled {
		slog.Debug("Evaluating Anthropic parser", "traceID", event.Tp.TraceId, "path", httpSpan.Path)
		span, ok := ebpfhttp.AnthropicSpan(&httpSpan, req, resp)
		if ok {
			return span
		}
	}

	if isClientEvent(event.Type) && parseCtx != nil && parseCtx.payloadExtraction.HTTP.GenAI.Gemini.Enabled {
		slog.Debug("Evaluating Gemini parser", "traceID", event.Tp.TraceId, "path", httpSpan.Path)
		span, ok := ebpfhttp.GeminiSpan(&httpSpan, req, resp)
		if ok {
			return span
		}
	}

	if isClientEvent(event.Type) && parseCtx != nil && parseCtx.payloadExtraction.HTTP.GenAI.Qwen.Enabled {
		slog.Debug("Evaluating Qwen parser", "traceID", event.Tp.TraceId, "path", httpSpan.Path)
		span, ok := ebpfhttp.QwenSpan(&httpSpan, req, resp)
		if ok {
			slog.Debug("Qwen parser matched", "traceID", event.Tp.TraceId, "path", span.Path)
			return span
		}
		slog.Debug("Qwen parser did not match", "traceID", event.Tp.TraceId, "path", httpSpan.Path)
	}

	if isClientEvent(event.Type) && parseCtx != nil && parseCtx.payloadExtraction.HTTP.GenAI.Bedrock.Enabled {
		slog.Debug("Evaluating Bedrock parser", "traceID", event.Tp.TraceId, "path", httpSpan.Path)
		span, ok := ebpfhttp.BedrockSpan(&httpSpan, req, resp)
		if ok {
			return span
		}
	}
	slog.Debug("Evaluating JSONRPC parser", "traceID", event.Tp.TraceId, "path", httpSpan.Path)
	if parseCtx != nil && parseCtx.payloadExtraction.HTTP.JSONRPC.Enabled {
		span, ok := ebpfhttp.JSONRPCSpan(&httpSpan, req, resp)
		if ok {
			return span
		}
	}

	if parseCtx != nil && parseCtx.httpEnricher != nil {
		parseCtx.httpEnricher.Enrich(&httpSpan, req, resp)
	}

	return httpSpan
}

func ReadHTTPInfoIntoSpan(parseCtx *EBPFParseContext, record *ringbuf.Record, filter ServiceFilter) (request.Span, bool, error) {
	event, err := ReinterpretCast[BPFHTTPInfo](record.RawSample)
	if err != nil {
		return request.Span{}, true, err
	}

	// Generated by Go instrumentation
	if event.EventSource == GenericEventSourceTypeKProbes && !filter.ValidPID(app.PID(event.Pid.UserPid), event.Pid.Ns, PIDTypeKProbes) {
		return request.Span{}, true, nil
	}

	return HTTPInfoEventToSpan(parseCtx, event)
}

func HTTPInfoEventToSpan(parseCtx *EBPFParseContext, event *BPFHTTPInfo) (request.Span, bool, error) {
	var (
		requestBuffer, responseBuffer *largebuf.LargeBuffer
		hasResponse                   bool
		isClient                      = isClientEvent(event.Type)
	)

	slog.Debug("Event", "traceID", event.Tp.TraceId, "conn", event.ConnInfo, "buf", event.Buf[:])

	if event.HasLargeBuffers == 1 {
		b, ok := extractTCPLargeBuffer(parseCtx, event.Tp.TraceId, packetTypeRequest, directionByPacketType(packetTypeRequest, isClient), event.ConnInfo)
		if ok {
			requestBuffer = b
		} else {
			slog.Debug("missing large buffer for HTTP request", "traceID", event.Tp.TraceId, "conn", event.ConnInfo, "packetType", packetTypeRequest)
			requestBuffer = largebuf.NewLargeBufferFrom(event.Buf[:])
		}

		b, ok = extractTCPLargeBuffer(parseCtx, event.Tp.TraceId, packetTypeResponse, directionByPacketType(packetTypeResponse, isClient), event.ConnInfo)
		if ok {
			responseBuffer = b
			hasResponse = true
		} else {
			slog.Debug("missing large buffer for HTTP response", "traceID", event.Tp.TraceId, "conn", event.ConnInfo, "packetType", packetTypeResponse)
		}
	} else {
		requestBuffer = largebuf.NewLargeBufferFrom(event.Buf[:])
	}

	// Defense-in-depth: if the captured request or response buffer begins with
	// a TLS record header (e.g. 0x17 0x03 application_data), the eBPF side has
	// mixed plaintext and ciphertext in the same large buffer. This should no
	// longer happen thanks to the SSL-mismatch guard in http_send_large_buffer,
	// but we still sanitise here so that older kernels or out-of-tree BPF
	// objects never produce malformed spans.
	if isLikelyTLSRecord(bufferHeadBytes(requestBuffer, 5)) {
		slog.Debug(
			"discarding HTTP request buffer: looks like raw TLS record (plaintext/ciphertext mix)",
			"traceID", event.Tp.TraceId,
			"conn", event.ConnInfo,
		)
		requestBuffer = largebuf.NewLargeBufferFrom(event.Buf[:])
	}
	if hasResponse && isLikelyTLSRecord(bufferHeadBytes(responseBuffer, 5)) {
		slog.Debug(
			"discarding HTTP response buffer: looks like raw TLS record (plaintext/ciphertext mix)",
			"traceID", event.Tp.TraceId,
			"conn", event.ConnInfo,
		)
		responseBuffer = nil
		hasResponse = false
	}

	if parseCtx != nil && !parseCtx.payloadExtraction.Enabled() {
		// There's no need to parse HTTP headers/body,
		// create the span directly.
		return httpRequestToSpan(event, requestBuffer), false, nil
	}

	if !hasResponse {
		// Response payload unavailable (e.g. missing large response buffer).
		// Try strict request-only Qwen stream fallback for client spans.
		httpSpan := httpRequestToSpan(event, requestBuffer)
		slog.Debug(
			"HTTP response payload unavailable; entering request-only fallback path",
			"traceID", event.Tp.TraceId,
			"isClient", isClient,
			"qwenEnabled", parseCtx != nil && parseCtx.payloadExtraction.HTTP.GenAI.Qwen.Enabled,
			"path", httpSpan.Path,
		)
		if isClient &&
			parseCtx != nil &&
			parseCtx.payloadExtraction.HTTP.GenAI.Qwen.Enabled {
			reqReader := requestBuffer.NewReader()
			req, err := http.ReadRequest(bufio.NewReader(&reqReader))
			if err == nil {
				if span, ok := ebpfhttp.QwenStreamRequestSpan(&httpSpan, req); ok {
					slog.Debug("Qwen request-only fallback matched", "traceID", event.Tp.TraceId, "path", span.Path)
					return span, false, nil
				}
				slog.Debug("Qwen request-only fallback did not match", "traceID", event.Tp.TraceId, "path", httpSpan.Path)
			} else {
				slog.Debug("failed to parse request for Qwen request-only fallback", "traceID", event.Tp.TraceId, "error", err)
			}
		}
		return httpSpan, false, nil
	}

	// http.ReadRequest requires a *bufio.Reader; that one allocation is unavoidable.
	reqReader := requestBuffer.NewReader()
	req, err := http.ReadRequest(bufio.NewReader(&reqReader))
	resp, err2 := httpSafeParseResponse(responseBuffer, req)
	if err != nil || err2 != nil {
		slog.Debug("error while parsing http request or response, falling back to manual HTTP info parsing", "reqErr", err, "respErr", err2)
		return httpRequestToSpan(event, requestBuffer), false, nil
	}

	return httpRequestResponseToSpan(parseCtx, event, req, resp), false, nil
}

// HTTP response buffers might have been sent incomplete, before the full body.
// Try to parse the original buffer first, if an EOF is encountered, append an empty
// body to the buffer and try again.
func httpSafeParseResponse(responseBuffer *largebuf.LargeBuffer, req *http.Request) (*http.Response, error) {
	r := responseBuffer.NewReader()
	rd := bufio.NewReader(&r)
	resp, err := http.ReadResponse(rd, req)
	if err != nil && errors.Is(err, io.ErrUnexpectedEOF) {
		// Append empty body terminator and retry, reusing the same reader (preserves scratch).
		responseBuffer.AppendChunk([]byte("\r\n\r\n"))
		r.Reset()
		rd.Reset(&r)
		return http.ReadResponse(rd, req)
	}
	return resp, err
}

func httpRequestToSpan(event *BPFHTTPInfo, requestBuffer *largebuf.LargeBuffer) request.Span {
	var (
		result     = HTTPInfo{BPFHTTPInfo: *event}
		bufHost    string
		bufPort    int
		parsedHost bool
	)

	raw := requestBuffer.UnsafeView()

	// When we can't find the connection info, we signal that through making the
	// source and destination ports equal to max short. E.g. async SSL
	if event.ConnInfo.S_port != 0 || event.ConnInfo.D_port != 0 {
		source, target := (*BPFConnInfo)(&event.ConnInfo).reqHostInfo()
		result.Host = target
		result.Peer = source
	} else {
		bufHost, bufPort = httpHostFromBuf(raw)
		parsedHost = true

		if bufPort >= 0 {
			result.Host = bufHost
			result.ConnInfo.D_port = uint16(bufPort)
		}
	}
	result.URL = httpURLFromBuf(raw)
	result.Method = httpMethodFromBuf(raw)

	if request.EventType(result.Type) == request.EventTypeHTTPClient && !parsedHost {
		bufHost, _ = httpHostFromBuf(raw)
	}

	result.HeaderHost = bufHost

	return httpInfoToSpanLegacy(&result)
}

func httpURLFromBuf(req []byte) string {
	if end := bytes.IndexByte(req, 0); end >= 0 {
		req = req[:end]
	}

	space := bytes.IndexByte(req, ' ')
	if space < 0 {
		return ""
	}

	req = req[space+1:]

	nextSpace := bytes.IndexAny(req, " \r\n")
	if nextSpace < 0 {
		return string(req)
	}

	return string(req[:nextSpace])
}

func httpMethodFromBuf(req []byte) string {
	method, _, found := bytes.Cut(req, []byte(" "))
	if !found {
		return ""
	}

	return string(method)
}

func httpHostFromBuf(req []byte) (string, int) {
	if end := bytes.IndexByte(req, 0); end >= 0 {
		req = req[:end]
	}

	idx := bytes.Index(req, []byte("Host: "))
	if idx < 0 {
		return "", -1
	}

	req = req[idx+len("Host: "):]

	// only parse full host information, partial may
	// get the wrong name or wrong port
	hostPort, _, found := bytes.Cut(req, []byte("\r"))
	if !found {
		return "", -1
	}
	host, portStr, err := net.SplitHostPort(string(hostPort))
	if err != nil {
		return string(hostPort), -1
	}

	port, _ := strconv.Atoi(portStr)

	return host, port
}

// isLikelyTLSRecord reports whether the byte slice looks like the leading
// bytes of a TLS 1.0–1.3 record header rather than a plaintext HTTP message.
//
// TLS record format (RFC 5246 §6.2.1, RFC 8446 §5.1):
//
//	struct {
//	    ContentType type;           // 1 byte, 20..23 (0x14..0x17) or 24 (0x18)
//	    ProtocolVersion version;    // 2 bytes, major 0x03 + minor 0x00..0x04
//	    uint16 length;              // 2 bytes
//	    opaque fragment[length];
//	} TLSPlaintext;
//
// A valid HTTP request begins with an ASCII method letter, and a valid HTTP
// response begins with "HTTP/". Neither collides with the record-type prefix,
// so we can reliably reject a buffer that starts with these bytes as having
// captured ciphertext instead of the plaintext payload.
func isLikelyTLSRecord(head []byte) bool {
	if len(head) < 3 {
		return false
	}
	switch head[0] {
	case 0x14, // change_cipher_spec
		0x15, // alert
		0x16, // handshake
		0x17, // application_data
		0x18: // heartbeat
		// Require TLS major version byte to avoid false positives when a
		// legitimate payload happens to start with one of the bytes above.
		return head[1] == 0x03 && head[2] <= 0x04
	}
	return false
}

// bufferHeadBytes returns up to n bytes from the start of the captured
// LargeBuffer without consuming it. It is safe to call with a nil receiver.
func bufferHeadBytes(buf *largebuf.LargeBuffer, n int) []byte {
	if buf == nil || n <= 0 {
		return nil
	}
	r := buf.NewReader()
	head := make([]byte, n)
	read, _ := io.ReadFull(&r, head)
	return head[:read]
}
