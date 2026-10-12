// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package gpumon

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.opentelemetry.io/obi/pkg/export/attributes"
	attr "go.opentelemetry.io/obi/pkg/export/attributes/names"
)

func fullSnapshot() *Snapshot {
	return &Snapshot{
		Devices: []Device{{
			UUID:           "aaa",
			BusID:          "0000:0b:00.0",
			Model:          "NVIDIA A100",
			Up:             true,
			MigCapable:     true,
			NumaNode:       0,
			PCIeGeneration: 4,
			PCIeWidth:      16,
		}},
		NVLinks: []NVLink{{
			GPUUUID:  "aaa",
			LinkID:   1,
			Active:   true,
			PeerUUID: "bbb",
			Rate:     4,
			Width:    nvlinkWidthLanes,
		}},
		Processes: []GPUProcess{{
			GPUUUID:  "aaa",
			PID:      1234,
			Identity: ProcessIdentity{Executable: "python", PodName: "pod-a", JobName: "job-a", Rank: "0"},
		}},
		Conflicts: []GPUProcess{{
			GPUUUID:  "aaa",
			PID:      1234,
			Identity: ProcessIdentity{PodName: "pod-a", Rank: "0"},
		}},
		AER: []AERDelta{{
			GPUUUID:  "aaa",
			BusID:    "0000:0b:00.0",
			Severity: "correctable",
			Count:    2,
		}},
	}
}

func newPromTestSetup(t *testing.T) (*PromReporter, *prometheus.Registry) {
	t.Helper()
	reporter := NewPromReporter()
	registry := prometheus.NewRegistry()
	registry.MustRegister(reporter)
	return reporter, registry
}

func gatherFamilies(t *testing.T, registry *prometheus.Registry) map[string]*dto.MetricFamily {
	t.Helper()
	families, err := registry.Gather()
	require.NoError(t, err)

	byName := map[string]*dto.MetricFamily{}
	for _, f := range families {
		byName[f.GetName()] = f
	}
	return byName
}

func labelsOf(metric *dto.Metric) map[string]string {
	labels := map[string]string{}
	for _, l := range metric.Label {
		labels[l.GetName()] = l.GetValue()
	}
	return labels
}

func labelValue(metric *dto.Metric, name string) string {
	for _, l := range metric.Label {
		if l.GetName() == name {
			return l.GetValue()
		}
	}
	return ""
}

func familyNames(families map[string]*dto.MetricFamily) []string {
	names := make([]string, 0, len(families))
	for name := range families {
		names = append(names, name)
	}
	return names
}

func TestPromReporterFamilyNamesAndLabels(t *testing.T) {
	reporter, registry := newPromTestSetup(t)
	reporter.Report(fullSnapshot())

	families := gatherFamilies(t, registry)
	assert.ElementsMatch(t, []string{
		attributes.GPUDeviceInfo.Prom,
		attributes.GPUUp.Prom,
		attributes.GPUNumaNode.Prom,
		attributes.GPUPcieGeneration.Prom,
		attributes.GPUPcieWidth.Prom,
		attributes.GPUNvlinkUp.Prom,
		attributes.GPUNvlinkRate.Prom,
		attributes.GPUNvlinkWidth.Prom,
		attributes.GPUProcessInfo.Prom,
		attributes.GPUOccupancyConflict.Prom,
		attributes.GPUPcieAerErrors.Prom,
	}, familyNames(families))

	info := families[attributes.GPUDeviceInfo.Prom]
	require.Len(t, info.Metric, 1)
	assert.Equal(t, map[string]string{
		attr.GPUUUID.Prom():       "aaa",
		attr.GPUPciBusID.Prom():   "0000:0b:00.0",
		attr.HWModel.Prom():       "NVIDIA A100",
		attr.GPUMigCapable.Prom(): "true",
	}, labelsOf(info.Metric[0]))

	nvlinkUp := families[attributes.GPUNvlinkUp.Prom]
	require.Len(t, nvlinkUp.Metric, 1)
	assert.Equal(t, map[string]string{
		attr.GPUUUID.Prom():           "aaa",
		attr.GPUNvlinkLinkID.Prom():   "1",
		attr.GPUPeerUUID.Prom():       "bbb",
		attr.GPUNvlinkSwitchID.Prom(): "",
	}, labelsOf(nvlinkUp.Metric[0]))

	processInfo := families[attributes.GPUProcessInfo.Prom]
	require.Len(t, processInfo.Metric, 1)
	assert.Equal(t, map[string]string{
		attr.GPUUUID.Prom():               "aaa",
		attr.ProcessPID.Prom():            "1234",
		attr.ProcessExecutableName.Prom(): "python",
		attr.K8sPodName.Prom():            "pod-a",
		attr.K8sJobName.Prom():            "job-a",
		attr.MLRank.Prom():                "0",
	}, labelsOf(processInfo.Metric[0]))

	aer := families[attributes.GPUPcieAerErrors.Prom]
	require.Len(t, aer.Metric, 1)
	assert.Equal(t, map[string]string{
		attr.GPUUUID.Prom():            "aaa",
		attr.GPUPcieAerSeverity.Prom(): "correctable",
		attr.GPUPciBusID.Prom():        "0000:0b:00.0",
	}, labelsOf(aer.Metric[0]))
}

