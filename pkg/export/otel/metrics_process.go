// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package otel // import "go.opentelemetry.io/obi/pkg/export/otel"

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"

	"go.opentelemetry.io/obi/pkg/appolly/app"
	"go.opentelemetry.io/obi/pkg/appolly/app/svc"
	"go.opentelemetry.io/obi/pkg/export/attributes"
	attr "go.opentelemetry.io/obi/pkg/export/attributes/names"
	"go.opentelemetry.io/obi/pkg/export/otel/metric"
	instrument "go.opentelemetry.io/obi/pkg/export/otel/metric/api/metric"
	"go.opentelemetry.io/obi/pkg/export/otel/otelcfg"
	"go.opentelemetry.io/obi/pkg/export/otel/perapp"
	"go.opentelemetry.io/obi/pkg/internal/procmetrics"
	"go.opentelemetry.io/obi/pkg/metadata"
	"go.opentelemetry.io/obi/pkg/pipe/global"
	"go.opentelemetry.io/obi/pkg/pipe/msg"
	"go.opentelemetry.io/obi/pkg/pipe/swarm"
)

func pmlog() *slog.Logger {
	return slog.With("component", "otel.ProcessMetricsReporter")
}

// ProcessMetricsReporter reports the process-level metrics that the
// procmetrics collector samples from /proc, following the OpenTelemetry
// process semantic conventions.
type ProcessMetricsReporter struct {
	ctx       context.Context
	cfg       *otelcfg.MetricsConfig
	nodeMeta  metadata.NodeMeta
	exporter  sdkmetric.Exporter
	reporters otelcfg.ReporterPool[*svc.Attrs, *ProcessMetrics]
	input     <-chan []procmetrics.Snapshot
	log       *slog.Logger
	selector  attributes.Selection
	attrs     map[attributes.Name][]attributes.Field[*procmetrics.Snapshot, attribute.KeyValue]
}

// ProcessMetrics holds the OTEL instruments for the process metrics of a
// single service.
type ProcessMetrics struct {
	ctx      context.Context
	service  *svc.Attrs
	provider *metric.MeterProvider

	cpuTime        instrument.Float64Counter
	cpuUtilization instrument.Float64Gauge
	memoryUsage    instrument.Int64UpDownCounter
	memoryVirtual  instrument.Int64UpDownCounter
	diskIO         instrument.Int64Counter
	attrs          map[attributes.Name][]attributes.Field[*procmetrics.Snapshot, attribute.KeyValue]

	mu     sync.Mutex
	values map[app.PID]*processMetricValues
}

// processMetricValues tracks the last recorded memory contributions per PID so
// that absolute samples can be converted to UpDownCounter deltas.
type processMetricValues struct {
	lastMemoryUsage   int64
	lastMemoryVirtual int64
}

// ReportProcessMetrics creates a graph node that receives procmetrics
// snapshots and exports them as OTEL metrics.
func ReportProcessMetrics(
	ctxInfo *global.ContextInfo,
	cfg *otelcfg.MetricsConfig,
	jointMetricsConfig *perapp.GlobalMetricsConfig,
	selectorCfg *attributes.SelectorConfig,
	input *msg.Queue[[]procmetrics.Snapshot],
) swarm.InstanceFunc {
	return func(ctx context.Context) (swarm.RunFunc, error) {
		if !cfg.EndpointEnabled() ||
			!jointMetricsConfig.Features.Process() ||
			input == nil {
			return swarm.EmptyRunFunc()
		}

		reporter, err := newProcessMetricsReporter(ctx, ctxInfo, cfg, selectorCfg, input)
		if err != nil {
			return nil, fmt.Errorf("instantiating OTEL process metrics reporter: %w", err)
		}

		return reporter.reportMetrics, nil
	}
}

func newProcessMetricsReporter(
	ctx context.Context,
	ctxInfo *global.ContextInfo,
	cfg *otelcfg.MetricsConfig,
	selectorCfg *attributes.SelectorConfig,
	input *msg.Queue[[]procmetrics.Snapshot],
) (*ProcessMetricsReporter, error) {
	selector, err := attributes.NewAttrSelector(ctxInfo.MetricAttributeGroups, selectorCfg)
	if err != nil {
		return nil, err
	}
	exporter, err := ctxInfo.OTELMetricsExporter.Instantiate(ctx)
	if err != nil {
		return nil, err
	}

	reporter := &ProcessMetricsReporter{
		ctx:      ctx,
		cfg:      cfg,
		nodeMeta: ctxInfo.NodeMeta,
		exporter: instrumentMetricsExporter(ctxInfo.Metrics, exporter),
		input:    input.Subscribe(msg.SubscriberName("otel.ProcessMetricsReporter")),
		log:      pmlog(),
		selector: selectorCfg.SelectionCfg,
		attrs:    map[attributes.Name][]attributes.Field[*procmetrics.Snapshot, attribute.KeyValue]{},
	}
	for _, name := range []attributes.Name{
		attributes.ProcessCPUTime, attributes.ProcessCPUUtilization,
		attributes.ProcessMemoryUsage, attributes.ProcessMemoryVirtual, attributes.ProcessDiskIO,
	} {
		reporter.attrs[name] = attributes.OpenTelemetryGetters(procmetrics.OTELGetters, selector.For(name))
	}

	reporter.reporters, err = otelcfg.NewReporterPool[*svc.Attrs, *ProcessMetrics](cfg.ReportersCacheLen, cfg.TTL, timeNow,
		func(_ svc.UID, v *ProcessMetrics) {
			llog := reporter.log.With("service", v.service.UID)
			llog.Debug("evicting process metrics reporter from cache")

			go func() {
				if err := v.provider.Shutdown(ctx); err != nil {
					llog.Warn("error shutting down evicted process metrics provider", "error", err)
				}
			}()
		}, reporter.newMetricSet)
	if err != nil {
		return nil, fmt.Errorf("creating process metrics reporters pool: %w", err)
	}

	return reporter, nil
}

