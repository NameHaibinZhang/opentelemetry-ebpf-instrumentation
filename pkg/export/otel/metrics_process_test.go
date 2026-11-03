// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package otel

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"go.opentelemetry.io/obi/pkg/appolly/app"
	"go.opentelemetry.io/obi/pkg/appolly/app/svc"
	"go.opentelemetry.io/obi/pkg/appolly/services"
	"go.opentelemetry.io/obi/pkg/export"
	"go.opentelemetry.io/obi/pkg/export/attributes"
	attr "go.opentelemetry.io/obi/pkg/export/attributes/names"
	"go.opentelemetry.io/obi/pkg/export/otel/metric"
	"go.opentelemetry.io/obi/pkg/export/otel/otelcfg"
	"go.opentelemetry.io/obi/pkg/internal/procmetrics"
)

func TestProcessMetricsMemoryAndRemoval(t *testing.T) {
	metrics, reader := newTestProcessMetrics(t, attributes.Selection{})
	first := procmetrics.Snapshot{
		PID: 123, Command: "worker", MemoryUsageBytes: 100, MemoryVirtualBytes: 200,
		CPUTimeUserSeconds: 1, CPUUtilizationUser: 0.5, DiskReadBytes: 10,
	}
	for _, usage := range []int64{100, 100, 60} {
		first.MemoryUsageBytes = usage
		first.MemoryVirtualBytes = usage * 2
		recordProcessMetrics(t.Context(), metrics, &first)
	}
	data := collectProcessMetrics(t, reader)
	assert.Equal(t, int64(60), data[attributes.ProcessMemoryUsage.OTEL].(metricdata.Sum[int64]).DataPoints[0].Value)
	assert.Equal(t, int64(120), data[attributes.ProcessMemoryVirtual.OTEL].(metricdata.Sum[int64]).DataPoints[0].Value)
	assert.Len(t, data, 5)

	second := first
	second.PID = 456
	recordProcessMetrics(t.Context(), metrics, &second)
	first.Removed = true
	recordProcessMetrics(t.Context(), metrics, &first)
	data = collectProcessMetrics(t, reader)
	require.Len(t, data, 5)
	for name, aggregation := range data {
		var sets []attribute.Set
		switch points := aggregation.(type) {
		case metricdata.Sum[int64]:
			for _, point := range points.DataPoints {
				sets = append(sets, point.Attributes)
			}
		case metricdata.Sum[float64]:
			for _, point := range points.DataPoints {
				sets = append(sets, point.Attributes)
			}
		case metricdata.Gauge[float64]:
			for _, point := range points.DataPoints {
				sets = append(sets, point.Attributes)
				require.True(t, point.Attributes.HasValue(attribute.Key(attr.CPUMode)))
			}
		}
		for _, set := range sets {
			pid, ok := set.Value(attribute.Key(attr.ProcessPID))
			require.True(t, ok, name)
			assert.Equal(t, int64(456), pid.AsInt64(), name)
		}
	}
	second.Removed = true
	recordProcessMetrics(t.Context(), metrics, &second)
	assert.Empty(t, collectProcessMetrics(t, reader))
	assert.Empty(t, metrics.values)
}

func TestProcessMetricsAttributeSelection(t *testing.T) {
	metrics, reader := newTestProcessMetrics(t, attributes.Selection{
		"process.memory.usage": {Include: []string{"process.*"}, Exclude: []string{"process.command"}},
	})
	snapshot := procmetrics.Snapshot{PID: 123, Command: "worker", ExecutablePath: "/bin/worker", Owner: "root", ParentPID: 1}
	recordProcessMetrics(t.Context(), metrics, &snapshot)
	data := collectProcessMetrics(t, reader)
	point := data[attributes.ProcessMemoryUsage.OTEL].(metricdata.Sum[int64]).DataPoints[0]
	assert.False(t, point.Attributes.HasValue(attribute.Key(attr.ProcessCommand)))
	for _, name := range []attr.Name{attr.ProcessPID, attr.ProcessExecutablePath, attr.ProcessOwner, attr.ProcessParentPID} {
		assert.True(t, point.Attributes.HasValue(attribute.Key(name)), name)
	}
	cpu := data[attributes.ProcessCPUTime.OTEL].(metricdata.Sum[float64]).DataPoints[0]
	command, ok := cpu.Attributes.Value(attribute.Key(attr.ProcessCommand))
	require.True(t, ok)
	assert.Equal(t, "worker", command.AsString())
}

func TestProcessMetricsReporterRespectsServiceFeatures(t *testing.T) {
	constructed := 0
	pool, err := otelcfg.NewReporterPool[*svc.Attrs, *ProcessMetrics](10, time.Minute, time.Now,
		func(svc.UID, *ProcessMetrics) {},
		func(*svc.Attrs) (*ProcessMetrics, error) {
			constructed++
			metrics, _ := newTestProcessMetrics(t, attributes.Selection{})
			return metrics, nil
		})
	require.NoError(t, err)
	r := ProcessMetricsReporter{ctx: t.Context(), reporters: pool, log: pmlog()}
	modes := services.NewExportModes()
	modes.AllowMetrics()
	snapshot := procmetrics.Snapshot{PID: 123, Service: svc.Attrs{UID: svc.UID{Name: "worker"}, ExportModes: modes}}
	r.reportProcessMetrics([]procmetrics.Snapshot{snapshot})
	assert.Zero(t, constructed)
	snapshot.Service.Features = export.FeatureProcess
	r.reportProcessMetrics([]procmetrics.Snapshot{snapshot})
	assert.Equal(t, 1, constructed)
	snapshot.Removed = true
	r.reportProcessMetrics([]procmetrics.Snapshot{snapshot})
	_, exists := r.reporters.Lookup(snapshot.Service.UID)
	assert.False(t, exists)
	r.reportProcessMetrics([]procmetrics.Snapshot{snapshot})
	assert.Equal(t, 1, constructed, "tombstones must not create reporters")
}

func newTestProcessMetrics(t *testing.T, selection attributes.Selection) (*ProcessMetrics, *metric.ManualReader) {
	t.Helper()
	reader := metric.NewManualReader()
	provider := metric.NewMeterProvider(metric.WithReader(reader))
	t.Cleanup(func() { require.NoError(t, provider.Shutdown(t.Context())) })
	selector, err := attributes.NewAttrSelector(attributes.UndefinedGroup, &attributes.SelectorConfig{SelectionCfg: selection})
	require.NoError(t, err)
	metrics := &ProcessMetrics{
		values: map[app.PID]*processMetricValues{},
		attrs:  map[attributes.Name][]attributes.Field[*procmetrics.Snapshot, attribute.KeyValue]{},
	}
	for _, name := range []attributes.Name{
		attributes.ProcessCPUTime, attributes.ProcessCPUUtilization,
		attributes.ProcessMemoryUsage, attributes.ProcessMemoryVirtual, attributes.ProcessDiskIO,
	} {
		metrics.attrs[name] = attributes.OpenTelemetryGetters(procmetrics.OTELGetters, selector.For(name))
	}
	require.NoError(t, setupProcessMeters(metrics, provider.Meter(reporterName)))
	return metrics, reader
}

func collectProcessMetrics(t *testing.T, reader *metric.ManualReader) map[string]metricdata.Aggregation {
	t.Helper()
	var data metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(t.Context(), &data))
	result := map[string]metricdata.Aggregation{}
	for _, scope := range data.ScopeMetrics {
		for _, metric := range scope.Metrics {
			result[metric.Name] = metric.Data
		}
	}
	return result
}
