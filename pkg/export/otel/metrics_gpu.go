// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package otel // import "go.opentelemetry.io/obi/pkg/export/otel"

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/attribute"

	"go.opentelemetry.io/obi/pkg/export/attributes"
	attr "go.opentelemetry.io/obi/pkg/export/attributes/names"
	"go.opentelemetry.io/obi/pkg/export/otel/metric"
	instrument "go.opentelemetry.io/obi/pkg/export/otel/metric/api/metric"
	"go.opentelemetry.io/obi/pkg/export/otel/otelcfg"
	"go.opentelemetry.io/obi/pkg/gpumon"
	"go.opentelemetry.io/obi/pkg/pipe/global"
)

const gpuMeterName = "obi_gpu"

// GPUMetricsReporter reports node-level GPU device metrics through the shared
// OTLP metrics exporter. It implements gpumon.Reporter.
type GPUMetricsReporter struct {
	provider *metric.MeterProvider

	info     instrument.Int64UpDownCounter
	process  instrument.Int64UpDownCounter
	up       instrument.Int64Gauge
	numa     instrument.Int64Gauge
	pcieGen  instrument.Int64Gauge
	pcieW    instrument.Int64Gauge
	nvUp     instrument.Int64Gauge
	nvRate   instrument.Int64Gauge
	nvWidth  instrument.Int64Gauge
	conflict instrument.Int64Counter
	aer      instrument.Int64Counter

	// Attribute sets recorded in the previous snapshot, for presence diffing:
	// updowncounters are decremented and removed, gauge series that went away
	// are removed so stale values stop being re-exported.
	prevInfo    map[gpuInfoKey][]attribute.KeyValue
	prevProcess map[gpuProcessKey][]attribute.KeyValue
	prevUp      map[string]struct{}
	prevNuma    map[string]struct{}
	prevPcieGen map[string]struct{}
	prevPcieW   map[string]struct{}
	prevNVLink  map[gpuNVLinkKey][]attribute.KeyValue
}

// gpuInfoKey identifies one obi.gpu.info attribute set.
type gpuInfoKey struct {
	uuid       string
	busID      string
	model      string
	migCapable bool
}

// gpuProcessKey identifies one obi.gpu.process.info attribute set.
type gpuProcessKey struct {
	uuid       string
	pid        int
	executable string
	pod        string
	job        string
	rank       string
}

// gpuNVLinkKey identifies one NVLink gauge attribute set. Peer and switch are
// part of the key so a topology change removes the series of the old link.
type gpuNVLinkKey struct {
	uuid     string
	linkID   int
	peer     string
	switchID string
}

// NewGPUMetricsReporter instantiates the shared OTLP metrics exporter (without
// owning its lifecycle) and creates a meter provider for the GPU metrics.
func NewGPUMetricsReporter(
	ctx context.Context,
	ctxInfo *global.ContextInfo,
	cfg *otelcfg.MetricsConfig,
) (*GPUMetricsReporter, error) {
	exporter, err := ctxInfo.OTELMetricsExporter.Instantiate(ctx)
	if err != nil {
		return nil, fmt.Errorf("instantiating metrics exporter for GPU metrics: %w", err)
	}

	provider := metric.NewMeterProvider(
		metric.WithResource(newResourceInternal(&ctxInfo.NodeMeta)),
		metric.WithReader(metric.NewPeriodicReader(sharedExporter{exporter},
			metric.WithInterval(cfg.Interval))),
	)

	reporter := &GPUMetricsReporter{
		provider: provider,
	}
	if err := reporter.setupInstruments(provider.Meter(gpuMeterName)); err != nil {
		return nil, err
	}
	return reporter, nil
}

