// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package gpumon

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeFile creates path (including parent directories) with the given content.
func writeFile(t *testing.T, path string, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

// fakeSMI returns an SMI command that dispatches on the joined arguments and
// errors for unknown invocations.
func fakeSMI(t *testing.T, responses map[string]string) func(args ...string) (string, error) {
	t.Helper()
	return func(args ...string) (string, error) {
		key := strings.Join(args, " ")
		out, ok := responses[key]
		if !ok {
			return "", fmt.Errorf("unexpected nvidia-smi invocation: %s", key)
		}
		return out, nil
	}
}

// newTestSources builds a Sources with temporary procfs, sysfs and devfs roots.
func newTestSources(t *testing.T, smi func(args ...string) (string, error)) *Sources {
	t.Helper()
	s := &Sources{
		ProcRoot: filepath.Join(t.TempDir(), "proc"),
		SysRoot:  filepath.Join(t.TempDir(), "sys"),
		DevRoot:  filepath.Join(t.TempDir(), "dev"),
		SMI:      smi,
	}
	for _, dir := range []string{s.ProcRoot, s.SysRoot, s.DevRoot} {
		require.NoError(t, os.MkdirAll(dir, 0o755))
	}
	return s
}

// writeDevice lays out a GPU under the procfs driver directory, its sysfs
// attributes under the PCI device directory and its /dev node.
func writeDevice(t *testing.T, s *Sources, dirName, busID, information string, devNode string, sysfs map[string]string) {
	t.Helper()
	writeFile(t, filepath.Join(s.ProcRoot, "driver", "nvidia", "gpus", dirName, "information"), information)
	if devNode != "" {
		writeFile(t, filepath.Join(s.DevRoot, devNode), "")
	}
	for name, content := range sysfs {
		writeFile(t, filepath.Join(s.SysRoot, "bus", "pci", "devices", busID, name), content)
	}
}

func TestNormalizeBDF(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
	}{
		{"00000000:0B:00.0", "0000:0b:00.0"},
		{"0000:0B:00.0", "0000:0b:00.0"},
		{"  00000000:0B:00.0  ", "0000:0b:00.0"},
		{"0B:00.0", "0b:00.0"},
		{"not-a-bdf", "not-a-bdf"},
		{"00:0b:00.0:extra", "00:0b:00.0:extra"},
	} {
		assert.Equal(t, tc.want, normalizeBDF(tc.in), "input %q", tc.in)
	}
}

func TestPcieGenerationFromSpeed(t *testing.T) {
	for _, tc := range []struct {
		speed string
		want  int
	}{
		{"", 0},
		{"not a speed", 0},
		{"2.5 GT/s PCIe", 1},
		{"5 GT/s PCIe", 2},
		{"8 GT/s PCIe", 3},
		{"16 GT/s PCIe", 4},
		{"16.0 GT/s PCIe", 4},
		{"32 GT/s PCIe", 5},
		{"64 GT/s PCIe", 6},
		{"128 GT/s PCIe", 7},
		{"256 GT/s PCIe", 0},
	} {
		assert.Equal(t, tc.want, pcieGenerationFromSpeed(tc.speed), "speed %q", tc.speed)
	}
}

func TestNvlinkGeneration(t *testing.T) {
	for _, tc := range []struct {
		gbps float64
		want int
	}{
		{20, 1},
		{22.4, 1},
		{22.5, 2},
		{25, 2},
		{25.4, 3},
		{26, 3},
		{26.1, 4},
		{32, 4},
		{37.9, 4},
		{38, 5},
		{450, 5},
	} {
		assert.Equal(t, tc.want, nvlinkGeneration(tc.gbps), "rate %v GB/s", tc.gbps)
	}
}

func TestParseNvlinkStatus(t *testing.T) {
	const out = `GPU 0: NVIDIA A100-SXM4-40GB (UUID: GPU-aaa)
	Link 0: 25.781 GB/s
	Link 1: <inactive>

GPU 1: NVIDIA A100-SXM4-40GB (UUID: GPU-bbb)
	Link 0: 25.781 GB/s
`
	links := parseNvlinkStatus(out)

	require.Len(t, links, 3)
	assert.Equal(t, NVLink{GPUUUID: "GPU-aaa", LinkID: 0, Active: true, Rate: 3}, links[0], "25.781 GB/s is NVLink3")
	assert.Equal(t, NVLink{GPUUUID: "GPU-aaa", LinkID: 1}, links[1])
	assert.Equal(t, NVLink{GPUUUID: "GPU-bbb", LinkID: 0, Active: true, Rate: 3}, links[2])
}

