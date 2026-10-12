// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package otel

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.opentelemetry.io/obi/internal/test/collector"
	"go.opentelemetry.io/obi/pkg/export/attributes"
	attr "go.opentelemetry.io/obi/pkg/export/attributes/names"
	"go.opentelemetry.io/obi/pkg/export/otel/otelcfg"
	"go.opentelemetry.io/obi/pkg/gpumon"
	"go.opentelemetry.io/obi/pkg/pipe/global"
)

func newTestGPUMetricsReporter(t *testing.T, metricRecords chan collector.MetricRecord) *GPUMetricsReporter {
	t.Helper()

	mcfg := &otelcfg.MetricsConfig{
		Interval:        50 * time.Millisecond,
		TTL:             30 * time.Minute,
		MetricsConsumer: testMetricsConsumer(metricRecords),
	}
	reporter, err := NewGPUMetricsReporter(
		t.Context(),
		&global.ContextInfo{OTELMetricsExporter: &otelcfg.MetricsExporterInstancer{Cfg: mcfg}},
		mcfg,
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, reporter.Close(context.Background()))
	})
	return reporter
}

// awaitGPURecords reads records from inCh until satisfy returns true for the
// accumulated records, and returns them. It fails the test on timeout.
func awaitGPURecords(
	t *testing.T,
	inCh <-chan collector.MetricRecord,
	satisfy func(seen []collector.MetricRecord) bool,
) []collector.MetricRecord {
	t.Helper()

	records := []collector.MetricRecord{}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for !satisfy(records) {
		select {
		case item := <-inCh:
			records = append(records, item)
		case <-deadline.C:
			t.Fatalf("timeout waiting for GPU metric records, got %v", records)
			return records
		}
	}
	return records
}

// drainGPURecords collects the records that arrive within the given duration.
func drainGPURecords(inCh <-chan collector.MetricRecord, d time.Duration) []collector.MetricRecord {
	records := []collector.MetricRecord{}
	deadline := time.NewTimer(d)
	defer deadline.Stop()
	for {
		select {
		case item := <-inCh:
			records = append(records, item)
		case <-deadline.C:
			return records
		}
	}
}

// gpuRecordsWhere returns the records of the given metric whose gpu.uuid label
// matches uuid.
func gpuRecordsWhere(records []collector.MetricRecord, name, uuid string) []collector.MetricRecord {
	return slices.DeleteFunc(slices.Clone(records), func(r collector.MetricRecord) bool {
		return r.Name != name || r.Attributes[string(attr.GPUUUID)] != uuid
	})
}

func gpuTestDevice(uuid string) gpumon.Device {
	return gpumon.Device{
		UUID:           uuid,
		BusID:          "0000:0" + uuid + ":00.0",
		Model:          "NVIDIA A100-SXM4-40GB",
		Up:             true,
		MigCapable:     true,
		NumaNode:       0,
		PCIeGeneration: 4,
		PCIeWidth:      16,
	}
}