func TestPromReporterValues(t *testing.T) {
	reporter, _ := newPromTestSetup(t)
	reporter.Report(fullSnapshot())

	assert.InDelta(t, 1.0, testutil.ToFloat64(reporter.up.WithLabelValues("aaa")), 1e-9)
	assert.InDelta(t, 1.0, testutil.ToFloat64(reporter.info.WithLabelValues("aaa", "0000:0b:00.0", "NVIDIA A100", "true")), 1e-9)
	assert.InDelta(t, 0.0, testutil.ToFloat64(reporter.numa.WithLabelValues("aaa")), 1e-9)
	assert.InDelta(t, 4.0, testutil.ToFloat64(reporter.pcieGeneration.WithLabelValues("aaa")), 1e-9)
	assert.InDelta(t, 16.0, testutil.ToFloat64(reporter.pcieWidth.WithLabelValues("aaa")), 1e-9)
	assert.InDelta(t, 1.0, testutil.ToFloat64(reporter.nvlinkUp.WithLabelValues("aaa", "1", "bbb", "")), 1e-9)
	assert.InDelta(t, 4.0, testutil.ToFloat64(reporter.nvlinkRate.WithLabelValues("aaa", "1", "bbb", "")), 1e-9)
	assert.InDelta(t, float64(nvlinkWidthLanes), testutil.ToFloat64(reporter.nvlinkWidth.WithLabelValues("aaa", "1", "bbb", "")), 1e-9)
	assert.InDelta(t, 1.0, testutil.ToFloat64(reporter.processInfo.WithLabelValues("aaa", "1234", "python", "pod-a", "job-a", "0")), 1e-9)
	assert.InDelta(t, 1.0, testutil.ToFloat64(reporter.conflicts.WithLabelValues("aaa", "pod-a", "", "0")), 1e-9)
	assert.InDelta(t, 2.0, testutil.ToFloat64(reporter.aer.WithLabelValues("aaa", "correctable", "0000:0b:00.0")), 1e-9)
}

func TestPromReporterGhostDeviceOnlyReportsUp(t *testing.T) {
	reporter, registry := newPromTestSetup(t)
	reporter.Report(&Snapshot{Devices: []Device{{UUID: "ghost", Up: false}}})

	families := gatherFamilies(t, registry)
	assert.ElementsMatch(t, []string{attributes.GPUUp.Prom}, familyNames(families))
	assert.InDelta(t, 0.0, testutil.ToFloat64(reporter.up.WithLabelValues("ghost")), 1e-9)
}

func TestPromReporterWipesGaugeSeriesBetweenCycles(t *testing.T) {
	reporter, registry := newPromTestSetup(t)

	two := fullSnapshot()
	two.Devices = append(two.Devices, Device{
		UUID: "bbb", BusID: "0000:1b:00.0", Model: "NVIDIA H100", Up: true, NumaNode: 1,
	})
	reporter.Report(two)
	assert.InDelta(t, 1.0, testutil.ToFloat64(reporter.info.WithLabelValues("bbb", "0000:1b:00.0", "NVIDIA H100", "false")), 1e-9)

	reporter.Report(fullSnapshot())

	families := gatherFamilies(t, registry)
	info := families[attributes.GPUDeviceInfo.Prom]
	require.Len(t, info.Metric, 1, "the gone device series must be wiped")
	assert.Equal(t, "aaa", labelValue(info.Metric[0], attr.GPUUUID.Prom()))
}

func TestPromReporterCountersAccumulateAcrossCycles(t *testing.T) {
	reporter, _ := newPromTestSetup(t)

	reporter.Report(fullSnapshot())
	reporter.Report(fullSnapshot())

	assert.InDelta(t, 2.0, testutil.ToFloat64(reporter.conflicts.WithLabelValues("aaa", "pod-a", "", "0")), 1e-9)
	assert.InDelta(t, 4.0, testutil.ToFloat64(reporter.aer.WithLabelValues("aaa", "correctable", "0000:0b:00.0")), 1e-9)
}