func TestFillNvlinkRemotes(t *testing.T) {
	const out = `GPU 0: NVIDIA A100-SXM4-40GB (UUID: GPU-aaa)
	Link 0: Remote Device 00000000:1B:00.0: Link 1
	Link 1: Remote Device 00000000:98:00.0: Link 0

GPU 1: NVIDIA A100-SXM4-40GB (UUID: GPU-bbb)
	Link 0: Remote Device 00000000:0B:00.0: Link 0
`
	s := newTestSources(t, fakeSMI(t, map[string]string{"nvlink -R": out}))

	links := []NVLink{
		{GPUUUID: "GPU-aaa", LinkID: 0},
		{GPUUUID: "GPU-aaa", LinkID: 1},
		{GPUUUID: "GPU-bbb", LinkID: 0},
		{GPUUUID: "GPU-bbb", LinkID: 9},
	}
	s.fillNvlinkRemotes(links, map[string]string{"0000:0b:00.0": "GPU-aaa", "0000:1b:00.0": "GPU-bbb"})

	assert.Equal(t, "GPU-bbb", links[0].PeerUUID)
	assert.Equal(t, "NVSwitch:0000:98:00.0", links[1].SwitchID)
	assert.Equal(t, "GPU-aaa", links[2].PeerUUID)
	assert.Equal(t, NVLink{GPUUUID: "GPU-bbb", LinkID: 9}, links[3], "links without a reported remote keep no peer")
}

func TestNVLinks(t *testing.T) {
	const status = `GPU 0: NVIDIA A100-SXM4-40GB (UUID: GPU-aaa)
	Link 0: 25.781 GB/s
	Link 1: <inactive>
`
	const remotes = `GPU 0: NVIDIA A100-SXM4-40GB (UUID: GPU-aaa)
	Link 0: Remote Device 00000000:1B:00.0: Link 0
`
	s := newTestSources(t, fakeSMI(t, map[string]string{"nvlink -s": status, "nvlink -R": remotes}))

	links := s.NVLinks([]Device{{BusID: "0000:1b:00.0", UUID: "GPU-bbb"}})

	require.Len(t, links, 2)
	assert.Equal(t, NVLink{GPUUUID: "GPU-aaa", LinkID: 0, Active: true, Rate: 3, Width: 4, PeerUUID: "GPU-bbb"}, links[0])
	assert.Equal(t, NVLink{GPUUUID: "GPU-aaa", LinkID: 1, Width: 4}, links[1])
}

func TestNVLinksWithoutHardware(t *testing.T) {
	s := newTestSources(t, fakeSMI(t, map[string]string{"nvlink -s": ""}))

	assert.Nil(t, s.NVLinks(nil))
}

func TestProcesses(t *testing.T) {
	const out = `1234, GPU-aaa
5678, GPU-bbb
9999, MIG-abcdef
notapid, GPU-aaa
[Insufficient Permissions], N/A
`
	s := newTestSources(t, fakeSMI(t, map[string]string{
		"--query-compute-apps=pid,gpu_uuid --format=csv,noheader": out,
	}))

	processes := s.Processes()

	assert.Equal(t, []GPUProcess{
		{GPUUUID: "GPU-aaa", PID: 1234},
		{GPUUUID: "GPU-bbb", PID: 5678},
	}, processes)
}

func TestMigCapableByBusID(t *testing.T) {
	const out = `00000000:0B:00.0, Enabled
00000000:1B:00.0, Disabled
00000000:2B:00.0, Not Supported
`
	s := newTestSources(t, fakeSMI(t, map[string]string{
		"--query-gpu=pci.bus_id,mig.mode.current --format=csv,noheader": out,
	}))

	caps := s.migCapableByBusID()
	assert.Equal(t, map[string]bool{
		"0000:0b:00.0": true,
		"0000:1b:00.0": false,
		"0000:2b:00.0": false,
	}, caps)
}

func TestMigCapableByBusIDQueryFailure(t *testing.T) {
	s := newTestSources(t, fakeSMI(t, nil))

	assert.Nil(t, s.migCapableByBusID())
}

