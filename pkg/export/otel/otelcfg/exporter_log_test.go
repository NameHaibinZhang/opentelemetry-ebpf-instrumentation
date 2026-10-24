// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package otelcfg

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestCountMetrics(t *testing.T) {
	rm := &metricdata.ResourceMetrics{ScopeMetrics: []metricdata.ScopeMetrics{{
		Metrics: []metricdata.Metrics{
			{Name: "a", Data: metricdata.Gauge[int64]{DataPoints: make([]metricdata.DataPoint[int64], 2)}},
			{Name: "b", Data: metricdata.Sum[float64]{DataPoints: make([]metricdata.DataPoint[float64], 3)}},
			{Name: "c", Data: metricdata.Histogram[float64]{DataPoints: make([]metricdata.HistogramDataPoint[float64], 1)}},
		},
	}}}

	metrics, dataPoints := countMetrics(rm)
	assert.Equal(t, 3, metrics)
	assert.Equal(t, 6, dataPoints)
	assert.Equal(t, []string{"a", "b", "c"}, metricNames(rm))
}

func TestRedactEndpoint(t *testing.T) {
	assert.Equal(t, "http://user:xxxxx@collector:4318", redactEndpoint("http://user:secret@collector:4318"))
	assert.Equal(t, "http://collector:4318", redactEndpoint("http://collector:4318"))
}