func (r *GPUMetricsReporter) setupInstruments(meter instrument.Meter) error {
	var err error
	if r.info, err = meter.Int64UpDownCounter(attributes.GPUDeviceInfo.OTEL); err != nil {
		return fmt.Errorf("creating GPU info metric: %w", err)
	}
	if r.process, err = meter.Int64UpDownCounter(attributes.GPUProcessInfo.OTEL); err != nil {
		return fmt.Errorf("creating GPU process info metric: %w", err)
	}
	if r.up, err = meter.Int64Gauge(attributes.GPUUp.OTEL); err != nil {
		return fmt.Errorf("creating GPU up metric: %w", err)
	}
	if r.numa, err = meter.Int64Gauge(attributes.GPUNumaNode.OTEL); err != nil {
		return fmt.Errorf("creating GPU NUMA node metric: %w", err)
	}
	if r.pcieGen, err = meter.Int64Gauge(attributes.GPUPcieGeneration.OTEL); err != nil {
		return fmt.Errorf("creating GPU PCIe generation metric: %w", err)
	}
	if r.pcieW, err = meter.Int64Gauge(attributes.GPUPcieWidth.OTEL); err != nil {
		return fmt.Errorf("creating GPU PCIe width metric: %w", err)
	}
	if r.nvUp, err = meter.Int64Gauge(attributes.GPUNvlinkUp.OTEL); err != nil {
		return fmt.Errorf("creating NVLink up metric: %w", err)
	}
	if r.nvRate, err = meter.Int64Gauge(attributes.GPUNvlinkRate.OTEL); err != nil {
		return fmt.Errorf("creating NVLink rate metric: %w", err)
	}
	if r.nvWidth, err = meter.Int64Gauge(attributes.GPUNvlinkWidth.OTEL); err != nil {
		return fmt.Errorf("creating NVLink width metric: %w", err)
	}
	if r.conflict, err = meter.Int64Counter(attributes.GPUOccupancyConflict.OTEL); err != nil {
		return fmt.Errorf("creating GPU occupancy conflict metric: %w", err)
	}
	if r.aer, err = meter.Int64Counter(attributes.GPUPcieAerErrors.OTEL); err != nil {
		return fmt.Errorf("creating GPU PCIe AER errors metric: %w", err)
	}
	return nil
}

// Close shuts down the GPU meter provider, performing a final collection.
// The underlying exporter is shared with the other metric families and must
// not be shut down here.
func (r *GPUMetricsReporter) Close(ctx context.Context) error {
	return r.provider.Shutdown(ctx)
}

// Report emits one collection cycle's snapshot.
func (r *GPUMetricsReporter) Report(s *gpumon.Snapshot) {
	ctx := context.Background()
	r.reportDevices(ctx, s.Devices)
	r.reportNVLinks(ctx, s.NVLinks)
	r.reportProcesses(ctx, s.Processes)
	r.reportConflicts(ctx, s.Conflicts)
	r.reportAER(ctx, s.AER)
}