func TestGPUMetricsReporter_MetricNamesAndLabels(t *testing.T) {
	metricRecords := make(chan collector.MetricRecord, 100)
	reporter := newTestGPUMetricsReporter(t, metricRecords)

	reporter.Report(&gpumon.Snapshot{
		Devices: []gpumon.Device{gpuTestDevice("aaa")},
		NVLinks: []gpumon.NVLink{{
			GPUUUID:  "aaa",
			LinkID:   1,
			Active:   true,
			PeerUUID: "bbb",
			SwitchID: "sw1",
			Rate:     4,
			Width:    4,
		}},
		Processes: []gpumon.GPUProcess{{
			GPUUUID: "aaa",
			PID:     1234,
			Identity: gpumon.ProcessIdentity{
				Executable: "python",
				PodName:    "pod-a",
				JobName:    "job-a",
				Rank:       "0",
			},
		}},
		Conflicts: []gpumon.GPUProcess{{
			GPUUUID: "aaa",
			PID:     1234,
			Identity: gpumon.ProcessIdentity{
				Executable: "python",
				PodName:    "pod-a",
				JobName:    "job-a",
				Rank:       "0",
			},
		}},
		AER: []gpumon.AERDelta{{
			GPUUUID:  "aaa",
			BusID:    "0000:0b:00.0",
			Severity: "correctable",
			Count:    2,
		}},
	})

	names := []string{
		attributes.GPUDeviceInfo.OTEL,
		attributes.GPUUp.OTEL,
		attributes.GPUNumaNode.OTEL,
		attributes.GPUPcieGeneration.OTEL,
		attributes.GPUPcieWidth.OTEL,
		attributes.GPUNvlinkUp.OTEL,
		attributes.GPUNvlinkRate.OTEL,
		attributes.GPUNvlinkWidth.OTEL,
		attributes.GPUProcessInfo.OTEL,
		attributes.GPUOccupancyConflict.OTEL,
		attributes.GPUPcieAerErrors.OTEL,
	}
	records := readMetricsByName(t, metricRecords, timeout, names...)
	byName := map[string]collector.MetricRecord{}
	for _, r := range records {
		byName[r.Name] = r
	}

	up := byName[attributes.GPUUp.OTEL]
	assert.Equal(t, int64(1), up.IntVal)
	assert.Equal(t, "aaa", up.Attributes[string(attr.GPUUUID)])

	info := byName[attributes.GPUDeviceInfo.OTEL]
	assert.Equal(t, int64(1), info.IntVal)
	assert.Equal(t, "aaa", info.Attributes[string(attr.GPUUUID)])
	assert.Equal(t, "0000:0aaa:00.0", info.Attributes[string(attr.GPUPciBusID)])
	assert.Equal(t, "NVIDIA A100-SXM4-40GB", info.Attributes[string(attr.HWModel)])
	assert.Equal(t, "true", info.Attributes[string(attr.GPUMigCapable)])

	numa := byName[attributes.GPUNumaNode.OTEL]
	assert.Equal(t, int64(0), numa.IntVal)
	assert.Equal(t, "aaa", numa.Attributes[string(attr.GPUUUID)])

	gen := byName[attributes.GPUPcieGeneration.OTEL]
	assert.Equal(t, int64(4), gen.IntVal)
	width := byName[attributes.GPUPcieWidth.OTEL]
	assert.Equal(t, int64(16), width.IntVal)

	nvUp := byName[attributes.GPUNvlinkUp.OTEL]
	assert.Equal(t, int64(1), nvUp.IntVal)
	assert.Equal(t, "aaa", nvUp.Attributes[string(attr.GPUUUID)])
	assert.Equal(t, "1", nvUp.Attributes[string(attr.GPUNvlinkLinkID)])
	assert.Equal(t, "bbb", nvUp.Attributes[string(attr.GPUPeerUUID)])
	assert.Equal(t, "sw1", nvUp.Attributes[string(attr.GPUNvlinkSwitchID)])
	assert.Equal(t, int64(4), byName[attributes.GPUNvlinkRate.OTEL].IntVal)
	assert.Equal(t, int64(4), byName[attributes.GPUNvlinkWidth.OTEL].IntVal)

	proc := byName[attributes.GPUProcessInfo.OTEL]
	assert.Equal(t, int64(1), proc.IntVal)
	assert.Equal(t, "aaa", proc.Attributes[string(attr.GPUUUID)])
	assert.Equal(t, "1234", proc.Attributes[string(attr.ProcessPID)])
	assert.Equal(t, "python", proc.Attributes[string(attr.ProcessExecutableName)])
	assert.Equal(t, "pod-a", proc.Attributes[string(attr.K8sPodName)])
	assert.Equal(t, "job-a", proc.Attributes[string(attr.K8sJobName)])
	assert.Equal(t, "0", proc.Attributes[string(attr.MLRank)])

	conflict := byName[attributes.GPUOccupancyConflict.OTEL]
	assert.Equal(t, int64(1), conflict.IntVal)
	assert.Equal(t, "aaa", conflict.Attributes[string(attr.GPUUUID)])
	assert.Equal(t, "pod-a", conflict.Attributes[string(attr.K8sPodName)])
	assert.Equal(t, "job-a", conflict.Attributes[string(attr.K8sJobName)])
	assert.Equal(t, "0", conflict.Attributes[string(attr.MLRank)])
	assert.NotContains(t, conflict.Attributes, string(attr.ProcessPID))

	aer := byName[attributes.GPUPcieAerErrors.OTEL]
	assert.Equal(t, int64(2), aer.IntVal)
	assert.Equal(t, "aaa", aer.Attributes[string(attr.GPUUUID)])
	assert.Equal(t, "correctable", aer.Attributes[string(attr.GPUPcieAerSeverity)])
	assert.Equal(t, "0000:0b:00.0", aer.Attributes[string(attr.GPUPciBusID)])
}

