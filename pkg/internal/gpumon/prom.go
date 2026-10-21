// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package gpumon

import (
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
)

// PromReporter reports each snapshot to a Prometheus endpoint. It implements
// prometheus.Collector, so it can be registered as any other collector.
type PromReporter struct {
	info           *prometheus.GaugeVec
	up             *prometheus.GaugeVec
	numa           *prometheus.GaugeVec
	pcieGeneration *prometheus.GaugeVec
	pcieWidth      *prometheus.GaugeVec
	nvlinkUp       *prometheus.GaugeVec
	nvlinkRate     *prometheus.GaugeVec
	nvlinkWidth    *prometheus.GaugeVec
	processInfo    *prometheus.GaugeVec
	conflicts      *prometheus.CounterVec
	aer            *prometheus.CounterVec

	// Static labels appended to every series, parsed from
	// OTEL_RESOURCE_ATTRIBUTES.
	extra ExtraLabels
}

// NewPromReporter creates a Prometheus reporter for GPU device metrics, with
// the given static labels appended to every series.
func NewPromReporter(extra ExtraLabels) *PromReporter {
	r := &PromReporter{extra: extra}
	r.info = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: metricGPUInfo,
	}, r.labelNames(
		labelGPUUUID,
		labelGPUPciBusID,
		labelHWModel,
		labelGPUMigCapable,
	))
	r.up = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: metricGPUUp,
	}, r.labelNames(
		labelGPUUUID,
	))
	r.numa = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: metricGPUNumaNode,
	}, r.labelNames(
		labelGPUUUID,
	))
	r.pcieGeneration = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: metricGPUPcieGeneration,
	}, r.labelNames(
		labelGPUUUID,
	))
	r.pcieWidth = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: metricGPUPcieWidth,
	}, r.labelNames(
		labelGPUUUID,
	))
	r.nvlinkUp = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: metricGPUNvlinkUp,
	}, r.labelNames(
		labelGPUUUID,
		labelGPUNvlinkLinkID,
		labelGPUPeerUUID,
		labelGPUNvlinkSwitchID,
	))
	r.nvlinkRate = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: metricGPUNvlinkRate,
	}, r.labelNames(
		labelGPUUUID,
		labelGPUNvlinkLinkID,
		labelGPUPeerUUID,
		labelGPUNvlinkSwitchID,
	))
	r.nvlinkWidth = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: metricGPUNvlinkWidth,
	}, r.labelNames(
		labelGPUUUID,
		labelGPUNvlinkLinkID,
		labelGPUPeerUUID,
		labelGPUNvlinkSwitchID,
	))
	r.processInfo = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: metricGPUProcessInfo,
	}, r.labelNames(
		labelGPUUUID,
		labelProcessPID,
		labelProcessExecutable,
		labelK8sPodName,
		labelK8sJobName,
		labelMLRank,
	))
	r.conflicts = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: metricGPUOccupancyConflict,
	}, r.labelNames(
		labelGPUUUID,
		labelK8sPodName,
		labelK8sJobName,
		labelMLRank,
	))
	r.aer = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: metricGPUPcieAerErrors,
	}, r.labelNames(
		labelGPUUUID,
		labelGPUPcieAerSeverity,
		labelGPUPciBusID,
	))
	return r
}

// labelNames prepends the per-metric label names with the static extra label
// names.
func (r *PromReporter) labelNames(names ...string) []string {
	return append(names, r.extra.keys()...)
}

// labelValues prepends the per-metric label values with the static extra
// label values; the result lines up with the vec's label names.
func (r *PromReporter) labelValues(values ...string) []string {
	return append(values, r.extra.values()...)
}

// Report implements Reporter. State of the gauges is wiped and rebuilt on
// every cycle, since the snapshot carries the full current state; the
// counters accumulate across cycles.
func (r *PromReporter) Report(s *Snapshot) {
	r.reportDevices(s.Devices)
	r.reportNVLinks(s.NVLinks)
	r.reportProcesses(s)
}

