// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

/**
 * The following code is copied from bpf/generictracer/protocol_http.h and
 * adapted to run as a host unit test. The functions under test drive the
 * in-band finish of chunked (e.g. SSE streaming) responses:
 *
 *   static __always_inline bool http_chunked_token(const unsigned char *p);
 *   static __always_inline bool http_response_head_is_chunked(const void *, u32);
 *   static __always_inline bool http_response_head_is_chunked__legacy(const void *, u32);
 *   static __always_inline bool http_read_tail_is_chunk_end(const void *, u32);
 *
 * The BPF-only helpers (bpf_probe_read, bpf_clamp_umax, bpf_loop and the
 * percpu scratch map behind http_chunked_head_mem) are mocked below. The real
 * header cannot be #included directly on a non-BPF target because its map
 * definitions use SEC(".maps").
 *
 * These tests pin the behaviour relied on by the streaming-tail-loss fix
 * (PR "finish chunked responses in-band to keep streaming SSE tail"):
 *   - the "chunked" Transfer-Encoding token is detected case-insensitively
 *     anywhere within the scan window, and ignored beyond it (→ safe fallback);
 *   - the last-chunk terminator "\r\n0\r\n\r\n" is recognised ONLY when anchored
 *     at the read tail, so the same bytes appearing mid-body never misfire;
 *   - the bpf_loop and unrolled (__legacy) scanners agree on every input.
 */

#include <stdbool.h>
#include <stdint.h>
#include <stdio.h>
#include <string.h>

typedef uint8_t u8;
typedef uint16_t u16;
typedef uint32_t u32;
typedef uint64_t u64;

#ifndef __always_inline
#define __always_inline inline
#endif

// ---------------------------------------------------------------------------
// Mocks for the BPF runtime helpers used by the scanners.
// ---------------------------------------------------------------------------

static void bpf_probe_read(void *dst, u32 size, const void *src) {
    memcpy(dst, src, size);
}

#define bpf_clamp_umax(VAR, UMAX)                                                                  \
    do {                                                                                           \
        if ((VAR) > (UMAX))                                                                        \
            (VAR) = (UMAX);                                                                        \
    } while (0)

// Percpu scratch backing http_chunked_head_mem(). A single static buffer is
// enough for the single-threaded host test.
#define k_http_chunked_scan_window 256
static unsigned char g_chunked_head[k_http_chunked_scan_window];
static void *http_chunked_head_mem(void) {
    return g_chunked_head;
}

// bpf_loop(nr, cb, ctx, flags): invoke cb(index, ctx) for index in [0, nr),
// stopping early when the callback returns non-zero (mirrors the kernel helper).
typedef int (*bpf_loop_cb)(u32 index, void *data);
static long bpf_loop(u32 nr_loops, bpf_loop_cb cb, void *data, u64 flags) {
    (void)flags;
    for (u32 i = 0; i < nr_loops; i++) {
        if (cb(i, data)) {
            return i + 1;
        }
    }
    return nr_loops;
}

// ---------------------------------------------------------------------------
// Code under test (copied verbatim from protocol_http.h).
// ---------------------------------------------------------------------------

enum {
    k_http_chunked_detected = 1 << 0,
    k_http_chunked_last_seen = 1 << 1,
};

#define k_http_chunked_scan_loops (k_http_chunked_scan_window - 7 + 1)

static __always_inline bool http_chunked_token(const unsigned char *p) {
    return (p[0] | 0x20) == 'c' && (p[1] | 0x20) == 'h' && (p[2] | 0x20) == 'u' &&
           (p[3] | 0x20) == 'n' && (p[4] | 0x20) == 'k' && (p[5] | 0x20) == 'e' &&
           (p[6] | 0x20) == 'd';
}

struct chunked_scan_ctx {
    u32 n;
    bool found;
    u8 _pad[3];
};

