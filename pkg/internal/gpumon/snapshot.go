// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

// Package gpumon collects device-level GPU inventory metrics (PCIe, NUMA,
// NVLink, processes, AER errors) from sysfs, /proc and nvidia-smi, and hands
// each cycle's snapshot to the configured reporters.
package gpumon

// Device is a GPU as observed in a single collection cycle. A ghost device
// (the GPU was present in the previous cycle and is gone now) only carries
// UUID and Up=false, so consumers must tolerate empty values for the rest.
type Device struct {
	// UUID includes the "GPU-" prefix, matching the DCGM UUID label.
	UUID string
	// BusID is the PCI bus address in domain:bus:device.function form,
	// e.g. "0000:0b:00.0".
	BusID string
	// Model is the GPU model name, e.g. "NVIDIA A100-SXM4-40GB".
	Model string
	// Up reports whether the device node is accessible.
	Up bool
	// MigCapable reports whether the GPU can be partitioned in MIG instances.
	MigCapable bool
	// NumaNode is the NUMA node the GPU is attached to. Negative when unknown.
	NumaNode int
	// PCIeGeneration is the PCIe generation number (1=2.5GT/s ... 7=128GT/s).
	// Zero when unknown.
	PCIeGeneration int
	// PCIeWidth is the number of PCIe lanes (e.g. 16). Zero when unknown.
	PCIeWidth int
}

// NVLink is a single NVLink of a GPU, connecting it to a peer GPU or to an
// NVSwitch. PeerUUID and SwitchID are mutually exclusive; SwitchID has the
// "NVSwitch:<bus id>" form.
type NVLink struct {
	GPUUUID  string
	LinkID   int
	Active   bool
	PeerUUID string
	SwitchID string
	// Rate is the NVLink generation number derived from the link transfer
	// rate (1 for NVLink1 ... 5 for NVLink5). Zero when unknown.
	Rate int
	// Width is the number of lanes per link. Zero when unknown.
	Width int
}

// ProcessIdentity is the resolved identity of a GPU-using process.
type ProcessIdentity struct {
	// Executable is the process comm name from /proc/<pid>/comm.
	Executable string
	// PodName is the Kubernetes pod owning the process. Empty when the
	// process does not run in a pod the agent can resolve.
	PodName string
	// JobName is the name of the Job owning the pod, when there is one.
	JobName string
	// Rank is the RANK environment variable set by the training launcher
	// (e.g. torchrun). Empty when unset or not a number.
	Rank string
}

// GPUProcess is a process using a GPU through CUDA or MIG.
type GPUProcess struct {
	GPUUUID  string
	PID      int
	Identity ProcessIdentity
}

// AERSample is the cumulative count of PCIe Advanced Error Reporting errors
// for one GPU and severity. Severity values: correctable, nonfatal, fatal.
type AERSample struct {
	GPUUUID  string
	BusID    string
	Severity string
	Total    int64
}

// AERDelta is the number of AER errors accumulated since the previous cycle.
type AERDelta struct {
	GPUUUID  string
	BusID    string
	Severity string
	Count    int64
}

// Snapshot is the result of a single collection cycle.
type Snapshot struct {
	// Devices includes ghost devices (Up=false) for one cycle after a GPU
	// disappears, so reporters can emit the down transition.
	Devices []Device
	NVLinks []NVLink
	// Processes are the resolved GPU-using processes.
	Processes []GPUProcess
	// Conflicts are the processes (with a known pod name) whose GPU is being
	// shared with at least one other distinct workload identity.
	Conflicts []GPUProcess
	AER       []AERDelta
}

// Reporter consumes one snapshot per collection cycle.
type Reporter interface {
	Report(s *Snapshot)
}
