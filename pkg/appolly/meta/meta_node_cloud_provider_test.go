// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package meta

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pointDMISysVendorTo replaces the DMI sys_vendor file for the duration of the test.
func pointDMISysVendorTo(t *testing.T, sysVendor string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sys_vendor")
	require.NoError(t, os.WriteFile(path, []byte(sysVendor), 0o644))
	previousPath := dmiSysVendorPath
	dmiSysVendorPath = path
	t.Cleanup(func() {
		dmiSysVendorPath = previousPath
	})
}

func TestDetectCloudProvider(t *testing.T) {
	tests := []struct {
		name      string
		sysVendor string
		expected  cloudProvider
	}{
		{name: "alibaba cloud", sysVendor: "Alibaba Cloud\n", expected: cloudProviderAlibaba},
		{name: "aws", sysVendor: "Amazon EC2\n", expected: cloudProviderAWS},
		{name: "azure", sysVendor: "Microsoft Corporation\n", expected: cloudProviderAzure},
		{name: "gcp", sysVendor: "Google\n", expected: cloudProviderGCP},
		{name: "surrounding blanks", sysVendor: "  Alibaba Cloud  \n", expected: cloudProviderAlibaba},
		{name: "unknown vendor", sysVendor: "Dell Inc.\n", expected: cloudProviderUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pointDMISysVendorTo(t, tt.sysVendor)
			assert.Equal(t, tt.expected, detectCloudProvider())
		})
	}
}

func TestDetectCloudProvider_NoDMI(t *testing.T) {
	previousPath := dmiSysVendorPath
	dmiSysVendorPath = filepath.Join(t.TempDir(), "missing")
	t.Cleanup(func() {
		dmiSysVendorPath = previousPath
	})
	assert.Equal(t, cloudProviderUnknown, detectCloudProvider())
}

func TestCloudNodeFetchers(t *testing.T) {
	tests := []struct {
		name      string
		sysVendor string
		expected  int
	}{
		{name: "alibaba cloud", sysVendor: "Alibaba Cloud\n", expected: 1},
		{name: "aws", sysVendor: "Amazon EC2\n", expected: 1},
		{name: "azure", sysVendor: "Microsoft Corporation\n", expected: 1},
		{name: "gcp", sysVendor: "Google\n", expected: 1},
		// unknown provider keeps the unconditioned behavior
		{name: "unknown vendor", sysVendor: "Dell Inc.\n", expected: 4},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pointDMISysVendorTo(t, tt.sysVendor)
			assert.Len(t, cloudNodeFetchers(), tt.expected)
		})
	}
}

func TestCloudNodeFetchers_AlibabaCloud(t *testing.T) {
	srv := httptest.NewServer(alibabaIMDSMockHandler(t, alibabaIMDSMockValues))
	defer srv.Close()
	pointAlibabaIMDSTo(t, srv.URL+alibabaIMDSMockBasePath)
	pointDMISysVendorTo(t, "Alibaba Cloud\n")

	fetchers := cloudNodeFetchers()
	require.Len(t, fetchers, 1)

	meta, err := fetchers[0](t.Context())
	require.NoError(t, err)
	assert.Equal(t, "i-2ze88pl0kljl42nbq6kd", meta.HostID)
	assert.Contains(t, meta.Metadata, Entry{Key: "cloud.provider", Value: "alibaba_cloud"})
}

func TestCloudNodeFetchers_UnknownProviderRunsOthers(t *testing.T) {
	// when the provider is unknown, the Alibaba fetcher is included among the
	// fallback ones: point it to an unreachable endpoint so the test does not
	// depend on the environment
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	pointAlibabaIMDSTo(t, srv.URL+alibabaIMDSMockBasePath)
	pointDMISysVendorTo(t, "Dell Inc.\n")

	fetchers := cloudNodeFetchers()
	require.Len(t, fetchers, 4)

	meta, err := fetchers[3](t.Context())
	require.NoError(t, err)
	assert.Equal(t, NodeMeta{}, meta)
}