static int chunked_match(u32 index, void *data) {
    struct chunked_scan_ctx *ctx = data;
    if (index + 7 > ctx->n) {
        return 1;
    }
    unsigned char *buf = (unsigned char *)http_chunked_head_mem();
    if (!buf) {
        return 1;
    }
    index &= (k_http_chunked_scan_window - 1);
    if (index > k_http_chunked_scan_window - 7) {
        return 1;
    }
    if (http_chunked_token(&buf[index])) {
        ctx->found = true;
        return 1;
    }
    return 0;
}

static __always_inline bool http_response_head_is_chunked(const void *u_buf, u32 len) {
    u32 n = len;
    bpf_clamp_umax(n, k_http_chunked_scan_window);
    if (n < 7) {
        return false;
    }

    unsigned char *head = (unsigned char *)http_chunked_head_mem();
    if (!head) {
        return false;
    }
    bpf_probe_read(head, k_http_chunked_scan_window, (void *)u_buf);

    u32 nr_loops = n - 7 + 1;
    bpf_clamp_umax(nr_loops, k_http_chunked_scan_loops);

    struct chunked_scan_ctx ctx = {.n = n, .found = false};
    bpf_loop(nr_loops, chunked_match, &ctx, 0);
    return ctx.found;
}

static __always_inline bool http_response_head_is_chunked__legacy(const void *u_buf, u32 len) {
    unsigned char head[k_http_chunked_scan_window];
    u32 n = len;
    bpf_clamp_umax(n, k_http_chunked_scan_window);
    if (n < 7) {
        return false;
    }
    bpf_probe_read(head, k_http_chunked_scan_window, (void *)u_buf);
    for (u32 i = 0; i + 7 <= k_http_chunked_scan_window; i++) {
        if (i + 7 > n) {
            break;
        }
        if (http_chunked_token(&head[i])) {
            return true;
        }
    }
    return false;
}

static __always_inline bool http_read_tail_is_chunk_end(const void *u_buf, u32 len) {
    if (len < 5) {
        return false;
    }
    unsigned char tail[5];
    bpf_probe_read(tail, sizeof(tail), (void *)((const u8 *)u_buf + (len - 5)));
    return tail[0] == '0' && tail[1] == '\r' && tail[2] == '\n' && tail[3] == '\r' &&
           tail[4] == '\n';
}

// ---------------------------------------------------------------------------
// Test harness.
// ---------------------------------------------------------------------------

static int failures = 0;

static void check_bool(const char *name, bool expected, bool actual) {
    if (expected != actual) {
        fprintf(stderr, "FAIL: %s\n  expected %d, got %d\n", name, expected, actual);
        failures++;
    } else {
        printf("ok: %s\n", name);
    }
}

// The real capture always hands the scanner a full read buffer; model that by
// copying the input into a zero-padded window so the fixed 256-byte
// bpf_probe_read in the scanner never reads past the test allocation.
static bool head_is_chunked(const char *s) {
    unsigned char buf[k_http_chunked_scan_window] = {0};
    u32 n = (u32)strlen(s);
    memcpy(buf, s, n > sizeof(buf) ? sizeof(buf) : n);
    bool legacy = http_response_head_is_chunked__legacy(buf, n);
    bool loop = http_response_head_is_chunked(buf, n);
    // Parity: the two specialisations must agree on every input.
    if (legacy != loop) {
        fprintf(stderr,
                "FAIL: bpf_loop/legacy disagree on %.40s (legacy=%d loop=%d)\n",
                s,
                legacy,
                loop);
        failures++;
    }
    return legacy;
}

static bool tail_is_end(const char *bytes, u32 len) {
    return http_read_tail_is_chunk_end((const unsigned char *)bytes, len);
}

// -- http_chunked_token ------------------------------------------------------

