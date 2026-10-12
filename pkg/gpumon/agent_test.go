// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package gpumon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingReporter keeps every snapshot handed to it.
type recordingReporter struct {
	snapshots []*Snapshot
}

func (r *recordingReporter) Report(s *Snapshot) {
	r.snapshots = append(r.snapshots, s)
}

func gpuProc(uuid string, pid int, pod string) GPUProcess {
	return GPUProcess{GPUUUID: uuid, PID: pid, Identity: ProcessIdentity{PodName: pod}}
}

func TestAerDeltas(t *testing.T) {
	agent := NewAgent(0, nil, nil)

	// first observation only records the baseline
	assert.Empty(t, agent.aerDeltas([]AERSample{
		{GPUUUID: "aaa", BusID: "0000:0b:00.0", Severity: "correctable", Total: 10},
	}))

	deltas := agent.aerDeltas([]AERSample{
		{GPUUUID: "aaa", BusID: "0000:0b:00.0", Severity: "correctable", Total: 12},
		{GPUUUID: "aaa", BusID: "0000:0b:00.0", Severity: "nonfatal", Total: 5},
	})
	assert.Equal(t, []AERDelta{
		{GPUUUID: "aaa", BusID: "0000:0b:00.0", Severity: "correctable", Count: 2},
	}, deltas, "nonfatal has no baseline yet, so it emits no delta")

	assert.Empty(t, agent.aerDeltas([]AERSample{
		{GPUUUID: "aaa", BusID: "0000:0b:00.0", Severity: "correctable", Total: 12},
		{GPUUUID: "aaa", BusID: "0000:0b:00.0", Severity: "nonfatal", Total: 5},
	}), "unchanged counters emit no delta")

	deltas = agent.aerDeltas([]AERSample{
		{GPUUUID: "aaa", BusID: "0000:0b:00.0", Severity: "correctable", Total: 3},
	})
	assert.Equal(t, []AERDelta{
		{GPUUUID: "aaa", BusID: "0000:0b:00.0", Severity: "correctable", Count: 3},
	}, deltas, "a counter reset is reported as the new total")
}

func TestDetectConflicts(t *testing.T) {
	processes := []GPUProcess{
		gpuProc("gpu0", 101, "pod-a"),
		gpuProc("gpu0", 102, "pod-b"),
		gpuProc("gpu1", 201, "pod-c"),
		gpuProc("gpu1", 202, "pod-c"),
		gpuProc("gpu2", 301, ""),
		gpuProc("gpu2", 302, ""),
	}

	conflicts := detectConflicts(processes)

	assert.Equal(t, []GPUProcess{
		gpuProc("gpu0", 101, "pod-a"),
		gpuProc("gpu0", 102, "pod-b"),
	}, conflicts)
}

func TestDetectConflictsPodAndBareProcess(t *testing.T) {
	bare := GPUProcess{GPUUUID: "gpu0", PID: 402, Identity: ProcessIdentity{Executable: "python"}}
	processes := []GPUProcess{gpuProc("gpu0", 401, "pod-d"), bare}

	// the bare process counts as a distinct identity, but only the processes
	// with a known pod name are reported, because the metric is labeled by pod
	conflicts := detectConflicts(processes)

	assert.Equal(t, []GPUProcess{gpuProc("gpu0", 401, "pod-d")}, conflicts)
}

func TestDetectConflictsSingleIdentity(t *testing.T) {
	processes := []GPUProcess{
		gpuProc("gpu0", 101, "pod-a"),
		gpuProc("gpu0", 102, "pod-a"),
	}

	assert.Empty(t, detectConflicts(processes))
}

type fakeResolver struct {
	identities map[int]ProcessIdentity
}

func (r fakeResolver) Resolve(pid int) ProcessIdentity {
	return r.identities[pid]
}

