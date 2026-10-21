// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package gpuevent

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.opentelemetry.io/obi/pkg/appolly/app"
)

const (
	testUUID0 = "11111111-1111-1111-1111-111111111111"
	testUUID1 = "22222222-2222-2222-2222-222222222222"
)

func writeGPUInformation(t *testing.T, procRoot, busDir, model, uuid, busLocation string) {
	t.Helper()

	dir := filepath.Join(procRoot, "driver", "nvidia", "gpus", busDir)
	require.NoError(t, os.MkdirAll(dir, 0o755))

	content := fmt.Sprintf("Model: %s\nGPU UUID: %s\nBus Location: %s\n", model, uuid, busLocation)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "information"), []byte(content), 0o644))
}

func writeProcessEnviron(t *testing.T, procRoot string, pid app.PID, environ string) {
	t.Helper()

	dir := filepath.Join(procRoot, strconv.Itoa(int(pid)))
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "environ"), []byte(environ), 0o644))
}

func TestReadGPUInventory(t *testing.T) {
	procRoot := t.TempDir()
	writeGPUInformation(t, procRoot, "0000:3b:00.0", "NVIDIA H20", "GPU-"+testUUID1, "00000000:3B:00.0")
	writeGPUInformation(t, procRoot, "0000:0b:00.0", "NVIDIA H20", "GPU-"+testUUID0, "00000000:0B:00.0")
	// GPUs without a UUID cannot be attributed and are skipped.
	writeGPUInformation(t, procRoot, "0000:5b:00.0", "NVIDIA H20", "", "00000000:5B:00.0")

	devices := readGPUInventory(procRoot)

	require.Len(t, devices, 2)
	assert.Equal(t, gpuDevice{UUID: testUUID0, Model: "NVIDIA H20", BusID: "0000:0b:00.0"}, devices[0])
	assert.Equal(t, gpuDevice{UUID: testUUID1, Model: "NVIDIA H20", BusID: "0000:3b:00.0"}, devices[1])
}

func TestReadGPUInventoryWithoutDriver(t *testing.T) {
	assert.Empty(t, readGPUInventory(t.TempDir()))
}

func TestResolveVisibleDevices(t *testing.T) {
	devices := []gpuDevice{
		{UUID: testUUID0, Model: "NVIDIA H20", BusID: "0000:0b:00.0"},
		{UUID: testUUID1, Model: "NVIDIA H20", BusID: "0000:3b:00.0"},
	}
	const pid = app.PID(1234)

	tests := []struct {
		name      string
		environ   string
		noEnviron bool
		devices   []gpuDevice
		expected  []gpuDevice
		ok        bool
	}{
		{
			name:     "variable unset exposes every GPU",
			environ:  "PATH=/usr/bin\x00",
			expected: devices,
			ok:       true,
		},
		{
			name:     "numeric order remaps indices",
			environ:  "CUDA_VISIBLE_DEVICES=1,0\x00",
			expected: []gpuDevice{devices[1], devices[0]},
			ok:       true,
		},
		{
			name:     "GPU UUID entry",
			environ:  "CUDA_VISIBLE_DEVICES=GPU-" + testUUID1 + "\x00",
			expected: []gpuDevice{devices[1]},
			ok:       true,
		},
		{
			name:     "MIG instance resolves to the parent GPU",
			environ:  "CUDA_VISIBLE_DEVICES=MIG-GPU-" + testUUID0 + "/1/0\x00",
			expected: []gpuDevice{devices[0]},
			ok:       true,
		},
		{
			name:     "empty value exposes no GPU",
			environ:  "CUDA_VISIBLE_DEVICES=\x00",
			expected: nil,
			ok:       true,
		},
		{
			name:     "no device value exposes no GPU",
			environ:  "CUDA_VISIBLE_DEVICES=-1\x00",
			expected: nil,
			ok:       true,
		},
		{
			name:    "unresolvable entry makes the mapping unknown",
			environ: "CUDA_VISIBLE_DEVICES=0,bogus\x00",
		},
		{
			name:    "index beyond the inventory is unresolvable",
			environ: "CUDA_VISIBLE_DEVICES=2\x00",
		},
		{
			name:      "unreadable environment with a single GPU",
			noEnviron: true,
			devices:   devices[:1],
			expected:  devices[:1],
			ok:        true,
		},
		{
			name:      "unreadable environment with several GPUs",
			noEnviron: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			procRoot := t.TempDir()
			if !tc.noEnviron {
				writeProcessEnviron(t, procRoot, pid, tc.environ)
			}

			inventoryDevices := devices
			if tc.devices != nil {
				inventoryDevices = tc.devices
			}

			resolution := resolveVisibleDevices(procRoot, pid, inventoryDevices)

			assert.Equal(t, tc.ok, resolution.ok)
			assert.Equal(t, tc.expected, resolution.devices)
		})
	}
}