func (r *ProcessMetricsReporter) newMetricSet(service *svc.Attrs) (*ProcessMetrics, error) {
	var resourceAttributes []attribute.KeyValue
	if service != nil {
		resourceAttributes = append(otelcfg.GetAppResourceAttrs(&r.nodeMeta, service), otelcfg.ResourceAttrsFromEnv(service)...)
		resourceAttributes = otelcfg.FilterResourceAttrs(resourceAttributes, r.selector)
	}

	provider := metric.NewMeterProvider(
		metric.WithResource(resource.NewWithAttributes(attr.OBISchemaURL, resourceAttributes...)),
		metric.WithReader(metric.NewPeriodicReader(sharedExporter{r.exporter},
			metric.WithInterval(r.cfg.Interval))))

	metrics := &ProcessMetrics{
		ctx:      r.ctx,
		service:  service,
		provider: provider,
		values:   map[app.PID]*processMetricValues{},
		attrs:    r.attrs,
	}

	if err := setupProcessMeters(metrics, provider.Meter(reporterName, obiMeterOptions()...)); err != nil {
		return nil, err
	}
	return metrics, nil
}

func setupProcessMeters(metrics *ProcessMetrics, meter instrument.Meter) error {
	var err error
	if metrics.cpuTime, err = meter.Float64Counter(string(attributes.ProcessCPUTime.OTEL),
		instrument.WithUnit(attributes.ProcessCPUTime.Unit),
		instrument.WithDescription("CPU time of the process"),
	); err != nil {
		return err
	}
	if metrics.cpuUtilization, err = meter.Float64Gauge(string(attributes.ProcessCPUUtilization.OTEL),
		instrument.WithUnit(attributes.ProcessCPUUtilization.Unit),
		instrument.WithDescription("CPU utilization of the process"),
	); err != nil {
		return err
	}
	if metrics.memoryUsage, err = meter.Int64UpDownCounter(string(attributes.ProcessMemoryUsage.OTEL),
		instrument.WithUnit(attributes.ProcessMemoryUsage.Unit),
		instrument.WithDescription("Memory usage of the process"),
	); err != nil {
		return err
	}
	if metrics.memoryVirtual, err = meter.Int64UpDownCounter(string(attributes.ProcessMemoryVirtual.OTEL),
		instrument.WithUnit(attributes.ProcessMemoryVirtual.Unit),
		instrument.WithDescription("Virtual memory size of the process"),
	); err != nil {
		return err
	}
	if metrics.diskIO, err = meter.Int64Counter(string(attributes.ProcessDiskIO.OTEL),
		instrument.WithUnit(attributes.ProcessDiskIO.Unit),
		instrument.WithDescription("Disk IO bytes of the process"),
	); err != nil {
		return err
	}
	return nil
}

func (r *ProcessMetricsReporter) reportMetrics(ctx context.Context) {
	defer r.close()

	for {
		select {
		case <-ctx.Done():
			r.log.Debug("context done, stopping process metrics reporting")
			return
		case snapshots, ok := <-r.input:
			if !ok {
				r.log.Debug("process metrics input channel closed, stopping reporting")
				return
			}
			r.reportProcessMetrics(snapshots)
		}
	}
}

func (r *ProcessMetricsReporter) reportProcessMetrics(snapshots []procmetrics.Snapshot) {
	for i := range snapshots {
		snapshot := &snapshots[i]
		if !snapshot.Removed && (!snapshot.Service.ExportModes.CanExportMetrics() || !snapshot.Service.Features.Process()) {
			continue
		}
		var metrics *ProcessMetrics
		if snapshot.Removed {
			var ok bool
			metrics, ok = r.reporters.Lookup(snapshot.Service.UID)
			if !ok {
				continue
			}
		} else {
			var err error
			metrics, err = r.reporters.For(&snapshot.Service)
			if err != nil {
				r.log.Debug("creating process metric set failed", "pid", snapshot.PID, "error", err)
				continue
			}
		}
		recordProcessMetrics(r.ctx, metrics, snapshot)
		if snapshot.Removed && len(metrics.values) == 0 {
			r.reporters.Remove(snapshot.Service.UID)
		}
	}
}