// reportDevices reports the per-device gauges and diffs the obi.gpu.info
// attribute sets. Ghost devices (gone since the previous cycle) only carry
// their UUID; they report up=0 for one cycle and evict the other series.
func (r *GPUMetricsReporter) reportDevices(ctx context.Context, devices []gpumon.Device) {
	currentInfo := make(map[gpuInfoKey][]attribute.KeyValue, len(devices))
	currentUp := make(map[string]struct{}, len(devices))
	currentNuma := make(map[string]struct{}, len(devices))
	currentGen := make(map[string]struct{}, len(devices))
	currentWidth := make(map[string]struct{}, len(devices))

	for _, d := range devices {
		uuidAttr := attr.GPUUUID.OTEL().String(d.UUID)
		currentUp[d.UUID] = struct{}{}
		upVal := int64(0)
		if d.Up {
			upVal = 1
		}
		r.up.Record(ctx, upVal, instrument.WithAttributes(uuidAttr))
		if d.BusID == "" { // ghost device: only UUID is known
			continue
		}

		if d.NumaNode >= 0 {
			currentNuma[d.UUID] = struct{}{}
			r.numa.Record(ctx, int64(d.NumaNode), instrument.WithAttributes(uuidAttr))
		}
		if d.PCIeGeneration > 0 {
			currentGen[d.UUID] = struct{}{}
			r.pcieGen.Record(ctx, int64(d.PCIeGeneration), instrument.WithAttributes(uuidAttr))
		}
		if d.PCIeWidth > 0 {
			currentWidth[d.UUID] = struct{}{}
			r.pcieW.Record(ctx, int64(d.PCIeWidth), instrument.WithAttributes(uuidAttr))
		}

		kvs := []attribute.KeyValue{uuidAttr}
		kvs = append(kvs, attr.GPUPciBusID.OTEL().String(d.BusID))
		if d.Model != "" {
			kvs = append(kvs, attr.HWModel.OTEL().String(d.Model))
		}
		if d.MigCapable {
			kvs = append(kvs, attr.GPUMigCapable.OTEL().Bool(true))
		}
		currentInfo[gpuInfoKey{d.UUID, d.BusID, d.Model, d.MigCapable}] = kvs
	}

	r.diffInfo(ctx, currentInfo)
	r.prevUp = diffUUIDSeries(ctx, r.up, r.prevUp, currentUp)
	r.prevNuma = diffUUIDSeries(ctx, r.numa, r.prevNuma, currentNuma)
	r.prevPcieGen = diffUUIDSeries(ctx, r.pcieGen, r.prevPcieGen, currentGen)
	r.prevPcieW = diffUUIDSeries(ctx, r.pcieW, r.prevPcieW, currentWidth)
}

// diffInfo reconciles the obi.gpu.info attribute sets with the previous
// snapshot: new sets are incremented to 1, gone sets are decremented and
// removed.
func (r *GPUMetricsReporter) diffInfo(ctx context.Context, current map[gpuInfoKey][]attribute.KeyValue) {
	for key, kvs := range current {
		if _, ok := r.prevInfo[key]; !ok {
			r.info.Add(ctx, 1, instrument.WithAttributes(kvs...))
		}
	}
	for key, kvs := range r.prevInfo {
		if _, ok := current[key]; !ok {
			r.info.Add(ctx, -1, instrument.WithAttributes(kvs...))
			r.info.Remove(ctx, instrument.WithAttributes(kvs...))
		}
	}
	r.prevInfo = current
}

// diffUUIDSeries removes the gauge series of UUIDs that are gone from the
// current snapshot, so their last value stops being re-exported, and returns
// the current set as the new previous set.
func diffUUIDSeries(
	ctx context.Context,
	gauge instrument.Int64Gauge,
	prev map[string]struct{},
	current map[string]struct{},
) map[string]struct{} {
	for uuid := range prev {
		if _, ok := current[uuid]; !ok {
			gauge.Remove(ctx, instrument.WithAttributes(attr.GPUUUID.OTEL().String(uuid)))
		}
	}
	return current
}

// reportNVLinks reports the NVLink gauges, removing series of links that are
// gone or whose topology changed.
func (r *GPUMetricsReporter) reportNVLinks(ctx context.Context, links []gpumon.NVLink) {
	current := make(map[gpuNVLinkKey][]attribute.KeyValue, len(links))
	for _, l := range links {
		kvs := []attribute.KeyValue{
			attr.GPUUUID.OTEL().String(l.GPUUUID),
			attr.GPUNvlinkLinkID.OTEL().Int(l.LinkID),
		}
		if l.PeerUUID != "" {
			kvs = append(kvs, attr.GPUPeerUUID.OTEL().String(l.PeerUUID))
		}
		if l.SwitchID != "" {
			kvs = append(kvs, attr.GPUNvlinkSwitchID.OTEL().String(l.SwitchID))
		}
		key := gpuNVLinkKey{l.GPUUUID, l.LinkID, l.PeerUUID, l.SwitchID}
		current[key] = kvs

		upVal := int64(0)
		if l.Active {
			upVal = 1
		}
		r.nvUp.Record(ctx, upVal, instrument.WithAttributes(kvs...))
		if l.Rate > 0 {
			r.nvRate.Record(ctx, int64(l.Rate), instrument.WithAttributes(kvs...))
		}
		if l.Width > 0 {
			r.nvWidth.Record(ctx, int64(l.Width), instrument.WithAttributes(kvs...))
		}
	}

	for key, kvs := range r.prevNVLink {
		if _, ok := current[key]; !ok {
			r.nvUp.Remove(ctx, instrument.WithAttributes(kvs...))
			r.nvRate.Remove(ctx, instrument.WithAttributes(kvs...))
			r.nvWidth.Remove(ctx, instrument.WithAttributes(kvs...))
		}
	}
	r.prevNVLink = current
}

