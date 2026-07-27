// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package ebpfcommon // import "go.opentelemetry.io/obi/pkg/ebpf/common"

import (
	"bytes"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/hashicorp/golang-lru/v2/expirable"

	"go.opentelemetry.io/obi/pkg/appolly/app/request"
	"go.opentelemetry.io/obi/pkg/internal/largebuf"
)

// Deferred completion of kprobe/SSL HTTP client responses.
//
// The HTTP span event (EVENT_K_HTTP_REQUEST) and the response body chunk events
// (EVENT_TCP_LARGE_BUFFER) travel on the same ring buffer but can be emitted
// from different CPUs when the request is finished out-of-band. That lets the
// span event be drained before the trailing chunk events, so the span is built
// from a missing or truncated response buffer — dropping the streaming SSE tail
// (finish_reason / usage). See the in-band eBPF finish in protocol_http.h for the
// case it *can* fix; this handles the residual where the terminator never reached
// the eBPF emit path.
//
// When the response buffer is missing or an SSE/chunked stream has not yet
// reached its end marker, the span is parked here keyed by (traceID, conn). The
// trailing chunk events land in appendTCPLargeBuffer and, once the buffer looks
// complete, the parked span is built and emitted. A short TTL flushes anything
// that never completes as a best-effort span, so nothing is ever dropped.

const maxPendingHTTPResponses = 2048

// pendingHTTPResponseTimeout bounds how long we wait for the trailing response
// chunk events before emitting a best-effort span. The common (racing) case
// completes in microseconds via appendTCPLargeBuffer; this only caps the
// pathological "tail never arrives" case.
const pendingHTTPResponseTimeout = 2 * time.Second

type pendingHTTPRespKey struct {
	traceID [16]uint8
	conn    BpfConnectionInfoT
}

type pendingHTTPResp struct {
	event   BPFHTTPInfo
	emitted atomic.Bool
}

// httpResponseRawComplete reports whether a captured raw HTTP response looks like
// a fully-received stream. Non-chunked / non-SSE responses are complete as soon
// as the buffer is present; chunked SSE streams are complete once the OpenAI SSE
// end marker ("[DONE]") or the chunked last-chunk terminator ("0\r\n\r\n") is
// present at the tail.
func httpResponseRawComplete(raw []byte) bool {
	if len(raw) == 0 {
		return false
	}
	// OpenAI-compatible SSE end marker (DashScope, OpenAI, ...).
	if bytes.Contains(raw, []byte("[DONE]")) {
		return true
	}
	trimmed := bytes.TrimRight(raw, "\x00")
	// Chunked last-chunk terminator at the very end of the capture.
	if bytes.HasSuffix(trimmed, []byte("0\r\n\r\n")) {
		return true
	}
	// Only chunked / event-stream responses can arrive incrementally with a
	// trailing tail; everything else is complete once its buffer is present.
	if !bytes.Contains(raw, []byte("chunked")) && !bytes.Contains(raw, []byte("text/event-stream")) {
		return true
	}
	return false
}

// httpResponseState peeks (without consuming) the response large buffer for the
// given HTTP event and reports whether it is present and whether it looks
// complete.
func (ctx *EBPFParseContext) httpResponseState(event *BPFHTTPInfo) (complete, present bool) {
	isClient := isClientEvent(event.Type)
	buf, ok := peekTCPLargeBuffer(ctx, event.Tp.TraceId, packetTypeResponse,
		directionByPacketType(packetTypeResponse, isClient), event.ConnInfo, ProtocolTypeHTTP)
	if !ok || buf == nil {
		return false, false
	}
	return httpResponseRawComplete(buf.UnsafeView()), true
}

// maybeDeferHTTPResponse parks an HTTP client response span when its body has not
// been fully captured yet, returning true if the span was deferred (and the
// caller must not emit it now). The trailing chunk events will complete it in
// appendTCPLargeBuffer, or the TTL will flush a best-effort span.
func (ctx *EBPFParseContext) maybeDeferHTTPResponse(event *BPFHTTPInfo) bool {
	if ctx == nil || ctx.pendingHTTPResponses == nil {
		return false
	}
	// Only client responses carrying large buffers with payload extraction on can
	// have a droppable streaming tail; leave everything else on the fast path.
	if event.HasLargeBuffers != 1 || !isClientEvent(event.Type) || !ctx.payloadExtraction.Enabled() {
		return false
	}

	key := pendingHTTPRespKey{traceID: event.Tp.TraceId, conn: event.ConnInfo}
	if ctx.pendingHTTPResponses.Contains(key) {
		// Already parked (duplicate span event); let completion/flush handle it.
		return true
	}

	complete, present := ctx.httpResponseState(event)
	if complete {
		return false
	}

	pending := &pendingHTTPResp{event: *event}
	ctx.pendingHTTPResponses.Add(key, pending)
	slog.Debug("obi_defer park pending HTTP response",
		"reason", deferReason(present),
		"traceID", event.Tp.TraceId,
		"dport", event.ConnInfo.D_port)
	return true
}

