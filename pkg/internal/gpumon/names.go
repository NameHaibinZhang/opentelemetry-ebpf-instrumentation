// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package gpumon

// Metric and label names as emitted on the Prometheus endpoint. Metric names
// carry the gpu_ prefix (derived from the OBI obi.gpu.* conventions), while
// label names are identical to OBI's, so OBI dashboards keep working when the
// metrics come from this standalone exporter instead of OBI itself.
const (
	metricGPUInfo              = "gpu_info"
	metricGPUUp                = "gpu_up"
	metricGPUNumaNode          = "gpu_numa_node"
	metricGPUPcieGeneration    = "gpu_pcie_generation"
	metricGPUPcieWidth         = "gpu_pcie_width"
	metricGPUOccupancyConflict = "gpu_occupancy_conflict"
	metricGPUProcessInfo       = "gpu_process_info"
	metricGPUNvlinkUp          = "gpu_nvlink_up"
	metricGPUNvlinkRate        = "gpu_nvlink_rate"
	metricGPUNvlinkWidth       = "gpu_nvlink_width"
	metricGPUPcieAerErrors     = "gpu_pcie_aer_errors"

	labelGPUUUID            = "gpu_uuid"
	labelGPUPciBusID        = "gpu_pci_bus_id"
	labelHWModel            = "hw_model"
	labelGPUMigCapable      = "gpu_mig_capable"
	labelGPUNvlinkLinkID    = "gpu_nvlink_link_id"
	labelGPUPeerUUID        = "gpu_peer_uuid"
	labelGPUNvlinkSwitchID  = "gpu_nvlink_switch_id"
	labelGPUPcieAerSeverity = "gpu_pcie_aer_severity"
	labelMLRank             = "ml_rank"
	labelProcessPID         = "process_pid"
	labelProcessExecutable  = "process_executable_name"
	labelK8sPodName         = "k8s_pod_name"
	labelK8sJobName         = "k8s_job_name"
)
