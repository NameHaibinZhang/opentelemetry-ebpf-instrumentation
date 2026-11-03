// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package prom

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.opentelemetry.io/obi/pkg/appolly/app/svc"
	"go.opentelemetry.io/obi/pkg/appolly/services"
	"go.opentelemetry.io/obi/pkg/export"
	"go.opentelemetry.io/obi/pkg/export/attributes"
	attr "go.opentelemetry.io/obi/pkg/export/attributes/names"
	"go.opentelemetry.io/obi/pkg/internal/procmetrics"
)

func TestProcessMetricsSelectionFeaturesAndRemoval(t *testing.T) {
	selector, err := attributes.NewAttrSelector(attributes.GroupPrometheus|attributes.GroupKubernetes,
		&attributes.SelectorConfig{SelectionCfg: attributes.Selection{
			"process.memory.usage": {Include: []string{"process.*"}, Exclude: []string{"process.command"}},
		}})
	require.NoError(t, err)
	r := metricsReporter{processMetrics: newProcessMetricsCollector(selector)}
	registry := prometheus.NewRegistry()
	for _, collector := range r.processMetrics.collectors() {
		require.NoError(t, registry.Register(collector))
	}
	modes := services.NewExportModes()
	modes.AllowMetrics()
	snapshot := procmetrics.Snapshot{
		PID: 123, Command: "worker", ExecutablePath: "/bin/worker", Owner: "root",
		MemoryUsageBytes: 100, CPUTimeUserSeconds: 1,
		Service: svc.Attrs{
			UID: svc.UID{Name: "worker"}, ExportModes: modes,
			Metadata: map[attr.Name]string{attr.K8sPodName: "worker-pod"},
		},
	}
	r.collectProcessMetrics([]procmetrics.Snapshot{snapshot})
	families, err := registry.Gather()
	require.NoError(t, err)
	assert.Empty(t, families)

	snapshot.Service.Features = export.FeatureProcess
	r.collectProcessMetrics([]procmetrics.Snapshot{snapshot})
	r.collectProcessMetrics([]procmetrics.Snapshot{snapshot})
	families, err = registry.Gather()
	require.NoError(t, err)
	require.Len(t, families, 5)
	for _, family := range families {
		if family.GetName() == attributes.ProcessCPUTime.Prom {
			require.Len(t, family.Metric, 2)
			labels := map[string]string{}
			for _, label := range family.Metric[0].Label {
				labels[label.GetName()] = label.GetValue()
			}
			assert.Equal(t, "worker", labels["process_command"])
			assert.Equal(t, "worker-pod", labels["k8s_pod_name"])
		}
		if family.GetName() != attributes.ProcessMemoryUsage.Prom {
			continue
		}
		require.Len(t, family.Metric, 1)
		point := family.Metric[0]
		assert.InDelta(t, 100.0, point.Gauge.GetValue(), 0.0001)
		labels := map[string]string{}
		for _, label := range point.Label {
			labels[label.GetName()] = label.GetValue()
		}
		assert.NotContains(t, labels, "process_command")
		assert.Equal(t, "123", labels["process_pid"])
		assert.Equal(t, "/bin/worker", labels["process_executable_path"])
		assert.Equal(t, "root", labels["process_owner"])
	}

	snapshot.Removed = true
	snapshot.Service.Features = export.FeatureApplicationRED
	r.collectProcessMetrics([]procmetrics.Snapshot{snapshot})
	families, err = registry.Gather()
	require.NoError(t, err)
	assert.Empty(t, families)
}
