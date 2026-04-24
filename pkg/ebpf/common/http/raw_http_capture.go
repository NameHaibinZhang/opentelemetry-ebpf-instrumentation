// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package ebpfcommon // import "go.opentelemetry.io/obi/pkg/ebpf/common/http"

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/textproto"
	"strconv"
	"strings"
)

// TryQwenSpanWithRawResponse attempts Qwen enrichment when net/http failed to
// parse the response (e.g. malformed chunked framing) but we still have the raw
// capture. preAccum must already be Finalize()d by the caller if non-nil, or
// pass a live QwenStreamAccum and it will be finalized here once.
func TryQwenSpanWithRawResponse(
	base *request.Span,
	req *http.Request,
	rawResponse []byte,
	streamAccum *QwenStreamAccum,
) (request.Span, bool) {
	if base == nil || req == nil || len(rawResponse) == 0 {
		return request.Span{}, false
	}

	var pre *VendorOpenAI
	if streamAccum != nil {
		pre = streamAccum.Finalize()
	}

	resp, err := buildHTTPResponseFromRawCapture(rawResponse)
	if err != nil {
		if pre != nil && qwenVendorCaptureNonEmpty(pre) {
			return qwenSpanFromAccumOnly(base, req, pre)
		}
		return request.Span{}, false
	}

	if !isQwen(resp.Header) {
		return request.Span{}, false
	}

	span, ok := QwenSpan(base, req, resp, nil)
	if !ok {
		if pre != nil && qwenVendorCaptureNonEmpty(pre) {
			return qwenSpanFromAccumOnly(base, req, pre)
		}
		return request.Span{}, false
	}

	if pre != nil && span.GenAI != nil && span.GenAI.Qwen != nil {
		merged := mergeQwenVendorSnapshot(pre, span.GenAI.Qwen)
		if merged != nil {
			*span.GenAI.Qwen = *merged
		}
	}
	return span, true
}

func qwenSpanFromAccumOnly(base *request.Span, req *http.Request, pre *VendorOpenAI) (request.Span, bool) {
	if base == nil || req == nil || pre == nil || !qwenVendorCaptureNonEmpty(pre) {
		return request.Span{}, false
	}
	path := qwenRequestPath(req)
	reqB, err := io.ReadAll(req.Body)
	if err != nil && len(reqB) == 0 {
		slog.Debug("Qwen accum-only: request body unavailable", "path", path, "error", err)
		return request.Span{}, false
	}
	req.Body = io.NopCloser(bytes.NewBuffer(reqB))

	var parsedRequest OpenAIInput
	if !unmarshalQwenOpenAIInput(reqB, &parsedRequest) {
		slog.Debug("Qwen accum-only: failed to parse request")
	}
	if parsedRequest.Model == "" {
		window := reqB
		if len(window) > modelSearchWindow {
			window = window[:modelSearchWindow]
		}
		if matches := modelFieldRegexp.FindSubmatch(window); len(matches) == 2 {
			parsedRequest.Model = strings.TrimSpace(string(matches[1]))
		}
	}

	out := *pre
	out.Request = parsedRequest
	if out.OperationName == "" {
		out.OperationName = extractQwenOperation(req)
	}
	if out.ResponseModel == "" {
		out.ResponseModel = parsedRequest.Model
	}
	if parsedRequest.Model == "" {
		parsedRequest.Model = out.ResponseModel
		out.Request = parsedRequest
	}

	base.SubType = HTTPSubtypeQwen
	base.GenAI = &GenAI{Qwen: &out}
	return *base, true
}

// qwenVendorCaptureNonEmpty reports whether incremental SSE parsing produced
// anything worth keeping when the HTTP body reader failed.
func qwenVendorCaptureNonEmpty(v *VendorOpenAI) bool {
	if v == nil {
		return false
	}
	if v.Usage.GetInputTokens() > 0 || v.Usage.GetOutputTokens() > 0 || v.Usage.TotalTokens > 0 {
		return true
	}
	if v.ID != "" {
		return true
	}
	out := strings.TrimSpace(v.GetOutput())
	if out != "" && out != "null" && out != "[]" {
		return true
	}
	return false
}

func buildHTTPResponseFromRawCapture(raw []byte) (*http.Response, error) {
	idx := bytes.Index(raw, []byte("\r\n\r\n"))
	if idx < 0 {
		return nil, errors.New("raw HTTP response: missing header terminator")
	}
	headerBlock := raw[:idx]
	body := raw[idx+4:]

	tp := textproto.NewReader(bufio.NewReader(bytes.NewReader(headerBlock)))
	statusLine, err := tp.ReadLine()
	if err != nil {
		return nil, fmt.Errorf("status line: %w", err)
	}
	const prefix = "HTTP/"
	if len(statusLine) < len(prefix) || !strings.HasPrefix(statusLine, prefix) {
		return nil, errors.New("raw HTTP response: not HTTP status line")
	}

	code := 0
	fields := strings.Fields(statusLine)
	if len(fields) >= 2 {
		code, _ = strconv.Atoi(fields[1])
	}
	if code == 0 {
		code = http.StatusOK
	}

	mimeHdr, err := tp.ReadMIMEHeader()
	if err != nil {
		return nil, fmt.Errorf("mime header: %w", err)
	}
	hdr := http.Header(mimeHdr)

	body = maybeDecodeChunkedBody(body, hdr)
	if enc := hdr.Get("Content-Encoding"); enc != "" && len(body) > 0 {
		dec, err := decompressBody(enc, body)
		if err != nil {
			return nil, fmt.Errorf("decompress: %w", err)
		}
		body = dec
	}

	return &http.Response{
		Status:     fmt.Sprintf("%d %s", code, http.StatusText(code)),
		StatusCode: code,
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     hdr,
		Body:       io.NopCloser(bytes.NewReader(body)),
	}, nil
}

func maybeDecodeChunkedBody(body []byte, hdr http.Header) []byte {
	te := strings.ToLower(hdr.Get("Transfer-Encoding"))
	if !strings.Contains(te, "chunked") {
		return body
	}
	dec := decodeChunkedLenient(body)
	if len(dec) == 0 && len(body) > 0 {
		return body
	}
	return dec
}

// decodeChunkedLenient decodes chunked transfer coding; on any framing error it
// returns the original slice so callers can still scan for SSE payloads.
func decodeChunkedLenient(data []byte) []byte {
	if len(data) == 0 {
		return data
	}
	br := bufio.NewReader(bytes.NewReader(data))
	var out bytes.Buffer
	for {
		lineBytes, err := br.ReadBytes('\n')
		if err != nil {
			if out.Len() > 0 {
				return out.Bytes()
			}
			return data
		}
		line := strings.TrimSpace(string(lineBytes))
		if line == "" {
			continue
		}
		hexPart := strings.TrimSpace(strings.SplitN(line, ";", 2)[0])
		n64, err := strconv.ParseUint(hexPart, 16, 64)
		if err != nil {
			if out.Len() > 0 {
				return out.Bytes()
			}
			return data
		}
		if n64 == 0 {
			break
		}
		if n64 > 16<<20 {
			return data
		}
		n := int(n64)
		if _, err := io.CopyN(&out, br, int64(n)); err != nil {
			return data
		}
		// chunk-end CRLF
		if _, err := br.ReadBytes('\n'); err != nil {
			break
		}
	}
	return out.Bytes()
}
