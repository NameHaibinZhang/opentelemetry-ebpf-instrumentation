// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package instrumenter

import (
	"context"
	"log/slog"
	"os"
	"time"

	"go.opentelemetry.io/obi/pkg/export/connector"
	"go.opentelemetry.io/obi/pkg/internal/gpumon"
)

// gpuNodePollInterval is the sampling period of the node-level GPU inventory
// (procfs, sysfs and nvidia-smi).
const gpuNodePollInterval = 15 * time.Second

// startGPUNodeMetrics registers the node-level GPU collector on the shared
// Prometheus endpoint and starts the polling agent. It is a no-op when GPU
// monitoring is disabled by environment.
func startGPUNodeMetrics(ctx context.Context, pm *connector.PrometheusManager, port int, path string) {
	if gpumon.DisabledByEnv() {
		slog.Info("GPU node metrics disabled by environment")
		return
	}

	reporter := gpumon.NewPromReporter(gpumon.ParseExtraLabels(os.Getenv(gpumon.EnvResourceAttributes)))
	pm.Register(port, path, reporter)
	go pm.StartHTTP(ctx)

	store := gpumon.NewPodStore()
	if store != nil {
		go store.Start(ctx)
	}

	agent := gpumon.NewAgent(gpuNodePollInterval, gpumon.NewSources(), gpumon.NewK8sResolver(store), reporter)
	go func() {
		if err := agent.Run(ctx); err != nil {
			slog.Error("GPU node metrics collection failed", "error", err)
		}
	}()
	slog.Info("GPU node metrics collection started", "interval", gpuNodePollInterval)
}
