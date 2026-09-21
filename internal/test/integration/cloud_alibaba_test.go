// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package integration

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.opentelemetry.io/obi/internal/test/integration/components/jaeger"
	"go.opentelemetry.io/obi/internal/test/integration/components/promtest"
	ti "go.opentelemetry.io/obi/pkg/test/integration"
)

// This file contains tests related with the integration with Alibaba Cloud
func TestCloudResourceMetadata_Alibaba(t *testing.T) {
	network := setupIMDSSubnet(t, "100.100.100.0/24")
	setupMockAlibabaIMDS(t, network)
	setupContainerPrometheus(t, network, "prometheus-config-perapp.yml")
	setupContainerJaeger(t, network)
	setupContainerWeaver(t, network)
	setupContainerCollector(t, network, "otelcol-config-weaver.yml")
	setupGoOTelTestServer(t, network, nil)

	if t.Failed() {
		return
	}

	// Start OBI to instrument the test server.
	// No endpoint override is needed: the Alibaba Cloud IMDS is queried at its
	// well-known address, which the mock network provides at 100.100.100.200
	o := obi{
		Env: []string{
			`OTEL_EBPF_PROMETHEUS_PORT=8999`,
			"OTEL_EBPF_OPEN_PORT=8080",
		},
		Logs: createLogOutput(t, "cloud-meta-alibaba"),
	}
	if !KernelLockdownMode() {
		o.SecurityConfigSuffix = "_none"
	}
	o.instrument(t, network, "obi-config.yml")

	// Wait for test components to be ready
	waitForTestComponents(t, "http://localhost:8080")

	// Make some requests to generate metrics
	for range 4 {
		ti.DoHTTPGet(t, "http://localhost:8080/rolldice", 200)
	}

	// Query Prometheus for target_info with cloud metadata attributes
	pq := promtest.Client{HostPort: prometheusHostPort}

	t.Run("OTEL metrics", func(t *testing.T) {
		testAlibabaMetrics(t, pq, "rolldice", "otel")
	})
	t.Run("Prometheus metrics", func(t *testing.T) {
		testAlibabaMetrics(t, pq, "rolldice", "prometheus")
	})
	t.Run("OTEL traces", func(t *testing.T) {
		testAlibabaTraces(t)
	})

	runWeaverValidation(t)
}

func testAlibabaMetrics(t *testing.T, pq promtest.Client, serviceName, exporter string) {
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		// attribute values taken from alibaba-imds/nginx.conf mock
		query := `target_info{` +
			`service_name="` + serviceName + `",` +
			`exported="` + exporter + `",` +
			`cloud_account_id="1234567890123456",` +
			`cloud_availability_zone="cn-shanghai-e",` +
			`cloud_platform="alibaba_cloud_ecs",` +
			`cloud_provider="alibaba_cloud",` +
			`cloud_region="cn-shanghai",` +
			`host_id="i-2ze88pl0kljl42nbq6kd",` +
			`host_image_id="aliyun_2_1903_x64_20G_alibase_20240124.vhd",` +
			`host_type="ecs.g7.large"` +
			`}`
		results, err := pq.Query(query)
		require.NoError(ct, err, "failed to query metrics")
		assert.NotEmpty(ct, results, "target_info with cloud metadata should exist")
	}, testTimeout, 500*time.Millisecond)
}

func testAlibabaTraces(t *testing.T) {
	var trace jaeger.Trace
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		resp, err := http.Get(jaegerQueryURL + "?service=rolldice&operation=GET%20%2Frolldice")
		require.NoError(ct, err)
		if resp == nil {
			return
		}
		require.Equal(ct, http.StatusOK, resp.StatusCode)
		var tq jaeger.TracesQuery
		require.NoError(ct, json.NewDecoder(resp.Body).Decode(&tq))
		traces := tq.FindBySpan(jaeger.Tag{Key: "url.path", Type: "string", Value: "/rolldice"})
		require.NotEmpty(ct, traces)
		trace = traces[0]
		require.Len(ct, trace.Spans, 3) // parent - in queue - processing
	}, testTimeout, 100*time.Millisecond)

	for _, proc := range trace.Processes {
		sd := jaeger.DiffAsRegexp([]jaeger.Tag{
			{Key: "cloud.account.id", Type: "string", Value: "^1234567890123456$"},
			{Key: "cloud.availability_zone", Type: "string", Value: "^cn-shanghai-e$"},
			{Key: "cloud.platform", Type: "string", Value: "^alibaba_cloud_ecs$"},
			{Key: "cloud.provider", Type: "string", Value: "^alibaba_cloud$"},
			{Key: "cloud.region", Type: "string", Value: "^cn-shanghai$"},
			{Key: "host.id", Type: "string", Value: "^i-2ze88pl0kljl42nbq6kd$"},
			{Key: "host.image.id", Type: "string", Value: "^aliyun_2_1903_x64_20G_alibase_20240124.vhd$"},
			{Key: "host.type", Type: "string", Value: "^ecs.g7.large$"},
		}, proc.Tags)
		require.Empty(t, sd)
	}
}