func TestGPUInventoryResolveAndForget(t *testing.T) {
	procRoot := t.TempDir()
	writeGPUInformation(t, procRoot, "0000:0b:00.0", "NVIDIA H20", "GPU-"+testUUID0, "00000000:0B:00.0")
	writeGPUInformation(t, procRoot, "0000:3b:00.0", "NVIDIA H20", "GPU-"+testUUID1, "00000000:3B:00.0")

	const pid = app.PID(42)
	writeProcessEnviron(t, procRoot, pid, "CUDA_VISIBLE_DEVICES=1\x00")

	inventory := newGPUInventory()
	inventory.procRoot = procRoot

	device, ok := inventory.resolve(pid, 0)
	require.True(t, ok)
	assert.Equal(t, testUUID1, device.UUID)

	_, ok = inventory.resolve(pid, 1)
	assert.False(t, ok, "index beyond the visible devices must not resolve")

	writeProcessEnviron(t, procRoot, pid, "CUDA_VISIBLE_DEVICES=0\x00")

	device, ok = inventory.resolve(pid, 0)
	require.True(t, ok)
	assert.Equal(t, testUUID1, device.UUID, "cached resolution survives environment rewrites")

	inventory.forget(pid)

	device, ok = inventory.resolve(pid, 0)
	require.True(t, ok)
	assert.Equal(t, testUUID0, device.UUID)
}

func TestGPUInventoryRefreshesOnTTL(t *testing.T) {
	procRoot := t.TempDir()
	writeGPUInformation(t, procRoot, "0000:0b:00.0", "NVIDIA H20", "GPU-"+testUUID0, "00000000:0B:00.0")

	now := time.Unix(1000, 0)
	inventory := newGPUInventory()
	inventory.procRoot = procRoot
	inventory.now = func() time.Time { return now }

	device, ok := inventory.byUUID("GPU-" + testUUID0)
	require.True(t, ok)
	assert.Equal(t, testUUID0, device.UUID)

	writeGPUInformation(t, procRoot, "0000:3b:00.0", "NVIDIA H20", "GPU-"+testUUID1, "00000000:3B:00.0")
	now = now.Add(gpuInventoryTTL / 2)

	_, ok = inventory.byUUID(testUUID1)
	assert.False(t, ok, "inventory must be cached within the TTL")

	now = now.Add(gpuInventoryTTL)

	device, ok = inventory.byUUID(testUUID1)
	require.True(t, ok)
	assert.Equal(t, testUUID1, device.UUID)
}

func TestSingleVisible(t *testing.T) {
	procRoot := t.TempDir()
	writeGPUInformation(t, procRoot, "0000:0b:00.0", "NVIDIA H20", "GPU-"+testUUID0, "00000000:0B:00.0")
	writeGPUInformation(t, procRoot, "0000:3b:00.0", "NVIDIA H20", "GPU-"+testUUID1, "00000000:3B:00.0")

	inventory := newGPUInventory()
	inventory.procRoot = procRoot

	writeProcessEnviron(t, procRoot, 7, "CUDA_VISIBLE_DEVICES=0,1\x00")
	writeProcessEnviron(t, procRoot, 8, "CUDA_VISIBLE_DEVICES=1\x00")

	_, ok := inventory.singleVisible(7)
	assert.False(t, ok, "two visible GPUs are ambiguous")

	device, ok := inventory.singleVisible(8)
	require.True(t, ok)
	assert.Equal(t, testUUID1, device.UUID)
}
