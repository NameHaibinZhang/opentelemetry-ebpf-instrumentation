// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package procmetrics

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/procfs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.opentelemetry.io/obi/pkg/appolly/app"
	"go.opentelemetry.io/obi/pkg/appolly/app/svc"
	"go.opentelemetry.io/obi/pkg/appolly/discover/exec"
	"go.opentelemetry.io/obi/pkg/export"
	"go.opentelemetry.io/obi/pkg/pipe/msg"
)

func TestHarvestBaselinesAndTombstone(t *testing.T) {
	root := t.TempDir()
	fs, err := procfs.NewFS(root)
	require.NoError(t, err)
	tp := &trackedProcess{startTime: 42, command: "worker", executablePath: "/bin/worker", parentPID: 1}
	c := processCollector{fs: fs, procRoot: root, numCPU: 64, tracked: map[app.PID]*trackedProcess{123: tp}}
	now := time.Unix(100, 0)
	writeProcessSample(t, root, 42, 100, 50, 1000, 2000)

	first := c.harvest(123, tp, now)
	assert.False(t, first.Removed)
	assert.Equal(t, int64(10*os.Getpagesize()), first.MemoryUsageBytes)
	assert.Equal(t, int64(409600), first.MemoryVirtualBytes)
	assert.Zero(t, first.CPUTimeUserSeconds)
	assert.Zero(t, first.DiskReadBytes)
	assert.Zero(t, first.DiskWriteBytes)
	assert.Equal(t, "root", first.Owner)

	writeProcessSample(t, root, 42, 200, 100, 1128, 2256)
	next := c.harvest(123, tp, now.Add(5*time.Second))
	assert.InDelta(t, 1.0, next.CPUTimeUserSeconds, 0.0001)
	assert.InDelta(t, 0.5, next.CPUTimeSystemSeconds, 0.0001)
	assert.InDelta(t, 0.1, next.CPUUtilizationUser, 0.0001)
	assert.InDelta(t, 0.05, next.CPUUtilizationSystem, 0.0001)
	assert.Equal(t, uint64(128), next.DiskReadBytes)
	assert.Equal(t, uint64(256), next.DiskWriteBytes)

	writeProcessSample(t, root, 43, 0, 0, 0, 0)
	removed := c.harvest(123, tp, now.Add(10*time.Second))
	assert.True(t, removed.Removed)
	assert.Equal(t, first.Command, removed.Command)
	assert.Equal(t, first.ExecutablePath, removed.ExecutablePath)
	assert.Equal(t, first.Owner, removed.Owner)
	assert.Empty(t, c.tracked)
}

func TestHarvestEstablishesDiskBaselineAfterReadFailure(t *testing.T) {
	root := t.TempDir()
	fs, err := procfs.NewFS(root)
	require.NoError(t, err)
	tp := &trackedProcess{startTime: 42}
	c := processCollector{fs: fs, numCPU: 1, tracked: map[app.PID]*trackedProcess{123: tp}}
	now := time.Unix(100, 0)
	writeProcessSample(t, root, 42, 100, 50, 1000, 2000)
	require.NoError(t, os.Remove(filepath.Join(root, "123", "io")))
	c.harvest(123, tp, now)

	writeProcessSample(t, root, 42, 100, 50, 1128, 2256)
	baseline := c.harvest(123, tp, now.Add(5*time.Second))
	assert.Zero(t, baseline.DiskReadBytes)
	assert.Zero(t, baseline.DiskWriteBytes)
	writeProcessSample(t, root, 42, 100, 50, 1256, 2512)
	next := c.harvest(123, tp, now.Add(10*time.Second))
	assert.Equal(t, uint64(128), next.DiskReadBytes)
	assert.Equal(t, uint64(256), next.DiskWriteBytes)
}

func TestProcessEventsRespectServiceFeatures(t *testing.T) {
	out := msg.NewQueue[[]Snapshot](msg.ChannelBufferLen(1))
	in := out.Subscribe()
	c := processCollector{output: out, clock: time.Now, tracked: map[app.PID]*trackedProcess{}}
	for _, features := range []export.Features{export.FeatureApplicationRED, export.FeatureProcess} {
		file := exec.New(exec.Init{
			Pid: 123, StartTime: 42, CmdExePath: "/bin/worker",
			Service: svc.Attrs{Features: features},
		})
		c.onProcessEvent(&exec.ProcessEvent{Type: exec.ProcessEventCreated, File: file})
		if !features.Process() {
			assert.Empty(t, c.tracked)
			continue
		}
		require.Contains(t, c.tracked, app.PID(123))
		c.onProcessEvent(&exec.ProcessEvent{Type: exec.ProcessEventTerminated, File: file})
		removed := <-in
		require.Len(t, removed, 1)
		assert.True(t, removed[0].Removed)
		assert.Equal(t, "/bin/worker", removed[0].ExecutablePath)
		assert.Empty(t, c.tracked)
	}
}

func TestProcessEventsReplaceReusedPID(t *testing.T) {
	out := msg.NewQueue[[]Snapshot](msg.ChannelBufferLen(1))
	in := out.Subscribe()
	c := processCollector{output: out, clock: time.Now, tracked: map[app.PID]*trackedProcess{}}
	old := exec.New(exec.Init{Pid: 123, StartTime: 42, CmdExePath: "/bin/old", Service: svc.Attrs{Features: export.FeatureProcess}})
	current := exec.New(exec.Init{Pid: 123, StartTime: 43, CmdExePath: "/bin/new", Service: svc.Attrs{Features: export.FeatureProcess}})
	c.onProcessEvent(&exec.ProcessEvent{Type: exec.ProcessEventCreated, File: old})
	c.onProcessEvent(&exec.ProcessEvent{Type: exec.ProcessEventCreated, File: current})
	removed := <-in
	require.Len(t, removed, 1)
	assert.True(t, removed[0].Removed)
	assert.Equal(t, "/bin/old", removed[0].ExecutablePath)
	assert.Equal(t, uint64(43), c.tracked[123].startTime)
	c.onProcessEvent(&exec.ProcessEvent{Type: exec.ProcessEventTerminated, File: old})
	assert.Contains(t, c.tracked, app.PID(123), "a delayed termination must not remove the new process")
}

func writeProcessSample(t *testing.T, root string, start, user, system, read, write uint64) {
	t.Helper()
	dir := filepath.Join(root, "123")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	passwdDir := filepath.Join(dir, "root", "etc")
	require.NoError(t, os.MkdirAll(passwdDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(passwdDir, "passwd"), []byte("root:x:0:0:root:/root:/bin/sh\n"), 0o600))
	fields := strings.Fields("123 (worker) S " + strings.Repeat("0 ", 49))
	fields[13] = strconv.FormatUint(user, 10)
	fields[14] = strconv.FormatUint(system, 10)
	fields[21] = strconv.FormatUint(start, 10)
	fields[22] = "409600"
	fields[23] = "10"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "stat"), []byte(strings.Join(fields, " ")), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "status"), []byte("Uid:\t0\t0\t0\t0\nCpus_allowed_list:\t0-1\n"), 0o600))
	io := fmt.Sprintf("rchar: 0\nwchar: 0\nsyscr: 0\nsyscw: 0\nread_bytes: %d\nwrite_bytes: %d\ncancelled_write_bytes: 0\n", read, write)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "io"), []byte(io), 0o600))
}
