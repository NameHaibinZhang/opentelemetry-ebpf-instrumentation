// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package gpuevent

import (
	"testing"
	"time"

	"github.com/cilium/ebpf"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.opentelemetry.io/obi/pkg/appolly/app"
	"go.opentelemetry.io/obi/pkg/appolly/app/request"
	ebpfcommon "go.opentelemetry.io/obi/pkg/ebpf/common"
	"go.opentelemetry.io/obi/pkg/export/imetrics"
	ebpfconvenience "go.opentelemetry.io/obi/pkg/internal/ebpf/convenience"
	"go.opentelemetry.io/obi/pkg/obi"
)

func TestRingbufMetrics(t *testing.T) {
	spec, err := LoadBpf()
	require.NoError(t, err)

	require.Contains(t, spec.Maps, "ringbuf_write_stats_storage")
	assert.Equal(t, ebpf.PerCPUArray, spec.Maps["ringbuf_write_stats_storage"].Type)
	assert.Equal(t, ebpfconvenience.PinInternal, spec.Maps["ringbuf_write_stats_storage"].Pinning)
}

func TestRingbufMetricsConstant(t *testing.T) {
	tracer := New(nil, &obi.Config{}, imetrics.NoopReporter{})
	assert.Equal(t, false, tracer.constants()["ringbuf_metrics_enabled"])

	tracer = New(nil, &obi.Config{}, struct{ imetrics.NoopReporter }{})
	assert.Equal(t, true, tracer.constants()["ringbuf_metrics_enabled"])
}

func TestApplyDeviceIdentityRetainsNameOnlyObservation(t *testing.T) {
	const (
		pid   = app.PID(1234)
		index = uint32(7)
		model = "NVIDIA H20"
	)

	tracer := &Tracer{
		deviceModels: map[app.PID]map[uint32]string{
			pid: {index: model},
		},
	}
	span := request.Span{Pid: request.PidInfo{HostPID: pid}}

	tracer.applyDeviceIdentity(&span, BpfCudaDeviceT{
		Index: index,
		Known: 1,
	})

	assert.True(t, span.CudaDeviceKnown)
	assert.Equal(t, index, span.CudaDeviceIndex)
	assert.Empty(t, span.CudaDeviceUUID)
	assert.Equal(t, model, span.CudaDeviceModel)
}

func TestApplyDeviceIdentityScopesModelToProcess(t *testing.T) {
	tracer := &Tracer{
		deviceModels: map[app.PID]map[uint32]string{
			1234: {7: "NVIDIA H20"},
		},
	}
	span := request.Span{Pid: request.PidInfo{HostPID: 5678}}

	tracer.applyDeviceIdentity(&span, BpfCudaDeviceT{
		Index: 7,
		Known: 1,
	})

	assert.Empty(t, span.CudaDeviceModel)
}

func TestBlockPIDDiscardsDeviceModelsBeforePIDReuse(t *testing.T) {
	const (
		pid   = app.PID(1234)
		index = uint32(7)
		model = "NVIDIA H20"
	)

	tracer := &Tracer{
		pidsFilter: &ebpfcommon.IdentityPidsFilter{},
		deviceModels: map[app.PID]map[uint32]string{
			pid: {index: model},
		},
	}

	tracer.BlockPID(pid, 0)
	tracer.AllowPID(pid, 0, nil)

	span := request.Span{Pid: request.PidInfo{HostPID: pid}}
	tracer.applyDeviceIdentity(&span, BpfCudaDeviceT{
		Index: index,
		Known: 1,
	})

	assert.True(t, span.CudaDeviceKnown)
	assert.Empty(t, span.CudaDeviceModel)
	assert.NotContains(t, tracer.deviceModels, pid)
}

func testInventory(pid app.PID, devices ...gpuDevice) *gpuInventory {
	return &gpuInventory{
		devices:   devices,
		fetchedAt: time.Now(),
		now:       time.Now,
		ttl:       time.Hour,
		visible: map[app.PID]visibleResolution{
			pid: {devices: devices, ok: true},
		},
	}
}

func TestApplyDeviceIdentityResolvesUnobservedDevice(t *testing.T) {
	const pid = app.PID(1234)
	tracer := &Tracer{
		deviceModels: map[app.PID]map[uint32]string{},
		devices:      testInventory(pid, gpuDevice{UUID: testUUID0, Model: "NVIDIA H20"}),
	}
	span := request.Span{Pid: request.PidInfo{HostPID: pid}}

	tracer.applyDeviceIdentity(&span, BpfCudaDeviceT{Index: 3, Known: 0})

	assert.True(t, span.CudaDeviceKnown)
	assert.Equal(t, uint32(0), span.CudaDeviceIndex)
	assert.Equal(t, "GPU-"+testUUID0, span.CudaDeviceUUID)
	assert.Equal(t, "NVIDIA H20", span.CudaDeviceModel)
}

func TestApplyDeviceIdentityResolvesKnownIndex(t *testing.T) {
	const pid = app.PID(1234)
	tracer := &Tracer{
		deviceModels: map[app.PID]map[uint32]string{},
		devices: testInventory(pid,
			gpuDevice{UUID: testUUID0, Model: "NVIDIA H20"},
			gpuDevice{UUID: testUUID1, Model: "NVIDIA H100"},
		),
	}
	span := request.Span{Pid: request.PidInfo{HostPID: pid}}

	tracer.applyDeviceIdentity(&span, BpfCudaDeviceT{Index: 1, Known: 1})

	assert.True(t, span.CudaDeviceKnown)
	assert.Equal(t, uint32(1), span.CudaDeviceIndex)
	assert.Equal(t, "GPU-"+testUUID1, span.CudaDeviceUUID)
	assert.Equal(t, "NVIDIA H100", span.CudaDeviceModel)
}

func TestApplyDeviceIdentityIgnoresAmbiguousUnobservedDevice(t *testing.T) {
	const pid = app.PID(1234)
	tracer := &Tracer{
		deviceModels: map[app.PID]map[uint32]string{},
		devices: testInventory(pid,
			gpuDevice{UUID: testUUID0, Model: "NVIDIA H20"},
			gpuDevice{UUID: testUUID1, Model: "NVIDIA H100"},
		),
	}
	span := request.Span{Pid: request.PidInfo{HostPID: pid}}

	tracer.applyDeviceIdentity(&span, BpfCudaDeviceT{Known: 0})

	assert.False(t, span.CudaDeviceKnown)
	assert.Empty(t, span.CudaDeviceUUID)
	assert.Empty(t, span.CudaDeviceModel)
}

func TestCudaUUIDStringAddsGPUPrefix(t *testing.T) {
	var raw [16]uint8
	for i := range raw {
		raw[i] = uint8(i + 1)
	}

	assert.Equal(t, "GPU-01020304-0506-0708-090a-0b0c0d0e0f10", cudaUUIDString(raw))
	assert.Empty(t, cudaUUIDString([16]uint8{}))
}
