// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package obi // import "go.opentelemetry.io/obi/pkg/obi"

import (
	"time"

	"go.opentelemetry.io/obi/pkg/config"
)

// GPUConfig configures the node-level GPU device metrics monitor (pkg/gpumon).
type GPUConfig struct {
	// Mode enables or disables GPU device metrics collection:
	// "auto" (default) starts the monitor only when NVIDIA GPUs are detected,
	// "on" always starts it, "off" never does.
	Mode config.GPUMonitorMode `yaml:"mode" env:"OTEL_EBPF_GPU_MODE" jsonschema:"type=string,enum=on,enum=off,enum=auto"`
	// Interval between GPU metric collection cycles. Defaults to 30s.
	Interval time.Duration `yaml:"interval" env:"OTEL_EBPF_GPU_INTERVAL"`
	// Prometheus allows exposing the GPU device metrics through a standalone
	// Prometheus HTTP endpoint, in addition to the configured metrics exporter.
	Prometheus GPUPrometheusConfig `yaml:"prometheus"`
}

// GPUPrometheusConfig configures the standalone Prometheus endpoint for the
// GPU device metrics.
type GPUPrometheusConfig struct {
	// Port where the Prometheus endpoint listens. 0 (default) means disabled.
	Port int `yaml:"port" env:"OTEL_EBPF_GPU_PROMETHEUS_PORT" validate:"gte=0,lte=65535"`
	// Path where the Prometheus endpoint serves metrics. Defaults to /metrics.
	Path string `yaml:"path" env:"OTEL_EBPF_GPU_PROMETHEUS_PATH" validate:"omitempty,startswith=/"`
}

var DefaultGPUConfig = GPUConfig{
	Mode:     config.GPUMonitorModeAuto,
	Interval: 30 * time.Second,
	Prometheus: GPUPrometheusConfig{
		Path: "/metrics",
	},
}