func recordProcessMetrics(ctx context.Context, metrics *ProcessMetrics, snapshot *procmetrics.Snapshot) {
	if metrics == nil {
		return
	}

	metrics.mu.Lock()
	defer metrics.mu.Unlock()

	cpuUser := metrics.attributes(snapshot, attributes.ProcessCPUTime, attribute.String(string(attr.CPUMode), procmetrics.CPUModeUser))
	cpuSystem := metrics.attributes(snapshot, attributes.ProcessCPUTime, attribute.String(string(attr.CPUMode), procmetrics.CPUModeSystem))
	utilUser := metrics.attributes(snapshot, attributes.ProcessCPUUtilization, attribute.String(string(attr.CPUMode), procmetrics.CPUModeUser))
	utilSystem := metrics.attributes(snapshot, attributes.ProcessCPUUtilization, attribute.String(string(attr.CPUMode), procmetrics.CPUModeSystem))
	memory := metrics.attributes(snapshot, attributes.ProcessMemoryUsage)
	virtual := metrics.attributes(snapshot, attributes.ProcessMemoryVirtual)
	read := metrics.attributes(snapshot, attributes.ProcessDiskIO, attribute.String(string(attr.DiskIODirection), procmetrics.DiskDirectionRead))
	write := metrics.attributes(snapshot, attributes.ProcessDiskIO, attribute.String(string(attr.DiskIODirection), procmetrics.DiskDirectionWrite))

	if snapshot.Removed {
		metrics.cpuTime.Remove(ctx, instrument.WithAttributeSet(cpuUser))
		metrics.cpuTime.Remove(ctx, instrument.WithAttributeSet(cpuSystem))
		metrics.cpuUtilization.Remove(ctx, instrument.WithAttributeSet(utilUser))
		metrics.cpuUtilization.Remove(ctx, instrument.WithAttributeSet(utilSystem))
		metrics.memoryUsage.Remove(ctx, instrument.WithAttributeSet(memory))
		metrics.memoryVirtual.Remove(ctx, instrument.WithAttributeSet(virtual))
		metrics.diskIO.Remove(ctx, instrument.WithAttributeSet(read))
		metrics.diskIO.Remove(ctx, instrument.WithAttributeSet(write))
		delete(metrics.values, snapshot.PID)
		return
	}

	values := metrics.values[snapshot.PID]
	if values == nil {
		values = &processMetricValues{}
		metrics.values[snapshot.PID] = values
	}
	metrics.cpuTime.Add(ctx, snapshot.CPUTimeUserSeconds, instrument.WithAttributeSet(cpuUser))
	metrics.cpuTime.Add(ctx, snapshot.CPUTimeSystemSeconds, instrument.WithAttributeSet(cpuSystem))
	metrics.cpuUtilization.Record(ctx, snapshot.CPUUtilizationUser, instrument.WithAttributeSet(utilUser))
	metrics.cpuUtilization.Record(ctx, snapshot.CPUUtilizationSystem, instrument.WithAttributeSet(utilSystem))
	metrics.memoryUsage.Add(ctx, snapshot.MemoryUsageBytes-values.lastMemoryUsage, instrument.WithAttributeSet(memory))
	metrics.memoryVirtual.Add(ctx, snapshot.MemoryVirtualBytes-values.lastMemoryVirtual, instrument.WithAttributeSet(virtual))
	metrics.diskIO.Add(ctx, int64(snapshot.DiskReadBytes), instrument.WithAttributeSet(read))
	metrics.diskIO.Add(ctx, int64(snapshot.DiskWriteBytes), instrument.WithAttributeSet(write))
	values.lastMemoryUsage = snapshot.MemoryUsageBytes
	values.lastMemoryVirtual = snapshot.MemoryVirtualBytes
}

func (m *ProcessMetrics) attributes(snapshot *procmetrics.Snapshot, name attributes.Name, dimensions ...attribute.KeyValue) attribute.Set {
	values := []attribute.KeyValue{attribute.Int64(string(attr.ProcessPID), int64(snapshot.PID))}
	for _, field := range m.attrs[name] {
		if value := field.Get(snapshot); value.Valid() {
			values = append(values, value)
		}
	}
	return attribute.NewSet(append(values, dimensions...)...)
}

func (r *ProcessMetricsReporter) close() {
	r.reporters.ForEach(func(_ svc.UID, metrics *ProcessMetrics) {
		if err := metrics.provider.Shutdown(context.WithoutCancel(r.ctx)); err != nil {
			r.log.Warn("closing process metrics provider", "error", err)
		}
	})
	go func() {
		if err := r.exporter.Shutdown(r.ctx); err != nil {
			r.log.Warn("closing process metrics exporter", "error", err)
			return
		}
		r.log.Debug("process metrics reporter closed")
	}()
}
