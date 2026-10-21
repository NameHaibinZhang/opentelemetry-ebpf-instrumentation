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
)

func fullSnapshot() *Snapshot {
	return &Snapshot{
		Devices: []Device{{
			UUID:           "GPU-aaa",
			BusID:          "0000:0b:00.0",
			Model:          "NVIDIA A100",
			Up:             true,
			MigCapable:     true,
			NumaNode:       0,
			PCIeGeneration: 4,
			PCIeWidth:      16,
		}},
		NVLinks: []NVLink{{
			GPUUUID:  "GPU-aaa",
			LinkID:   1,
			Active:   true,
			PeerUUID: "GPU-bbb",
			Rate:     4,
			Width:    nvlinkWidthLanes,
		}},
		Processes: []GPUProcess{{
			GPUUUID:  "GPU-aaa",
			PID:      1234,
			Identity: ProcessIdentity{Executable: "python", PodName: "pod-a", JobName: "job-a", Rank: "0"},
		}},
		Conflicts: []GPUProcess{{
			GPUUUID:  "GPU-aaa",
			PID:      1234,
			Identity: ProcessIdentity{PodName: "pod-a", Rank: "0"},
		}},
		AER: []AERDelta{{
			GPUUUID:  "GPU-aaa",
			BusID:    "0000:0b:00.0",
			Severity: "correctable",
			Count:    2,
		}},
	}
}

func newPromTestSetup(t *testing.T) (*PromReporter, *prometheus.Registry) {
	t.Helper()
	reporter := NewPromReporter(nil)
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
		metricGPUInfo,
		metricGPUUp,
		metricGPUNumaNode,
		metricGPUPcieGeneration,
		metricGPUPcieWidth,
		metricGPUNvlinkUp,
		metricGPUNvlinkRate,
		metricGPUNvlinkWidth,
		metricGPUProcessInfo,
		metricGPUOccupancyConflict,
		metricGPUPcieAerErrors,
	}, familyNames(families))

	info := families[metricGPUInfo]
	require.Len(t, info.Metric, 1)
	assert.Equal(t, map[string]string{
		labelGPUUUID:       "GPU-aaa",
		labelGPUPciBusID:   "0000:0b:00.0",
		labelHWModel:       "NVIDIA A100",
		labelGPUMigCapable: "true",
	}, labelsOf(info.Metric[0]))

	nvlinkUp := families[metricGPUNvlinkUp]
	require.Len(t, nvlinkUp.Metric, 1)
	assert.Equal(t, map[string]string{
		labelGPUUUID:           "GPU-aaa",
		labelGPUNvlinkLinkID:   "1",
		labelGPUPeerUUID:       "GPU-bbb",
		labelGPUNvlinkSwitchID: "",
	}, labelsOf(nvlinkUp.Metric[0]))

	processInfo := families[metricGPUProcessInfo]
	require.Len(t, processInfo.Metric, 1)
	assert.Equal(t, map[string]string{
		labelGPUUUID:           "GPU-aaa",
		labelProcessPID:        "1234",
		labelProcessExecutable: "python",
		labelK8sPodName:        "pod-a",
		labelK8sJobName:        "job-a",
		labelMLRank:            "0",
	}, labelsOf(processInfo.Metric[0]))

	aer := families[metricGPUPcieAerErrors]
	require.Len(t, aer.Metric, 1)
	assert.Equal(t, map[string]string{
		labelGPUUUID:            "GPU-aaa",
		labelGPUPcieAerSeverity: "correctable",
		labelGPUPciBusID:        "0000:0b:00.0",
	}, labelsOf(aer.Metric[0]))
}

func TestPromReporterValues(t *testing.T) {
	reporter, _ := newPromTestSetup(t)
	reporter.Report(fullSnapshot())

	assert.InDelta(t, 1.0, testutil.ToFloat64(reporter.up.WithLabelValues("GPU-aaa")), 1e-9)
	assert.InDelta(t, 1.0, testutil.ToFloat64(reporter.info.WithLabelValues("GPU-aaa", "0000:0b:00.0", "NVIDIA A100", "true")), 1e-9)
	assert.InDelta(t, 0.0, testutil.ToFloat64(reporter.numa.WithLabelValues("GPU-aaa")), 1e-9)
	assert.InDelta(t, 4.0, testutil.ToFloat64(reporter.pcieGeneration.WithLabelValues("GPU-aaa")), 1e-9)
	assert.InDelta(t, 16.0, testutil.ToFloat64(reporter.pcieWidth.WithLabelValues("GPU-aaa")), 1e-9)
	assert.InDelta(t, 1.0, testutil.ToFloat64(reporter.nvlinkUp.WithLabelValues("GPU-aaa", "1", "GPU-bbb", "")), 1e-9)
	assert.InDelta(t, 4.0, testutil.ToFloat64(reporter.nvlinkRate.WithLabelValues("GPU-aaa", "1", "GPU-bbb", "")), 1e-9)
	assert.InDelta(t, float64(nvlinkWidthLanes), testutil.ToFloat64(reporter.nvlinkWidth.WithLabelValues("GPU-aaa", "1", "GPU-bbb", "")), 1e-9)
	assert.InDelta(t, 1.0, testutil.ToFloat64(reporter.processInfo.WithLabelValues("GPU-aaa", "1234", "python", "pod-a", "job-a", "0")), 1e-9)
	assert.InDelta(t, 1.0, testutil.ToFloat64(reporter.conflicts.WithLabelValues("GPU-aaa", "pod-a", "", "0")), 1e-9)
	assert.InDelta(t, 2.0, testutil.ToFloat64(reporter.aer.WithLabelValues("GPU-aaa", "correctable", "0000:0b:00.0")), 1e-9)
}

