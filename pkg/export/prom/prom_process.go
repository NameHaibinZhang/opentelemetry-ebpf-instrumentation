// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package prom // import "go.opentelemetry.io/obi/pkg/export/prom"

import (
	"context"
	"strconv"

	"github.com/prometheus/client_golang/prometheus"

	"go.opentelemetry.io/obi/pkg/export/attributes"
	attr "go.opentelemetry.io/obi/pkg/export/attributes/names"
	"go.opentelemetry.io/obi/pkg/internal/procmetrics"
	"go.opentelemetry.io/obi/pkg/pipe/swarm/swarms"
)

// processMetricsCollector exposes process metrics
// sampled from /proc by the procmetrics collector.
type processMetricsCollector struct {
	cpuTime        *prometheus.CounterVec
	cpuUtilization *prometheus.GaugeVec
	memoryUsage    *prometheus.GaugeVec
	memoryVirtual  *prometheus.GaugeVec
	diskIO         *prometheus.CounterVec
	attrs          map[attributes.Name][]attributes.Field[*procmetrics.Snapshot, string]
}

func newProcessMetricsCollector(selector *attributes.AttrSelector) processMetricsCollector {
	attrs := map[attributes.Name][]attributes.Field[*procmetrics.Snapshot, string]{}
	for _, name := range []attributes.Name{
		attributes.ProcessCPUTime, attributes.ProcessCPUUtilization,
		attributes.ProcessMemoryUsage, attributes.ProcessMemoryVirtual, attributes.ProcessDiskIO,
	} {
		attrs[name] = attributes.PrometheusGetters(procmetrics.PromGetters, selector.For(name))
	}
	labels := func(name attributes.Name, dimensions ...string) []string {
		return append(append(labelNames(attrs[name]), attr.ProcessPID.Prom()), dimensions...)
	}

	return processMetricsCollector{
		cpuTime: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: attributes.ProcessCPUTime.Prom,
			Help: "CPU time of the process.",
		}, labels(attributes.ProcessCPUTime, attr.CPUMode.Prom())),
		cpuUtilization: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: attributes.ProcessCPUUtilization.Prom,
			Help: "CPU utilization of the process.",
		}, labels(attributes.ProcessCPUUtilization, attr.CPUMode.Prom())),
		memoryUsage: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: attributes.ProcessMemoryUsage.Prom,
			Help: "Memory usage of the process.",
		}, labels(attributes.ProcessMemoryUsage)),
		memoryVirtual: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: attributes.ProcessMemoryVirtual.Prom,
			Help: "Virtual memory size of the process.",
		}, labels(attributes.ProcessMemoryVirtual)),
		diskIO: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: attributes.ProcessDiskIO.Prom,
			Help: "Disk IO bytes of the process.",
		}, labels(attributes.ProcessDiskIO, attr.DiskIODirection.Prom())),
		attrs: attrs,
	}
}

func (c *processMetricsCollector) collectors() []prometheus.Collector {
	return []prometheus.Collector{
		c.cpuTime,
		c.cpuUtilization,
		c.memoryUsage,
		c.memoryVirtual,
		c.diskIO,
	}
}

func (r *metricsReporter) watchForProcessMetrics(ctx context.Context) {
	log := mlog().With("function", "watchForProcessMetrics")
	swarms.ForEachInput(ctx, r.processInput, log.Debug, r.collectProcessMetrics)
}

func (r *metricsReporter) collectProcessMetrics(snapshots []procmetrics.Snapshot) {
	for i := range snapshots {
		snapshot := &snapshots[i]
		if !snapshot.Removed && (!snapshot.Service.ExportModes.CanExportMetrics() || !snapshot.Service.Features.Process()) {
			continue
		}
		if snapshot.Removed {
			r.deleteProcessMetrics(snapshot)
			continue
		}

		c := &r.processMetrics
		c.cpuTime.WithLabelValues(c.labels(snapshot, attributes.ProcessCPUTime, procmetrics.CPUModeUser)...).Add(snapshot.CPUTimeUserSeconds)
		c.cpuTime.WithLabelValues(c.labels(snapshot, attributes.ProcessCPUTime, procmetrics.CPUModeSystem)...).Add(snapshot.CPUTimeSystemSeconds)
		c.cpuUtilization.WithLabelValues(c.labels(snapshot, attributes.ProcessCPUUtilization, procmetrics.CPUModeUser)...).Set(snapshot.CPUUtilizationUser)
		c.cpuUtilization.WithLabelValues(c.labels(snapshot, attributes.ProcessCPUUtilization, procmetrics.CPUModeSystem)...).Set(snapshot.CPUUtilizationSystem)

		c.memoryUsage.WithLabelValues(c.labels(snapshot, attributes.ProcessMemoryUsage)...).Set(float64(snapshot.MemoryUsageBytes))
		c.memoryVirtual.WithLabelValues(c.labels(snapshot, attributes.ProcessMemoryVirtual)...).Set(float64(snapshot.MemoryVirtualBytes))

		c.diskIO.WithLabelValues(c.labels(snapshot, attributes.ProcessDiskIO, procmetrics.DiskDirectionRead)...).Add(float64(snapshot.DiskReadBytes))
		c.diskIO.WithLabelValues(c.labels(snapshot, attributes.ProcessDiskIO, procmetrics.DiskDirectionWrite)...).Add(float64(snapshot.DiskWriteBytes))
	}
}

func (c *processMetricsCollector) labels(snapshot *procmetrics.Snapshot, name attributes.Name, dimensions ...string) []string {
	values := append(labelValues(snapshot, c.attrs[name]), strconv.Itoa(int(snapshot.PID)))
	return append(values, dimensions...)
}

func (r *metricsReporter) deleteProcessMetrics(snapshot *procmetrics.Snapshot) {
	c := &r.processMetrics
	for _, mode := range []string{procmetrics.CPUModeUser, procmetrics.CPUModeSystem} {
		c.cpuTime.DeleteLabelValues(c.labels(snapshot, attributes.ProcessCPUTime, mode)...)
		c.cpuUtilization.DeleteLabelValues(c.labels(snapshot, attributes.ProcessCPUUtilization, mode)...)
	}
	c.memoryUsage.DeleteLabelValues(c.labels(snapshot, attributes.ProcessMemoryUsage)...)
	c.memoryVirtual.DeleteLabelValues(c.labels(snapshot, attributes.ProcessMemoryVirtual)...)
	for _, direction := range []string{procmetrics.DiskDirectionRead, procmetrics.DiskDirectionWrite} {
		c.diskIO.DeleteLabelValues(c.labels(snapshot, attributes.ProcessDiskIO, direction)...)
	}
}