func deferReason(present bool) string {
	if present {
		return "incomplete"
	}
	return "missing"
}

// tryCompletePendingHTTPResponse is called after a response chunk is appended. If
// a span is parked for this (traceID, conn) and its buffer now looks complete, it
// is built and returned for emission.
func (ctx *EBPFParseContext) tryCompletePendingHTTPResponse(traceID [16]uint8, conn BpfConnectionInfoT) (request.Span, bool) {
	if ctx == nil || ctx.pendingHTTPResponses == nil {
		return request.Span{}, false
	}
	key := pendingHTTPRespKey{traceID: traceID, conn: conn}
	pending, ok := ctx.pendingHTTPResponses.Get(key)
	if !ok || pending == nil || pending.emitted.Load() {
		return request.Span{}, false
	}
	if !ctx.httpResponseBufferComplete(&pending.event) {
		return request.Span{}, false
	}
	if !pending.emitted.CompareAndSwap(false, true) {
		return request.Span{}, false
	}
	ctx.pendingHTTPResponses.Remove(key)

	span, ignore, err := buildHTTPInfoSpan(ctx, &pending.event)
	if err != nil || ignore {
		slog.Debug("obi_defer complete build failed", "err", err, "ignore", ignore, "traceID", traceID)
		return request.Span{}, false
	}
	slog.Debug("obi_defer complete pending HTTP response via chunk",
		"traceID", traceID, "dport", conn.D_port)
	return span, true
}

func (ctx *EBPFParseContext) httpResponseBufferComplete(event *BPFHTTPInfo) bool {
	complete, _ := ctx.httpResponseState(event)
	return complete
}

// deferredHTTPResponseHandler flushes a parked span whose trailing chunks never
// arrived, building a best-effort span from whatever was captured.
func deferredHTTPResponseHandler(parseCtx *EBPFParseContext) func(pendingHTTPRespKey, *pendingHTTPResp) {
	return func(key pendingHTTPRespKey, pending *pendingHTTPResp) {
		if pending == nil || !pending.emitted.CompareAndSwap(false, true) {
			return
		}
		span, ignore, err := buildHTTPInfoSpan(parseCtx, &pending.event)
		if err != nil || ignore {
			slog.Debug("obi_defer flush build failed", "err", err, "ignore", ignore, "traceID", key.traceID)
			return
		}
		slog.Debug("obi_defer flush pending HTTP response on timeout",
			"traceID", key.traceID, "dport", key.conn.D_port)
		parseCtx.emitExtraSpans(span)
	}
}

// peekTCPLargeBuffer returns the large buffer for a key without consuming it
// (unlike extractTCPLargeBuffer, which removes it).
func peekTCPLargeBuffer(
	parseCtx *EBPFParseContext,
	traceID [16]uint8,
	packetType, direction uint8,
	connInfo BpfConnectionInfoT,
	protocolType uint8,
) (*largebuf.LargeBuffer, bool) {
	key := largeBufferKey{
		traceID:    traceID,
		packetType: packetType,
		direction:  direction,
		connInfo:   connInfo,
		kind:       protocolToLargeBufferKind(protocolType),
	}
	return parseCtx.largeBuffers.Get(key)
}

// newPendingHTTPResponses builds the parking LRU used by maybeDeferHTTPResponse.
func newPendingHTTPResponses(parseCtx *EBPFParseContext) *expirable.LRU[pendingHTTPRespKey, *pendingHTTPResp] {
	return expirable.NewLRU[pendingHTTPRespKey, *pendingHTTPResp](
		maxPendingHTTPResponses,
		deferredHTTPResponseHandler(parseCtx),
		pendingHTTPResponseTimeout,
	)
}
