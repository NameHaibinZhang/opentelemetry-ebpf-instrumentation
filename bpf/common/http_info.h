// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include <bpfcore/vmlinux.h>

#include <common/connection_info.h>
#include <common/event_source.h>
#include <common/tp_info.h>

#define FULL_BUF_SIZE 256

// Here we keep the information that is sent on the ring buffer
typedef struct http_info {
    u8 flags; // Must be first, we use it to tell what kind of packet we have on the ring buffer
    u8 type;
    u8 ssl;
    u8 delayed;
    connection_info_t conn_info;
    u64 start_monotime_ns;
    u64 end_monotime_ns;
    u64 req_monotime_ns;
    u64 extra_id;
    tp_info_t tp;
    pid_info pid;
    u32 len;
    u32 resp_len;
    u32 task_tid;
    u32 lb_req_bytes;
    u32 lb_res_bytes;
    u16 status;
    unsigned char buf[FULL_BUF_SIZE];
    u8 has_large_buffers;
    u8 direction;
    u8 submitted;
    enum event_source_type event_source;
    // chunked: in-band finish flags for chunked (e.g. SSE streaming) responses.
    // bit0 = Transfer-Encoding: chunked detected on the response head.
    // bit1 = the chunked last-chunk terminator was observed at a read tail.
    // Reuses one of the former padding bytes, so the struct size is unchanged.
    u8 chunked;
    u8 _pad[1];
} http_info_t;