func TestPromReporterGhostDeviceOnlyReportsUp(t *testing.T) {
	reporter, registry := newPromTestSetup(t)
	reporter.Report(&Snapshot{Devices: []Device{{UUID: "ghost", Up: false}}})

	families := gatherFamilies(t, registry)
	assert.ElementsMatch(t, []string{metricGPUUp}, familyNames(families))
	assert.InDelta(t, 0.0, testutil.ToFloat64(reporter.up.WithLabelValues("ghost")), 1e-9)
}

func TestPromReporterWipesGaugeSeriesBetweenCycles(t *testing.T) {
	reporter, registry := newPromTestSetup(t)

	two := fullSnapshot()
	two.Devices = append(two.Devices, Device{
		UUID: "GPU-bbb", BusID: "0000:1b:00.0", Model: "NVIDIA H100", Up: true, NumaNode: 1,
	})
	reporter.Report(two)
	assert.InDelta(t, 1.0, testutil.ToFloat64(reporter.info.WithLabelValues("GPU-bbb", "0000:1b:00.0", "NVIDIA H100", "false")), 1e-9)

	reporter.Report(fullSnapshot())

	families := gatherFamilies(t, registry)
	info := families[metricGPUInfo]
	require.Len(t, info.Metric, 1, "the gone device series must be wiped")
	assert.Equal(t, "GPU-aaa", labelValue(info.Metric[0], labelGPUUUID))
}

func TestPromReporterCountersAccumulateAcrossCycles(t *testing.T) {
	reporter, _ := newPromTestSetup(t)

	reporter.Report(fullSnapshot())
	reporter.Report(fullSnapshot())

	assert.InDelta(t, 2.0, testutil.ToFloat64(reporter.conflicts.WithLabelValues("GPU-aaa", "pod-a", "", "0")), 1e-9)
	assert.InDelta(t, 4.0, testutil.ToFloat64(reporter.aer.WithLabelValues("GPU-aaa", "correctable", "0000:0b:00.0")), 1e-9)
}

func TestPromReporterExtraLabels(t *testing.T) {
	reporter, registry := newPromTestSetupWithExtra(t, ParseExtraLabels("deployment.environment=prod,team=pay"))
	reporter.Report(fullSnapshot())

	families := gatherFamilies(t, registry)
	info := families[metricGPUInfo]
	require.Len(t, info.Metric, 1)
	assert.Equal(t, map[string]string{
		labelGPUUUID:             "GPU-aaa",
		labelGPUPciBusID:         "0000:0b:00.0",
		labelHWModel:             "NVIDIA A100",
		labelGPUMigCapable:       "true",
		"deployment_environment": "prod",
		"team":                   "pay",
	}, labelsOf(info.Metric[0]))

	up := families[metricGPUUp]
	require.Len(t, up.Metric, 1)
	assert.Equal(t, "prod", labelValue(up.Metric[0], "deployment_environment"))
	assert.Equal(t, "pay", labelValue(up.Metric[0], "team"))

	// The vec wiring must match the reported values, or Collect would panic.
	assert.InDelta(t, 1.0, testutil.ToFloat64(reporter.up.WithLabelValues("GPU-aaa", "prod", "pay")), 1e-9)
	assert.InDelta(t, 2.0, testutil.ToFloat64(reporter.aer.WithLabelValues("GPU-aaa", "correctable", "0000:0b:00.0", "prod", "pay")), 1e-9)
}

func TestPromReporterCounterExtraLabelsAccumulate(t *testing.T) {
	reporter, _ := newPromTestSetupWithExtra(t, ParseExtraLabels("team=pay"))
	reporter.Report(fullSnapshot())
	reporter.Report(fullSnapshot())
	assert.InDelta(t, 4.0, testutil.ToFloat64(reporter.aer.WithLabelValues("GPU-aaa", "correctable", "0000:0b:00.0", "pay")), 1e-9)
}

func newPromTestSetupWithExtra(t *testing.T, extra ExtraLabels) (*PromReporter, *prometheus.Registry) {
	t.Helper()
	reporter := NewPromReporter(extra)
	registry := prometheus.NewRegistry()
	registry.MustRegister(reporter)
	return reporter, registry
}
