// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package gpumon // import "go.opentelemetry.io/obi/pkg/gpumon"

import (
	"strconv"

	"github.com/prometheus/client_golang/prometheus"

	"go.opentelemetry.io/obi/pkg/export/attributes"
	attr "go.opentelemetry.io/obi/pkg/export/attributes/names"
)

// PromReporter reports each snapshot to the Prometheus endpoint of the agent.
// It implements prometheus.Collector, so it can be registered as any other
// collector.
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
}

// NewPromReporter creates a Prometheus reporter for GPU device metrics.
func NewPromReporter() *PromReporter {
	return &PromReporter{
		info: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: attributes.GPUDeviceInfo.Prom,
		}, []string{
			attr.GPUUUID.Prom(),
			attr.GPUPciBusID.Prom(),
			attr.HWModel.Prom(),
			attr.GPUMigCapable.Prom(),
		}),
		up: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: attributes.GPUUp.Prom,
		}, []string{
			attr.GPUUUID.Prom(),
		}),
		numa: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: attributes.GPUNumaNode.Prom,
		}, []string{
			attr.GPUUUID.Prom(),
		}),
		pcieGeneration: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: attributes.GPUPcieGeneration.Prom,
		}, []string{
			attr.GPUUUID.Prom(),
		}),
		pcieWidth: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: attributes.GPUPcieWidth.Prom,
		}, []string{
			attr.GPUUUID.Prom(),
		}),
		nvlinkUp: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: attributes.GPUNvlinkUp.Prom,
		}, []string{
			attr.GPUUUID.Prom(),
			attr.GPUNvlinkLinkID.Prom(),
			attr.GPUPeerUUID.Prom(),
			attr.GPUNvlinkSwitchID.Prom(),
		}),
		nvlinkRate: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: attributes.GPUNvlinkRate.Prom,
		}, []string{
			attr.GPUUUID.Prom(),
			attr.GPUNvlinkLinkID.Prom(),
			attr.GPUPeerUUID.Prom(),
			attr.GPUNvlinkSwitchID.Prom(),
		}),
		nvlinkWidth: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: attributes.GPUNvlinkWidth.Prom,
		}, []string{
			attr.GPUUUID.Prom(),
			attr.GPUNvlinkLinkID.Prom(),
			attr.GPUPeerUUID.Prom(),
			attr.GPUNvlinkSwitchID.Prom(),
		}),
		processInfo: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: attributes.GPUProcessInfo.Prom,
		}, []string{
			attr.GPUUUID.Prom(),
			attr.ProcessPID.Prom(),
			attr.ProcessExecutableName.Prom(),
			attr.K8sPodName.Prom(),
			attr.K8sJobName.Prom(),
			attr.MLRank.Prom(),
		}),
		conflicts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: attributes.GPUOccupancyConflict.Prom,
		}, []string{
			attr.GPUUUID.Prom(),
			attr.K8sPodName.Prom(),
			attr.K8sJobName.Prom(),
			attr.MLRank.Prom(),
		}),
		aer: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: attributes.GPUPcieAerErrors.Prom,
		}, []string{
			attr.GPUUUID.Prom(),
			attr.GPUPcieAerSeverity.Prom(),
			attr.GPUPciBusID.Prom(),
		}),
	}
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
		r.up.WithLabelValues(d.UUID).Set(boolToFloat(d.Up))
		if d.BusID == "" {
			// Ghost device: only the up=0 transition is reported.
			continue
		}
		r.info.WithLabelValues(d.UUID, d.BusID, d.Model, strconv.FormatBool(d.MigCapable)).Set(1)
		if d.NumaNode >= 0 {
			r.numa.WithLabelValues(d.UUID).Set(float64(d.NumaNode))
		}
		if d.PCIeGeneration > 0 {
			r.pcieGeneration.WithLabelValues(d.UUID).Set(float64(d.PCIeGeneration))
		}
		if d.PCIeWidth > 0 {
			r.pcieWidth.WithLabelValues(d.UUID).Set(float64(d.PCIeWidth))
		}
	}
}

func (r *PromReporter) reportNVLinks(links []NVLink) {
	r.nvlinkUp.DeletePartialMatch(prometheus.Labels{})
	r.nvlinkRate.DeletePartialMatch(prometheus.Labels{})
	r.nvlinkWidth.DeletePartialMatch(prometheus.Labels{})

	for _, l := range links {
		linkID := strconv.Itoa(l.LinkID)
		r.nvlinkUp.WithLabelValues(l.GPUUUID, linkID, l.PeerUUID, l.SwitchID).Set(boolToFloat(l.Active))
		if l.Rate > 0 {
			r.nvlinkRate.WithLabelValues(l.GPUUUID, linkID, l.PeerUUID, l.SwitchID).Set(float64(l.Rate))
		}
		if l.Width > 0 {
			r.nvlinkWidth.WithLabelValues(l.GPUUUID, linkID, l.PeerUUID, l.SwitchID).Set(float64(l.Width))
		}
	}
}

func (r *PromReporter) reportProcesses(s *Snapshot) {
	r.processInfo.DeletePartialMatch(prometheus.Labels{})

	for _, p := range s.Processes {
		r.processInfo.WithLabelValues(
			p.GPUUUID,
			strconv.Itoa(p.PID),
			p.Identity.Executable,
			p.Identity.PodName,
			p.Identity.JobName,
			p.Identity.Rank,
		).Set(1)
	}

	for _, p := range s.Conflicts {
		r.conflicts.WithLabelValues(
			p.GPUUUID,
			p.Identity.PodName,
			p.Identity.JobName,
			p.Identity.Rank,
		).Inc()
	}

	for _, d := range s.AER {
		r.aer.WithLabelValues(d.GPUUUID, d.Severity, d.BusID).Add(float64(d.Count))
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