func (r *PromReporter) reportDevices(devices []Device) {
	r.info.DeletePartialMatch(prometheus.Labels{})
	r.up.DeletePartialMatch(prometheus.Labels{})
	r.numa.DeletePartialMatch(prometheus.Labels{})
	r.pcieGeneration.DeletePartialMatch(prometheus.Labels{})
	r.pcieWidth.DeletePartialMatch(prometheus.Labels{})

	for _, d := range devices {
		r.up.WithLabelValues(r.labelValues(d.UUID)...).Set(boolToFloat(d.Up))
		if d.BusID == "" {
			// Ghost device: only the up=0 transition is reported.
			continue
		}
		r.info.WithLabelValues(r.labelValues(d.UUID, d.BusID, d.Model, strconv.FormatBool(d.MigCapable))...).Set(1)
		if d.NumaNode >= 0 {
			r.numa.WithLabelValues(r.labelValues(d.UUID)...).Set(float64(d.NumaNode))
		}
		if d.PCIeGeneration > 0 {
			r.pcieGeneration.WithLabelValues(r.labelValues(d.UUID)...).Set(float64(d.PCIeGeneration))
		}
		if d.PCIeWidth > 0 {
			r.pcieWidth.WithLabelValues(r.labelValues(d.UUID)...).Set(float64(d.PCIeWidth))
		}
	}
}

func (r *PromReporter) reportNVLinks(links []NVLink) {
	r.nvlinkUp.DeletePartialMatch(prometheus.Labels{})
	r.nvlinkRate.DeletePartialMatch(prometheus.Labels{})
	r.nvlinkWidth.DeletePartialMatch(prometheus.Labels{})

	for _, l := range links {
		linkID := strconv.Itoa(l.LinkID)
		r.nvlinkUp.WithLabelValues(r.labelValues(l.GPUUUID, linkID, l.PeerUUID, l.SwitchID)...).Set(boolToFloat(l.Active))
		if l.Rate > 0 {
			r.nvlinkRate.WithLabelValues(r.labelValues(l.GPUUUID, linkID, l.PeerUUID, l.SwitchID)...).Set(float64(l.Rate))
		}
		if l.Width > 0 {
			r.nvlinkWidth.WithLabelValues(r.labelValues(l.GPUUUID, linkID, l.PeerUUID, l.SwitchID)...).Set(float64(l.Width))
		}
	}
}

func (r *PromReporter) reportProcesses(s *Snapshot) {
	r.processInfo.DeletePartialMatch(prometheus.Labels{})

	for _, p := range s.Processes {
		r.processInfo.WithLabelValues(r.labelValues(
			p.GPUUUID,
			strconv.Itoa(p.PID),
			p.Identity.Executable,
			p.Identity.PodName,
			p.Identity.JobName,
			p.Identity.Rank,
		)...).Set(1)
	}

	for _, p := range s.Conflicts {
		r.conflicts.WithLabelValues(r.labelValues(
			p.GPUUUID,
			p.Identity.PodName,
			p.Identity.JobName,
			p.Identity.Rank,
		)...).Inc()
	}

	for _, d := range s.AER {
		r.aer.WithLabelValues(r.labelValues(d.GPUUUID, d.Severity, d.BusID)...).Add(float64(d.Count))
	}
}

// Describe implements prometheus.Collector.
func (r *PromReporter) Describe(ch chan<- *prometheus.Desc) {
	r.info.Describe(ch)
	r.up.Describe(ch)
	r.numa.Describe(ch)
	r.pcieGeneration.Describe(ch)
	r.pcieWidth.Describe(ch)
	r.nvlinkUp.Describe(ch)
	r.nvlinkRate.Describe(ch)
	r.nvlinkWidth.Describe(ch)
	r.processInfo.Describe(ch)
	r.conflicts.Describe(ch)
	r.aer.Describe(ch)
}

// Collect implements prometheus.Collector.
func (r *PromReporter) Collect(ch chan<- prometheus.Metric) {
	r.info.Collect(ch)
	r.up.Collect(ch)
	r.numa.Collect(ch)
	r.pcieGeneration.Collect(ch)
	r.pcieWidth.Collect(ch)
	r.nvlinkUp.Collect(ch)
	r.nvlinkRate.Collect(ch)
	r.nvlinkWidth.Collect(ch)
	r.processInfo.Collect(ch)
	r.conflicts.Collect(ch)
	r.aer.Collect(ch)
}

func boolToFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