static void test_token_case_insensitive(void) {
    check_bool("token: chunked", true, http_chunked_token((const unsigned char *)"chunked"));
    check_bool("token: Chunked", true, http_chunked_token((const unsigned char *)"Chunked"));
    check_bool("token: CHUNKED", true, http_chunked_token((const unsigned char *)"CHUNKED"));
    check_bool("token: ChUnKeD", true, http_chunked_token((const unsigned char *)"ChUnKeD"));
    check_bool("token: chunker", false, http_chunked_token((const unsigned char *)"chunker"));
    check_bool("token: xhunked", false, http_chunked_token((const unsigned char *)"xhunked"));
}

// -- head detection ----------------------------------------------------------

static void test_head_detects_chunked_header(void) {
    check_bool("head: TE chunked",
               true,
               head_is_chunked("HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\n"
                               "Transfer-Encoding: chunked\r\n\r\n"));
    check_bool("head: TE Chunked (mixed case)",
               true,
               head_is_chunked("HTTP/1.1 200 OK\r\nTransfer-Encoding: Chunked\r\n\r\n"));
}

static void test_head_rejects_content_length(void) {
    check_bool("head: content-length only",
               false,
               head_is_chunked("HTTP/1.1 200 OK\r\nContent-Length: 42\r\n\r\n"));
}

static void test_head_too_short(void) {
    check_bool("head: shorter than token", false, head_is_chunked("chunk"));
}

static void test_head_token_at_window_edge(void) {
    // "chunked" whose last byte lands exactly on the final in-window position
    // must still match (position 249, token occupies 249..255).
    char buf[k_http_chunked_scan_window + 1];
    memset(buf, 'x', sizeof(buf) - 1);
    buf[sizeof(buf) - 1] = '\0';
    memcpy(buf + (k_http_chunked_scan_window - 7), "chunked", 7);
    check_bool("head: token ends at window edge", true, head_is_chunked(buf));
}

static void test_head_token_beyond_window(void) {
    // A "chunked" token that starts after the 256-byte window is not detected,
    // so such a response safely falls back to the existing out-of-band finish.
    char buf[512];
    memset(buf, 'x', sizeof(buf));
    memcpy(buf + 300, "chunked", 7);
    buf[sizeof(buf) - 1] = '\0';
    check_bool("head: token beyond scan window", false, head_is_chunked(buf));
}

// -- tail terminator ---------------------------------------------------------

static void test_tail_exact_terminator(void) {
    const char *b = "data: {\"choices\":[],\"usage\":{}}\r\n\r\n0\r\n\r\n";
    check_bool("tail: exact last-chunk terminator", true, tail_is_end(b, (u32)strlen(b)));
}

static void test_tail_full_7byte_terminator(void) {
    // The full "\r\n0\r\n\r\n" (terminator riding along with the previous chunk's
    // closing CRLF in the same read) still matches on its "0\r\n\r\n" tail.
    const char b[] = {'\r', '\n', '0', '\r', '\n', '\r', '\n'};
    check_bool("tail: full 7-byte terminator", true, tail_is_end(b, sizeof(b)));
}

static void test_tail_standalone_terminator(void) {
    // Fix A: the terminator flushed in its own SSL_read ("0\r\n\r\n", 5 bytes)
    // must now be recognised, instead of bailing on len < 7.
    const char b[] = {'0', '\r', '\n', '\r', '\n'};
    check_bool("tail: standalone 5-byte terminator", true, tail_is_end(b, sizeof(b)));
}

static void test_tail_too_short(void) {
    // Below the 5-byte terminator length: cannot match.
    const char b[] = {'\r', '\n', '\r', '\n'};
    check_bool("tail: shorter than terminator", false, tail_is_end(b, sizeof(b)));
}

static void test_tail_terminator_midbody_not_at_end(void) {
    // The terminator bytes appear mid-body but the read does NOT end on them;
    // position-anchoring must reject this so a still-continuing stream is not
    // finished early.
    const char *b = "\r\n0\r\n\r\ndata: {\"delta\":\"more\"}\r\n";
    check_bool("tail: terminator mid-body, not at tail", false, tail_is_end(b, (u32)strlen(b)));
}

