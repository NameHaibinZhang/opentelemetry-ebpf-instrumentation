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

// processMetricsCollector exposes the six Beyla-compatible process metrics
// sampled from /proc by the procmetrics collector.
type processMetricsCollector struct {
	cpuTime        *prometheus.CounterVec
	cpuUtilization *prometheus.GaugeVec
	memoryUsage    *prometheus.GaugeVec
	memoryVirtual  *prometheus.GaugeVec
	diskIO         *prometheus.CounterVec
	networkIO      *prometheus.CounterVec
}

func newProcessMetricsCollector(baseLabelNames []string) processMetricsCollector {
	pidModeLabels := append(append([]string{}, baseLabelNames...), attr.ProcessPID.Prom(), attr.CPUMode.Prom())
	pidDiskLabels := append(append([]string{}, baseLabelNames...), attr.ProcessPID.Prom(), attr.DiskIODirection.Prom())
	pidNetworkLabels := append(append([]string{}, baseLabelNames...), attr.ProcessPID.Prom(), attr.NetworkIoDirection.Prom())
	pidLabels := append(append([]string{}, baseLabelNames...), attr.ProcessPID.Prom())

	return processMetricsCollector{
		cpuTime: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: attributes.ProcessCPUTime.Prom,
			Help: "CPU time of the process.",
		}, pidModeLabels),
		cpuUtilization: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: attributes.ProcessCPUUtilization.Prom,
			Help: "CPU utilization of the process.",
		}, pidModeLabels),
		memoryUsage: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: attributes.ProcessMemoryUsage.Prom,
			Help: "Memory usage of the process.",
		}, pidLabels),
		memoryVirtual: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: attributes.ProcessMemoryVirtual.Prom,
			Help: "Virtual memory size of the process.",
		}, pidLabels),
		diskIO: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: attributes.ProcessDiskIO.Prom,
			Help: "Disk IO bytes of the process.",
		}, pidDiskLabels),
		networkIO: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: attributes.ProcessNetworkIO.Prom,
			Help: "Network IO bytes of the process.",
		}, pidNetworkLabels),
	}
}

func (c *processMetricsCollector) collectors() []prometheus.Collector {
	return []prometheus.Collector{
		c.cpuTime,
		c.cpuUtilization,
		c.memoryUsage,
		c.memoryVirtual,
		c.diskIO,
		c.networkIO,
	}
}

func (r *metricsReporter) watchForProcessMetrics(ctx context.Context) {
	log := mlog().With("function", "watchForProcessMetrics")
	swarms.ForEachInput(ctx, r.processInput, log.Debug, r.collectProcessMetrics)
}

func (r *metricsReporter) collectProcessMetrics(snapshots []procmetrics.Snapshot) {
	for i := range snapshots {
		snapshot := &snapshots[i]
		if !snapshot.Service.ExportModes.CanExportMetrics() {
			continue
		}
		baseLabels := r.labelValuesTargetInfo(&snapshot.Service)
		pid := strconv.Itoa(int(snapshot.PID))

		if snapshot.Removed {
			r.deleteProcessMetrics(baseLabels, pid)
			continue
		}

		userLabels := append(baseValues(baseLabels, pid), procmetrics.CPUModeUser)
		systemLabels := append(baseValues(baseLabels, pid), procmetrics.CPUModeSystem)
		r.processMetrics.cpuTime.WithLabelValues(userLabels...).Add(snapshot.CPUTimeUserSeconds)
		r.processMetrics.cpuTime.WithLabelValues(systemLabels...).Add(snapshot.CPUTimeSystemSeconds)
		r.processMetrics.cpuUtilization.WithLabelValues(userLabels...).Set(snapshot.CPUUtilizationUser)
		r.processMetrics.cpuUtilization.WithLabelValues(systemLabels...).Set(snapshot.CPUUtilizationSystem)

		pidLabels := baseValues(baseLabels, pid)
		r.processMetrics.memoryUsage.WithLabelValues(pidLabels...).Set(float64(snapshot.MemoryUsageBytes))
		r.processMetrics.memoryVirtual.WithLabelValues(pidLabels...).Set(float64(snapshot.MemoryVirtualBytes))

		readLabels := append(baseValues(baseLabels, pid), procmetrics.DiskDirectionRead)
		writeLabels := append(baseValues(baseLabels, pid), procmetrics.DiskDirectionWrite)
		r.processMetrics.diskIO.WithLabelValues(readLabels...).Add(float64(snapshot.DiskReadBytes))
		r.processMetrics.diskIO.WithLabelValues(writeLabels...).Add(float64(snapshot.DiskWriteBytes))

		receiveLabels := append(baseValues(baseLabels, pid), procmetrics.NetworkDirectionReceive)
		transmitLabels := append(baseValues(baseLabels, pid), procmetrics.NetworkDirectionTransmit)
		r.processMetrics.networkIO.WithLabelValues(receiveLabels...).Add(float64(snapshot.NetworkReceiveBytes))
		r.processMetrics.networkIO.WithLabelValues(transmitLabels...).Add(float64(snapshot.NetworkTransmitBytes))
	}
}

func baseValues(baseLabels []string, pid string) []string {
	values := make([]string, 0, len(baseLabels)+1)
	values = append(values, baseLabels...)
	return append(values, pid)
}

func (r *metricsReporter) deleteProcessMetrics(baseLabels []string, pid string) {
	r.processMetrics.cpuTime.DeleteLabelValues(append(baseValues(baseLabels, pid), procmetrics.CPUModeUser)...)
	r.processMetrics.cpuTime.DeleteLabelValues(append(baseValues(baseLabels, pid), procmetrics.CPUModeSystem)...)
	r.processMetrics.cpuUtilization.DeleteLabelValues(append(baseValues(baseLabels, pid), procmetrics.CPUModeUser)...)
	r.processMetrics.cpuUtilization.DeleteLabelValues(append(baseValues(baseLabels, pid), procmetrics.CPUModeSystem)...)
	r.processMetrics.memoryUsage.DeleteLabelValues(baseValues(baseLabels, pid)...)
	r.processMetrics.memoryVirtual.DeleteLabelValues(baseValues(baseLabels, pid)...)
	r.processMetrics.diskIO.DeleteLabelValues(append(baseValues(baseLabels, pid), procmetrics.DiskDirectionRead)...)
	r.processMetrics.diskIO.DeleteLabelValues(append(baseValues(baseLabels, pid), procmetrics.DiskDirectionWrite)...)
	r.processMetrics.networkIO.DeleteLabelValues(append(baseValues(baseLabels, pid), procmetrics.NetworkDirectionReceive)...)
	r.processMetrics.networkIO.DeleteLabelValues(append(baseValues(baseLabels, pid), procmetrics.NetworkDirectionTransmit)...)
}
