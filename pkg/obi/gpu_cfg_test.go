// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package obi

import (
	"bytes"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.opentelemetry.io/obi/pkg/config"
)

func TestConfig_GPUDefaults(t *testing.T) {
	cfg, err := LoadConfig(nil)
	require.NoError(t, err)

	assert.Equal(t, config.GPUMonitorModeAuto, cfg.GPU.Mode)
	assert.Equal(t, 30*time.Second, cfg.GPU.Interval)
	assert.Equal(t, 0, cfg.GPU.Prometheus.Port)
	assert.Equal(t, "/metrics", cfg.GPU.Prometheus.Path)
}

func TestConfig_GPUFromYAML(t *testing.T) {
	cfg, err := LoadConfig(bytes.NewBufferString(`
gpu:
  mode: on
  interval: 5s
  prometheus:
    port: 3666
    path: /gpu-metrics
`))
	require.NoError(t, err)

	assert.Equal(t, config.GPUMonitorModeOn, cfg.GPU.Mode)
	assert.Equal(t, 5*time.Second, cfg.GPU.Interval)
	assert.Equal(t, 3666, cfg.GPU.Prometheus.Port)
	assert.Equal(t, "/gpu-metrics", cfg.GPU.Prometheus.Path)
}

func TestConfig_GPUFromEnv(t *testing.T) {
	t.Setenv("OTEL_EBPF_GPU_MODE", "off")
	t.Setenv("OTEL_EBPF_GPU_INTERVAL", "15s")
	t.Setenv("OTEL_EBPF_GPU_PROMETHEUS_PORT", "3667")
	t.Setenv("OTEL_EBPF_GPU_PROMETHEUS_PATH", "/gpu-env")

	cfg, err := LoadConfig(nil)
	require.NoError(t, err)

	assert.Equal(t, config.GPUMonitorModeOff, cfg.GPU.Mode)
	assert.Equal(t, 15*time.Second, cfg.GPU.Interval)
	assert.Equal(t, 3667, cfg.GPU.Prometheus.Port)
	assert.Equal(t, "/gpu-env", cfg.GPU.Prometheus.Path)
}

func TestConfigValidate_GPUInterval(t *testing.T) {
	for _, interval := range []string{"0s", "-1s"} {
		t.Run(interval, func(t *testing.T) {
			cfg, err := LoadConfig(bytes.NewBufferString("gpu:\n  interval: " + interval + "\n"))
			require.NoError(t, err)
			require.ErrorContains(t, cfg.Validate(), "gpu.interval must be greater than 0")
		})
	}
}

func TestConfig_GPUInvalidModeFailsLoading(t *testing.T) {
	_, err := LoadConfig(bytes.NewBufferString("gpu:\n  mode: always\n"))
	require.ErrorContains(t, err, "invalid GPU monitoring mode value")
}