static void test_tail_content_ending_crlf_only(void) {
    // A normal content chunk that ends with CRLF but is not the last chunk.
    const char *b = "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\r\n\r\n";
    check_bool("tail: content chunk ending CRLF", false, tail_is_end(b, (u32)strlen(b)));
}

static void test_tail_trailered_last_chunk_falls_back(void) {
    // A last chunk carrying trailers ("0\r\n<trailer>\r\n\r\n") does not end in
    // the no-trailer terminator, so it deliberately falls back to out-of-band.
    const char *b = "0\r\nX-Checksum: abc\r\n\r\n";
    check_bool("tail: trailered last chunk falls back", false, tail_is_end(b, (u32)strlen(b)));
}

// -- combined arming semantics (mirrors __obi_protocol_http) -----------------

// Reproduces how the response handler arms the in-band finish flags: a chunked
// head detection is required before the tail terminator is honoured.
static u8 arm_flags(const char *bytes, u32 len) {
    unsigned char window[k_http_chunked_scan_window] = {0};
    memcpy(window, bytes, len > sizeof(window) ? sizeof(window) : len);
    u8 flags = 0;
    if (len > 0 && http_response_head_is_chunked__legacy(window, len)) {
        flags |= k_http_chunked_detected;
        if (http_read_tail_is_chunk_end((const unsigned char *)bytes, len)) {
            flags |= k_http_chunked_last_seen;
        }
    }
    return flags;
}

static void test_arm_single_read_complete_stream(void) {
    // Tiny SSE response fully delivered in one read: both detected and last-seen
    // arm, so the in-band finish will fire.
    const char *b = "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n"
                    "20\r\ndata: {\"usage\":{\"total_tokens\":7}}\r\n\r\n0\r\n\r\n";
    u8 f = arm_flags(b, (u32)strlen(b));
    check_bool(
        "arm: single-read complete stream -> detected", true, (f & k_http_chunked_detected) != 0);
    check_bool(
        "arm: single-read complete stream -> last_seen", true, (f & k_http_chunked_last_seen) != 0);
}

static void test_arm_non_chunked_never_arms(void) {
    const char *b = "HTTP/1.1 200 OK\r\nContent-Length: 12\r\n\r\nhello world!";
    check_bool("arm: non-chunked never arms", 0, arm_flags(b, (u32)strlen(b)));
}

static void test_arm_chunked_head_without_tail(void) {
    // First read of a chunked stream: header seen, but this read does not carry
    // the terminator, so only "detected" is set and finish is deferred.
    const char *b = "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n"
                    "1a\r\ndata: {\"choices\":[{\"i\":0}]}\r\n";
    u8 f = arm_flags(b, (u32)strlen(b));
    check_bool("arm: chunked head, no tail -> detected", true, (f & k_http_chunked_detected) != 0);
    check_bool(
        "arm: chunked head, no tail -> not last_seen", false, (f & k_http_chunked_last_seen) != 0);
}

int main(void) {
    test_token_case_insensitive();

    test_head_detects_chunked_header();
    test_head_rejects_content_length();
    test_head_too_short();
    test_head_token_at_window_edge();
    test_head_token_beyond_window();

    test_tail_exact_terminator();
    test_tail_full_7byte_terminator();
    test_tail_standalone_terminator();
    test_tail_too_short();
    test_tail_terminator_midbody_not_at_end();
    test_tail_content_ending_crlf_only();
    test_tail_trailered_last_chunk_falls_back();

    test_arm_single_read_complete_stream();
    test_arm_non_chunked_never_arms();
    test_arm_chunked_head_without_tail();

    if (failures > 0) {
        fprintf(stderr, "%d test(s) failed\n", failures);
        return 1;
    }
    printf("all http chunked detection tests passed\n");
    return 0;
}
