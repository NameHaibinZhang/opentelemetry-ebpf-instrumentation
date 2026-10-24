// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package otelcfg // import "go.opentelemetry.io/obi/pkg/export/otel/otelcfg"

import (
	"context"
	"log/slog"
	"net/url"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// loggingMetricsExporter logs the outcome of every OTLP metrics export.
type loggingMetricsExporter struct {
	sdkmetric.Exporter
	log *slog.Logger
}

func newLoggingMetricsExporter(exp sdkmetric.Exporter, protocol Protocol, endpoint string) *loggingMetricsExporter {
	return &loggingMetricsExporter{
		Exporter: exp,
		log: slog.With("component", "otelcfg.MetricsExporter",
			"protocol", protocol, "endpoint", redactEndpoint(endpoint)),
	}
}

func (e *loggingMetricsExporter) Export(ctx context.Context, rm *metricdata.ResourceMetrics) error {
	started := time.Now()
	err := e.Exporter.Export(ctx, rm)
	duration := time.Since(started).Round(time.Millisecond)

	metrics, dataPoints := countMetrics(rm)
	if err != nil {
		e.log.Error("failed to send metrics",
			"metrics", metrics, "dataPoints", dataPoints, "duration", duration, "error", err)
		return err
	}
	e.log.Info("metrics sent",
		"metrics", metrics, "dataPoints", dataPoints, "duration", duration)
	if e.log.Enabled(ctx, slog.LevelDebug) {
		e.log.Debug("sent metric names", "names", metricNames(rm))
	}
	return nil
}

func countMetrics(rm *metricdata.ResourceMetrics) (metrics, dataPoints int) {
	for _, scope := range rm.ScopeMetrics {
		metrics += len(scope.Metrics)
		for _, m := range scope.Metrics {
			dataPoints += countDataPoints(m.Data)
		}
	}
	return metrics, dataPoints
}

func countDataPoints(data metricdata.Aggregation) int {
	switch d := data.(type) {
	case metricdata.Gauge[int64]:
		return len(d.DataPoints)
	case metricdata.Gauge[float64]:
		return len(d.DataPoints)
	case metricdata.Sum[int64]:
		return len(d.DataPoints)
	case metricdata.Sum[float64]:
		return len(d.DataPoints)
	case metricdata.Histogram[int64]:
		return len(d.DataPoints)
	case metricdata.Histogram[float64]:
		return len(d.DataPoints)
	case metricdata.ExponentialHistogram[int64]:
		return len(d.DataPoints)
	case metricdata.ExponentialHistogram[float64]:
		return len(d.DataPoints)
	case metricdata.Summary:
		return len(d.DataPoints)
	default:
		return 0
	}
}

func metricNames(rm *metricdata.ResourceMetrics) []string {
	var names []string
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			names = append(names, m.Name)
		}
	}
	return names
}

// redactEndpoint hides any credentials embedded in the endpoint URL.
func redactEndpoint(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil {
		return endpoint
	}
	return u.Redacted()
}