func TestInventory(t *testing.T) {
	s := newTestSources(t, fakeSMI(t, map[string]string{
		"--query-gpu=pci.bus_id,mig.mode.current --format=csv,noheader": "00000000:0B:00.0, Enabled\n",
	}))

	writeDevice(t, s, "0000:0b:00.0", "0000:0b:00.0", `Model:			NVIDIA A100
Bus Location:		00000000:0B:00.0
Device Minor:		0
GPU UUID:		GPU-aaa
`, "nvidia0", map[string]string{
		"numa_node":          "0",
		"current_link_speed": "16.0 GT/s PCIe",
		"current_link_width": "16",
	})

	writeDevice(t, s, "0000:1b:00.0", "0000:1b:00.0", `Model:			NVIDIA H100
Bus Location:		00000000:1B:00.0
Device Minor:		1
GPU UUID:		GPU-bbb
`, "", nil)

	// an entry without a GPU UUID is skipped
	writeFile(t, filepath.Join(s.ProcRoot, "driver", "nvidia", "gpus", "0000:2b:00.0", "information"),
		"Model: something else\n")

	devices, err := s.Inventory()
	require.NoError(t, err)

	require.Len(t, devices, 2)
	assert.Equal(t, Device{
		UUID:           "GPU-aaa",
		BusID:          "0000:0b:00.0",
		Model:          "NVIDIA A100",
		Up:             true,
		NumaNode:       0,
		PCIeGeneration: 4,
		PCIeWidth:      16,
		MigCapable:     true,
	}, devices[0])
	assert.Equal(t, Device{
		UUID:           "GPU-bbb",
		BusID:          "0000:1b:00.0",
		Model:          "NVIDIA H100",
		Up:             false,
		NumaNode:       -1,
		PCIeGeneration: 0,
		PCIeWidth:      0,
		MigCapable:     false,
	}, devices[1])
}

func TestInventoryNoDriver(t *testing.T) {
	s := newTestSources(t, fakeSMI(t, nil))

	devices, err := s.Inventory()

	require.NoError(t, err)
	assert.Empty(t, devices)
}

func TestFillNumaFromTopology(t *testing.T) {
	s := newTestSources(t, fakeSMI(t, map[string]string{
		"--query-gpu=index,pci.bus_id --format=csv,noheader": "0, 00000000:0B:00.0\n1, 00000000:1B:00.0\n",
		"topo -m": "\tGPU0\tGPU1\tCPU Affinity\n" +
			"GPU0\t X \tNV2\t0-3\n" +
			"GPU1\tNV2\t X \t4-7\n",
	}))

	writeFile(t, filepath.Join(s.SysRoot, "devices", "system", "node", "node0", "cpulist"), "0-3")
	writeFile(t, filepath.Join(s.SysRoot, "devices", "system", "node", "node1", "cpulist"), "4-7")

	devices := []Device{
		{UUID: "GPU-aaa", BusID: "0000:0b:00.0", NumaNode: -1},
		{UUID: "GPU-bbb", BusID: "0000:1b:00.0", NumaNode: -1},
		{UUID: "GPU-ccc", BusID: "0000:2b:00.0", NumaNode: 1},
	}

	s.fillNumaFromTopology(devices)

	assert.Equal(t, 0, devices[0].NumaNode)
	assert.Equal(t, 1, devices[1].NumaNode)
	assert.Equal(t, 1, devices[2].NumaNode, "an already-known NUMA node must not be overwritten")
}

func TestAER(t *testing.T) {
	s := newTestSources(t, fakeSMI(t, nil))

	pci := filepath.Join(s.SysRoot, "bus", "pci", "devices", "0000:0b:00.0")
	writeFile(t, filepath.Join(pci, "aer_dev_correctable"), "8")
	writeFile(t, filepath.Join(pci, "aer_dev_nonfatal"), "0x10")
	// no aer_dev_fatal file: unsupported severity must be skipped

	samples := s.AER([]Device{
		{UUID: "GPU-aaa", BusID: "0000:0b:00.0"},
		{UUID: "ghost"},
	})

	assert.Equal(t, []AERSample{
		{GPUUUID: "GPU-aaa", BusID: "0000:0b:00.0", Severity: "correctable", Total: 8},
		{GPUUUID: "GPU-aaa", BusID: "0000:0b:00.0", Severity: "nonfatal", Total: 16},
	}, samples)
}

func TestParseCPUList(t *testing.T) {
	assert.Equal(t, map[int]struct{}{0: {}, 1: {}, 2: {}, 3: {}, 8: {}, 10: {}, 11: {}, 12: {}},
		parseCPUList("0-3,8,10-12"))
	assert.Empty(t, parseCPUList(""))
	assert.Empty(t, parseCPUList("garbage"))
	assert.Empty(t, parseCPUList("5-3"))
}