func TestGPUMetricsReporter_GhostDeviceAndEviction(t *testing.T) {
	metricRecords := make(chan collector.MetricRecord, 100)
	reporter := newTestGPUMetricsReporter(t, metricRecords)

	reporter.Report(&gpumon.Snapshot{
		Devices: []gpumon.Device{gpuTestDevice("aaa"), gpuTestDevice("bbb")},
	})
	awaitGPURecords(t, metricRecords, func(seen []collector.MetricRecord) bool {
		return len(gpuRecordsWhere(seen, attributes.GPUUp.OTEL, "aaa")) > 0 &&
			len(gpuRecordsWhere(seen, attributes.GPUUp.OTEL, "bbb")) > 0
	})

	// bbb goes ghost: only its UUID is known. It reports up=0 for one cycle,
	// while its info, NUMA and PCIe series are evicted right away.
	reporter.Report(&gpumon.Snapshot{
		Devices: []gpumon.Device{gpuTestDevice("aaa"), {UUID: "bbb", Up: false}},
	})
	awaitGPURecords(t, metricRecords, func(seen []collector.MetricRecord) bool {
		for _, r := range gpuRecordsWhere(seen, attributes.GPUUp.OTEL, "bbb") {
			if r.IntVal == 0 {
				return true
			}
		}
		return false
	})

	// bbb disappears for good, and ccc joins as a marker whose series keep
	// being re-exported on every collection cycle.
	reporter.Report(&gpumon.Snapshot{
		Devices: []gpumon.Device{gpuTestDevice("aaa"), gpuTestDevice("ccc")},
	})
	awaitGPURecords(t, metricRecords, func(seen []collector.MetricRecord) bool {
		return len(gpuRecordsWhere(seen, attributes.GPUUp.OTEL, "ccc")) >= 3
	})
	after := drainGPURecords(metricRecords, 250*time.Millisecond)

	// The series of the gone device are no longer exported.
	assert.Empty(t, gpuRecordsWhere(after, attributes.GPUUp.OTEL, "bbb"))
	assert.Empty(t, gpuRecordsWhere(after, attributes.GPUDeviceInfo.OTEL, "bbb"))
	assert.Empty(t, gpuRecordsWhere(after, attributes.GPUNumaNode.OTEL, "bbb"))
	assert.Empty(t, gpuRecordsWhere(after, attributes.GPUPcieGeneration.OTEL, "bbb"))
	assert.Empty(t, gpuRecordsWhere(after, attributes.GPUPcieWidth.OTEL, "bbb"))
	// The surviving device is still exported.
	assert.NotEmpty(t, gpuRecordsWhere(after, attributes.GPUUp.OTEL, "aaa"))
	assert.NotEmpty(t, gpuRecordsWhere(after, attributes.GPUDeviceInfo.OTEL, "aaa"))
}

func TestGPUMetricsReporter_ProcessInfoDiff(t *testing.T) {
	metricRecords := make(chan collector.MetricRecord, 100)
	reporter := newTestGPUMetricsReporter(t, metricRecords)

	reporter.Report(&gpumon.Snapshot{
		Devices: []gpumon.Device{gpuTestDevice("aaa")},
		Processes: []gpumon.GPUProcess{{
			GPUUUID:   "aaa",
			PID:       1234,
			Identity:  gpumon.ProcessIdentity{Executable: "python"},
		}},
	})
	awaitGPURecords(t, metricRecords, func(seen []collector.MetricRecord) bool {
		for _, r := range seen {
			if r.Name == attributes.GPUProcessInfo.OTEL &&
				r.Attributes[string(attr.GPUUUID)] == "aaa" &&
				r.Attributes[string(attr.ProcessPID)] == "1234" {
				return r.IntVal == 1
			}
		}
		return false
	})

	// The process goes away. The marker device keeps the reporter exporting,
	// so a series that failed to be evicted would keep being re-exported.
	reporter.Report(&gpumon.Snapshot{
		Devices: []gpumon.Device{gpuTestDevice("aaa"), gpuTestDevice("ccc")},
	})
	awaitGPURecords(t, metricRecords, func(seen []collector.MetricRecord) bool {
		return len(gpuRecordsWhere(seen, attributes.GPUUp.OTEL, "ccc")) >= 3
	})
	after := drainGPURecords(metricRecords, 250*time.Millisecond)

	assert.Empty(t, gpuRecordsWhere(after, attributes.GPUProcessInfo.OTEL, "aaa"))
}