// reportProcesses diffs the obi.gpu.process.info attribute sets.
func (r *GPUMetricsReporter) reportProcesses(ctx context.Context, processes []gpumon.GPUProcess) {
	current := make(map[gpuProcessKey][]attribute.KeyValue, len(processes))
	for _, p := range processes {
		kvs := []attribute.KeyValue{
			attr.GPUUUID.OTEL().String(p.GPUUUID),
			attr.ProcessPID.OTEL().Int(p.PID),
		}
		if p.Identity.Executable != "" {
			kvs = append(kvs, attr.ProcessExecutableName.OTEL().String(p.Identity.Executable))
		}
		if p.Identity.PodName != "" {
			kvs = append(kvs, attr.K8sPodName.OTEL().String(p.Identity.PodName))
		}
		if p.Identity.JobName != "" {
			kvs = append(kvs, attr.K8sJobName.OTEL().String(p.Identity.JobName))
		}
		if p.Identity.Rank != "" {
			kvs = append(kvs, attr.MLRank.OTEL().String(p.Identity.Rank))
		}
		key := gpuProcessKey{
			uuid:       p.GPUUUID,
			pid:        p.PID,
			executable: p.Identity.Executable,
			pod:        p.Identity.PodName,
			job:        p.Identity.JobName,
			rank:       p.Identity.Rank,
		}
		current[key] = kvs
	}

	for key, kvs := range current {
		if _, ok := r.prevProcess[key]; !ok {
			r.process.Add(ctx, 1, instrument.WithAttributes(kvs...))
		}
	}
	for key, kvs := range r.prevProcess {
		if _, ok := current[key]; !ok {
			r.process.Add(ctx, -1, instrument.WithAttributes(kvs...))
			r.process.Remove(ctx, instrument.WithAttributes(kvs...))
		}
	}
	r.prevProcess = current
}

// reportConflicts counts one per process that still shares its GPU with
// another distinct workload identity.
func (r *GPUMetricsReporter) reportConflicts(ctx context.Context, conflicts []gpumon.GPUProcess) {
	for _, p := range conflicts {
		kvs := []attribute.KeyValue{
			attr.GPUUUID.OTEL().String(p.GPUUUID),
			attr.K8sPodName.OTEL().String(p.Identity.PodName),
		}
		if p.Identity.JobName != "" {
			kvs = append(kvs, attr.K8sJobName.OTEL().String(p.Identity.JobName))
		}
		if p.Identity.Rank != "" {
			kvs = append(kvs, attr.MLRank.OTEL().String(p.Identity.Rank))
		}
		r.conflict.Add(ctx, 1, instrument.WithAttributes(kvs...))
	}
}

// reportAER adds the AER error deltas of the cycle.
func (r *GPUMetricsReporter) reportAER(ctx context.Context, deltas []gpumon.AERDelta) {
	for _, d := range deltas {
		kvs := []attribute.KeyValue{
			attr.GPUUUID.OTEL().String(d.GPUUUID),
			attr.GPUPcieAerSeverity.OTEL().String(d.Severity),
		}
		if d.BusID != "" {
			kvs = append(kvs, attr.GPUPciBusID.OTEL().String(d.BusID))
		}
		r.aer.Add(ctx, d.Count, instrument.WithAttributes(kvs...))
	}
}
