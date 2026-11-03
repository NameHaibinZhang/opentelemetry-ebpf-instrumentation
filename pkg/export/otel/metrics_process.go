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
// procmetrics collector samples from /proc, following the Beyla/OpenTelemetry
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
	networkIO      instrument.Int64Counter

	mu     sync.Mutex
	values map[app.PID]*processMetricValues
}

// processMetricValues tracks the last recorded memory contributions per PID so
// that the cumulative memory UpDownCounters can be compensated when a process
// disappears.
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
	}

	meter := provider.Meter(reporterName, obiMeterOptions()...)

	var err error
	if metrics.cpuTime, err = meter.Float64Counter(string(attributes.ProcessCPUTime.OTEL),
		instrument.WithUnit(attributes.ProcessCPUTime.Unit),
		instrument.WithDescription("CPU time of the process"),
	); err != nil {
		return nil, err
	}
	if metrics.cpuUtilization, err = meter.Float64Gauge(string(attributes.ProcessCPUUtilization.OTEL),
		instrument.WithUnit(attributes.ProcessCPUUtilization.Unit),
		instrument.WithDescription("CPU utilization of the process"),
	); err != nil {
		return nil, err
	}
	if metrics.memoryUsage, err = meter.Int64UpDownCounter(string(attributes.ProcessMemoryUsage.OTEL),
		instrument.WithUnit(attributes.ProcessMemoryUsage.Unit),
		instrument.WithDescription("Memory usage of the process"),
	); err != nil {
		return nil, err
	}
	if metrics.memoryVirtual, err = meter.Int64UpDownCounter(string(attributes.ProcessMemoryVirtual.OTEL),
		instrument.WithUnit(attributes.ProcessMemoryVirtual.Unit),
		instrument.WithDescription("Virtual memory size of the process"),
	); err != nil {
		return nil, err
	}
	if metrics.diskIO, err = meter.Int64Counter(string(attributes.ProcessDiskIO.OTEL),
		instrument.WithUnit(attributes.ProcessDiskIO.Unit),
		instrument.WithDescription("Disk IO bytes of the process"),
	); err != nil {
		return nil, err
	}
	if metrics.networkIO, err = meter.Int64Counter(string(attributes.ProcessNetworkIO.OTEL),
		instrument.WithUnit(attributes.ProcessNetworkIO.Unit),
		instrument.WithDescription("Network IO bytes of the process"),
	); err != nil {
		return nil, err
	}

	return metrics, nil
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
		if !snapshot.Service.ExportModes.CanExportMetrics() {
			continue
		}
		metrics, err := r.reporters.For(&snapshot.Service)
		if err != nil {
			r.log.Debug("creating process metric set failed", "pid", snapshot.PID, "error", err)
			continue
		}
		recordProcessMetrics(r.ctx, metrics, snapshot)
	}
}

func recordProcessMetrics(ctx context.Context, metrics *ProcessMetrics, snapshot *procmetrics.Snapshot) {
	if metrics == nil {
		return
	}

	metrics.mu.Lock()
	defer metrics.mu.Unlock()

	pidAttr := attribute.Int64(string(attr.ProcessPID), int64(snapshot.PID))

	if snapshot.Removed {
		// Compensate the cumulative memory UpDownCounters and zero the gauge so
		// the series does not keep stale values for a dead process.
		if values, ok := metrics.values[snapshot.PID]; ok {
			if values.lastMemoryUsage != 0 {
				metrics.memoryUsage.Add(ctx, -values.lastMemoryUsage, instrument.WithAttributes(pidAttr))
			}
			if values.lastMemoryVirtual != 0 {
				metrics.memoryVirtual.Add(ctx, -values.lastMemoryVirtual, instrument.WithAttributes(pidAttr))
			}
			delete(metrics.values, snapshot.PID)
		}
		metrics.cpuUtilization.Record(ctx, 0, instrument.WithAttributes(pidAttr))
		return
	}

	userAttr := instrument.WithAttributes(pidAttr, attribute.String(string(attr.CPUMode), procmetrics.CPUModeUser))
	systemAttr := instrument.WithAttributes(pidAttr, attribute.String(string(attr.CPUMode), procmetrics.CPUModeSystem))

	metrics.cpuTime.Add(ctx, snapshot.CPUTimeUserSeconds, userAttr)
	metrics.cpuTime.Add(ctx, snapshot.CPUTimeSystemSeconds, systemAttr)

	metrics.cpuUtilization.Record(ctx, snapshot.CPUUtilizationUser, userAttr)
	metrics.cpuUtilization.Record(ctx, snapshot.CPUUtilizationSystem, systemAttr)

	metrics.memoryUsage.Add(ctx, snapshot.MemoryUsageBytes, instrument.WithAttributes(pidAttr))
	metrics.memoryVirtual.Add(ctx, snapshot.MemoryVirtualBytes, instrument.WithAttributes(pidAttr))

	readAttr := instrument.WithAttributes(pidAttr, attribute.String(string(attr.DiskIODirection), procmetrics.DiskDirectionRead))
	writeAttr := instrument.WithAttributes(pidAttr, attribute.String(string(attr.DiskIODirection), procmetrics.DiskDirectionWrite))
	metrics.diskIO.Add(ctx, int64(snapshot.DiskReadBytes), readAttr)
	metrics.diskIO.Add(ctx, int64(snapshot.DiskWriteBytes), writeAttr)

	receiveAttr := instrument.WithAttributes(pidAttr, attribute.String(string(attr.NetworkIoDirection), procmetrics.NetworkDirectionReceive))
	transmitAttr := instrument.WithAttributes(pidAttr, attribute.String(string(attr.NetworkIoDirection), procmetrics.NetworkDirectionTransmit))
	metrics.networkIO.Add(ctx, int64(snapshot.NetworkReceiveBytes), receiveAttr)
	metrics.networkIO.Add(ctx, int64(snapshot.NetworkTransmitBytes), transmitAttr)

	values := metrics.values[snapshot.PID]
	if values == nil {
		values = &processMetricValues{}
		metrics.values[snapshot.PID] = values
	}
	values.lastMemoryUsage = snapshot.MemoryUsageBytes
	values.lastMemoryVirtual = snapshot.MemoryVirtualBytes
}

func (r *ProcessMetricsReporter) close() {
	go func() {
		if err := r.exporter.Shutdown(r.ctx); err != nil {
			r.log.Warn("closing process metrics exporter", "error", err)
			return
		}
		r.log.Debug("process metrics reporter closed")
	}()
}
