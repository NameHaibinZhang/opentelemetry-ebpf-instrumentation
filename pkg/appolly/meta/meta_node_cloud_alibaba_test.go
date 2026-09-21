// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package meta

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"

	attr "go.opentelemetry.io/obi/pkg/export/attributes/names"
)

const (
	alibabaIMDSMockBasePath = "/latest"
	alibabaIMDSMockToken    = "mock-imds-token"
)

var alibabaIMDSMockValues = map[string]string{
	"/meta-data/owner-account-id":       "1234567890123456",
	"/meta-data/region-id":              "cn-hangzhou",
	"/meta-data/zone-id":                "cn-hangzhou-b",
	"/meta-data/image-id":               "aliyun_3_x64_20G_alibase_20250101.vhd",
	"/meta-data/instance/instance-type": "ecs.g7.large",
	"/meta-data/instance-id":            "i-2ze88pl0kljl42nbq6kd",
}

// pointAlibabaIMDSTo overrides the IMDS base URL and the connection timeout
// for the duration of the test.
func pointAlibabaIMDSTo(t *testing.T, baseURL string) {
	t.Helper()
	previousBaseURL := alibabaIMDSBaseURL
	previousTimeout := connectionTimeout
	alibabaIMDSBaseURL = baseURL
	connectionTimeout = 5 * time.Second
	t.Cleanup(func() {
		alibabaIMDSBaseURL = previousBaseURL
		connectionTimeout = previousTimeout
	})
}

func alibabaIMDSMockHandler(t *testing.T, responses map[string]string) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, alibabaIMDSMockBasePath)
		if r.Method == http.MethodPut && path == alibabaIMDSTokenPath {
			assert.Equal(t, alibabaIMDSTokenTTL, r.Header.Get(alibabaIMDSTokenTTLHeader))
			fmt.Fprintln(w, alibabaIMDSMockToken)
			return
		}
		assert.Equal(t, alibabaIMDSMockToken, r.Header.Get(alibabaIMDSTokenHeader), path)
		value, ok := responses[path]
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		fmt.Fprintln(w, value)
	})
}

func TestAlibabaCloudNodeFetcher(t *testing.T) {
	srv := httptest.NewServer(alibabaIMDSMockHandler(t, alibabaIMDSMockValues))
	defer srv.Close()
	pointAlibabaIMDSTo(t, srv.URL+alibabaIMDSMockBasePath)

	meta, err := alibabaCloudNodeFetcher()(t.Context())
	require.NoError(t, err)
	require.Equal(t, NodeMeta{
		HostID: "i-2ze88pl0kljl42nbq6kd",
		Metadata: []Entry{
			{Key: attr.Name(semconv.CloudProviderKey), Value: "alibaba_cloud"},
			{Key: attr.Name(semconv.CloudPlatformKey), Value: "alibaba_cloud_ecs"},
			{Key: attr.Name(semconv.CloudAccountIDKey), Value: "1234567890123456"},
			{Key: attr.Name(semconv.CloudRegionKey), Value: "cn-hangzhou"},
			{Key: attr.Name(semconv.CloudAvailabilityZoneKey), Value: "cn-hangzhou-b"},
			{Key: attr.Name(semconv.HostImageIDKey), Value: "aliyun_3_x64_20G_alibase_20250101.vhd"},
			{Key: attr.Name(semconv.HostTypeKey), Value: "ecs.g7.large"},
		},
	}, meta)
}

func TestAlibabaCloudNodeFetcher_NotAlibabaCloud(t *testing.T) {
	t.Run("unreachable endpoint", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		srv.Close()
		pointAlibabaIMDSTo(t, srv.URL+alibabaIMDSMockBasePath)

		meta, err := alibabaCloudNodeFetcher()(t.Context())
		require.NoError(t, err)
		assert.Equal(t, NodeMeta{}, meta)
	})

	t.Run("token endpoint not supported", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "not found", http.StatusNotFound)
		}))
		defer srv.Close()
		pointAlibabaIMDSTo(t, srv.URL+alibabaIMDSMockBasePath)

		meta, err := alibabaCloudNodeFetcher()(t.Context())
		require.NoError(t, err)
		assert.Equal(t, NodeMeta{}, meta)
	})

	t.Run("metadata field not available", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPut {
				fmt.Fprintln(w, alibabaIMDSMockToken)
				return
			}
			http.Error(w, "forbidden", http.StatusForbidden)
		}))
		defer srv.Close()
		pointAlibabaIMDSTo(t, srv.URL+alibabaIMDSMockBasePath)

		meta, err := alibabaCloudNodeFetcher()(t.Context())
		require.NoError(t, err)
		assert.Equal(t, NodeMeta{}, meta)
	})
}

func TestAlibabaCloudNodeFetcher_RetriableErrors(t *testing.T) {
	t.Run("token endpoint failure", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "internal error", http.StatusInternalServerError)
		}))
		defer srv.Close()
		pointAlibabaIMDSTo(t, srv.URL+alibabaIMDSMockBasePath)

		meta, err := alibabaCloudNodeFetcher()(t.Context())
		require.Error(t, err)
		require.NotErrorIs(t, err, errNotAlibabaCloud)
		assert.Equal(t, NodeMeta{}, meta)
	})

	t.Run("metadata endpoint failure", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPut {
				fmt.Fprintln(w, alibabaIMDSMockToken)
				return
			}
			http.Error(w, "internal error", http.StatusInternalServerError)
		}))
		defer srv.Close()
		pointAlibabaIMDSTo(t, srv.URL+alibabaIMDSMockBasePath)

		meta, err := alibabaCloudNodeFetcher()(t.Context())
		require.Error(t, err)
		require.NotErrorIs(t, err, errNotAlibabaCloud)
		assert.Equal(t, NodeMeta{}, meta)
	})
}