func TestResolvedProcesses(t *testing.T) {
	s := newTestSources(t, fakeSMI(t, map[string]string{
		"--query-compute-apps=pid,gpu_uuid --format=csv,noheader": "101, GPU-bbb\n100, GPU-aaa\n",
	}))
	resolver := fakeResolver{identities: map[int]ProcessIdentity{
		100: {Executable: "python", Rank: "0"},
		101: {Executable: "train", PodName: "pod-a", JobName: "job-a", Rank: "1"},
	}}
	agent := NewAgent(time.Hour, s, resolver)

	processes := agent.resolvedProcesses()

	assert.Equal(t, []GPUProcess{
		{GPUUUID: "aaa", PID: 100, Identity: ProcessIdentity{Executable: "python", Rank: "0"}},
		{GPUUUID: "bbb", PID: 101, Identity: ProcessIdentity{Executable: "train", PodName: "pod-a", JobName: "job-a", Rank: "1"}},
	}, processes, "processes must be sorted by GPU UUID, then PID")
}

func TestCollectError(t *testing.T) {
	s := newTestSources(t, fakeSMI(t, nil))
	// a regular file where the GPUs directory belongs makes the listing fail
	writeFile(t, filepath.Join(s.ProcRoot, "driver", "nvidia", "gpus"), "not a directory")

	agent := NewAgent(time.Hour, s, nil)

	_, err := agent.collect()
	require.Error(t, err)
}

func TestCollectNoGPUs(t *testing.T) {
	s := newTestSources(t, fakeSMI(t, nil))

	agent := NewAgent(time.Hour, s, nil)

	snapshot, err := agent.collect()
	require.NoError(t, err)
	assert.Nil(t, snapshot)
}

// layoutGPUs writes the procfs information files of the given GPU UUIDs.
func layoutGPUs(t *testing.T, s *Sources, uuids ...string) {
	t.Helper()
	for i, uuid := range uuids {
		writeFile(t, filepath.Join(s.ProcRoot, "driver", "nvidia", "gpus",
			normalizeBDF("00000000:0"+string(rune('b'+i))+":00.0"), "information"),
			"GPU UUID: GPU-"+uuid+"\n")
	}
}

func TestGhostDeviceLifecycle(t *testing.T) {
	s := newTestSources(t, fakeSMI(t, map[string]string{
		"-q -d NVLINK":         "",
		"nvlink -gt d":         "",
		"--query-compute-apps=pid,gpu_uuid --format=csv,noheader": "",
	}))
	layoutGPUs(t, s, "aaa", "bbb")
	reporter := &recordingReporter{}
	agent := NewAgent(time.Hour, s, nil, reporter)

	agent.poll()
	require.Len(t, reporter.snapshots, 1)
	assert.Len(t, reporter.snapshots[0].Devices, 2)

	// one GPU disappears: the next snapshot carries it as a ghost with Up=false
	require.NoError(t, os.RemoveAll(filepath.Join(s.ProcRoot, "driver", "nvidia", "gpus", "0000:0c:00.0")))
	agent.poll()
	require.Len(t, reporter.snapshots, 2)
	require.Len(t, reporter.snapshots[1].Devices, 2)
	ghost := reporter.snapshots[1].Devices[1]
	assert.Equal(t, Device{UUID: "bbb", Up: false}, ghost)

	// and the cycle after that no longer reports it
	agent.poll()
	require.Len(t, reporter.snapshots, 3)
	assert.Len(t, reporter.snapshots[2].Devices, 1)
	assert.Equal(t, "aaa", reporter.snapshots[2].Devices[0].UUID)
}

func TestRunStopsOnContextCancel(t *testing.T) {
	s := newTestSources(t, fakeSMI(t, map[string]string{
		"-q -d NVLINK":         "",
		"nvlink -gt d":         "",
		"--query-compute-apps=pid,gpu_uuid --format=csv,noheader": "",
	}))
	layoutGPUs(t, s, "aaa")
	reporter := &recordingReporter{}
	agent := NewAgent(time.Millisecond, s, nil, reporter)

	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()

	require.NoError(t, agent.Run(ctx))
	assert.NotEmpty(t, reporter.snapshots, "the initial poll must always report")
}
