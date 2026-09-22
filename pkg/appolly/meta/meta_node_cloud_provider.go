// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package meta // import "go.opentelemetry.io/obi/pkg/appolly/meta"

import (
	"os"
	"strings"

	"go.opentelemetry.io/contrib/detectors/aws/ec2/v2"
	"go.opentelemetry.io/contrib/detectors/azure/azurevm"
	"go.opentelemetry.io/contrib/detectors/gcp"
)

// overridable in tests
var dmiSysVendorPath = "/sys/class/dmi/id/sys_vendor"

type cloudProvider string

const (
	cloudProviderUnknown cloudProvider = ""
	cloudProviderAlibaba cloudProvider = "alibaba_cloud"
	cloudProviderAWS     cloudProvider = "aws"
	cloudProviderAzure   cloudProvider = "azure"
	cloudProviderGCP     cloudProvider = "gcp"
)

// detectCloudProvider identifies the cloud provider from the SMBIOS system
// vendor exposed by the kernel, to avoid querying metadata endpoints of other
// clouds, whose unreachability would only delay the OBI startup.
func detectCloudProvider() cloudProvider {
	sysVendor, err := os.ReadFile(dmiSysVendorPath)
	if err != nil {
		return cloudProviderUnknown
	}
	vendor := strings.TrimSpace(string(sysVendor))
	switch {
	case strings.Contains(vendor, "Alibaba"):
		return cloudProviderAlibaba
	case strings.Contains(vendor, "Amazon"):
		return cloudProviderAWS
	case strings.Contains(vendor, "Microsoft"):
		return cloudProviderAzure
	case strings.Contains(vendor, "Google"):
		return cloudProviderGCP
	default:
		return cloudProviderUnknown
	}
}

// cloudNodeFetchers returns the fetchers for the detected cloud provider only.
// When the provider can't be identified (bare metal, non-Linux, unreadable DMI),
// it returns all the fetchers so OBI keeps working everywhere.
func cloudNodeFetchers() []fetcher {
	switch detectCloudProvider() {
	case cloudProviderAlibaba:
		return []fetcher{alibabaCloudNodeFetcher()}
	case cloudProviderAWS:
		return []fetcher{otelNodeFetcher(ec2.NewResourceDetector())}
	case cloudProviderAzure:
		return []fetcher{otelNodeFetcher(azurevm.NewResourceDetector(
			azurevm.WithAttributeFilter(azureVMAttributeFilter),
		))}
	case cloudProviderGCP:
		return []fetcher{otelNodeFetcher(gcp.NewDetector())}
	default:
		// the later the fetcher, the highest its priority
		return []fetcher{
			otelNodeFetcher(azurevm.NewResourceDetector(
				azurevm.WithAttributeFilter(azureVMAttributeFilter),
			)),
			otelNodeFetcher(gcp.NewDetector()),
			otelNodeFetcher(ec2.NewResourceDetector()),
			alibabaCloudNodeFetcher(),
		}
	}
}
