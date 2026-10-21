// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package gpuevent // import "go.opentelemetry.io/obi/pkg/internal/ebpf/gpuevent"

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/obi/pkg/appolly/app"
)

const (
	gpuUUIDPrefix = "GPU-"
	migUUIDPrefix = "MIG-"
	// gpuInventoryTTL bounds how often the host GPU inventory is re-read.
	gpuInventoryTTL = 30 * time.Second
)

// gpuDevice describes a physical GPU as reported by the NVIDIA driver.
type gpuDevice struct {
	UUID  string
	Model string
	BusID string
}

// gpuInventory maps the process-local CUDA device index carried by CUDA events
// to the physical GPU it refers to. CUDA enumerates devices per process, so the
// mapping depends on the process CUDA_VISIBLE_DEVICES.
type gpuInventory struct {
	procRoot string
	ttl      time.Duration
	now      func() time.Time

	mux       sync.Mutex
	devices   []gpuDevice
	fetchedAt time.Time
	visible   map[app.PID]visibleResolution
}

// visibleResolution holds the devices a process sees, in its own index order.
// ok is false when the mapping cannot be determined and must not be guessed.
type visibleResolution struct {
	devices []gpuDevice
	ok      bool
}

func newGPUInventory() *gpuInventory {
	return &gpuInventory{
		procRoot: "/proc",
		ttl:      gpuInventoryTTL,
		now:      time.Now,
		visible:  map[app.PID]visibleResolution{},
	}
}

// resolve returns the physical GPU behind a process-local device index.
func (g *gpuInventory) resolve(pid app.PID, index uint32) (gpuDevice, bool) {
	visible := g.visibleDevices(pid)
	if !visible.ok || int(index) >= len(visible.devices) {
		return gpuDevice{}, false
	}

	return visible.devices[index], true
}

// singleVisible returns the only GPU visible to the process, which is the
// device CUDA starts every thread on when no device was explicitly selected.
func (g *gpuInventory) singleVisible(pid app.PID) (gpuDevice, bool) {
	visible := g.visibleDevices(pid)
	if !visible.ok || len(visible.devices) != 1 {
		return gpuDevice{}, false
	}

	return visible.devices[0], true
}

// byUUID returns the physical GPU with the given UUID.
func (g *gpuInventory) byUUID(uuid string) (gpuDevice, bool) {
	uuid = strings.TrimPrefix(uuid, gpuUUIDPrefix)

	g.mux.Lock()
	defer g.mux.Unlock()

	g.refreshLocked()
	for _, device := range g.devices {
		if device.UUID == uuid {
			return device, true
		}
	}

	return gpuDevice{}, false
}

// forget drops the cached CUDA_VISIBLE_DEVICES resolution of a process, which
// must happen before its PID can be reused.
func (g *gpuInventory) forget(pid app.PID) {
	g.mux.Lock()
	delete(g.visible, pid)
	g.mux.Unlock()
}

func (g *gpuInventory) visibleDevices(pid app.PID) visibleResolution {
	g.mux.Lock()
	defer g.mux.Unlock()

	g.refreshLocked()

	if resolution, ok := g.visible[pid]; ok {
		return resolution
	}

	resolution := resolveVisibleDevices(g.procRoot, pid, g.devices)
	if g.visible == nil {
		g.visible = map[app.PID]visibleResolution{}
	}
	g.visible[pid] = resolution

	return resolution
}

func (g *gpuInventory) refreshLocked() {
	if !g.fetchedAt.IsZero() && g.now().Sub(g.fetchedAt) < g.ttl {
		return
	}

	g.devices = readGPUInventory(g.procRoot)
	g.fetchedAt = g.now()
	// CUDA_VISIBLE_DEVICES resolutions depend on the inventory, so they must be
	// recomputed when it changes.
	clear(g.visible)
}

// readGPUInventory lists the GPUs exposed by the NVIDIA driver in procfs. A
// host without the driver yields no devices. Devices are ordered by PCI bus
// address, the order CUDA uses for CUDA_DEVICE_ORDER=PCI_BUS_ID.
func readGPUInventory(procRoot string) []gpuDevice {
	gpusDir := filepath.Join(procRoot, "driver", "nvidia", "gpus")
	entries, err := os.ReadDir(gpusDir)
	if err != nil {
		return nil
	}

	devices := make([]gpuDevice, 0, len(entries))
	for _, entry := range entries {
		device, ok := readGPUDevice(filepath.Join(gpusDir, entry.Name()), entry.Name())
		if ok {
			devices = append(devices, device)
		}
	}

	sort.Slice(devices, func(i, j int) bool { return devices[i].BusID < devices[j].BusID })

	return devices
}

// readGPUDevice parses the information file the NVIDIA driver exposes per GPU.
// The directory name is the PCI bus address of the device.
func readGPUDevice(dir, busID string) (gpuDevice, bool) {
	fields := readKeyValues(filepath.Join(dir, "information"))

	device := gpuDevice{
		UUID:  strings.TrimPrefix(strings.TrimSpace(fields["GPU UUID"]), gpuUUIDPrefix),
		Model: fields["Model"],
		BusID: normalizeBusID(fields["Bus Location"]),
	}
	if device.BusID == "" {
		device.BusID = normalizeBusID(busID)
	}
	if device.UUID == "" {
		return gpuDevice{}, false
	}

	return device, true
}

// resolveVisibleDevices interprets the process CUDA_VISIBLE_DEVICES against the
// host inventory. With the variable unset every host GPU is visible, in the
// order CUDA enumerates them. Entries that cannot be resolved make the whole
// mapping unknown instead of guessing.
func resolveVisibleDevices(procRoot string, pid app.PID, devices []gpuDevice) visibleResolution {
	environ, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(int(pid)), "environ"))
	if err != nil {
		// Without the environment the mapping is only unambiguous when the host
		// exposes a single GPU.
		if len(devices) == 1 {
			return visibleResolution{devices: devices, ok: true}
		}
		return visibleResolution{}
	}

	value, set := cudaVisibleDevices(string(environ))
	if !set {
		return visibleResolution{devices: devices, ok: true}
	}
	if value == "" || value == "-1" {
		return visibleResolution{ok: true}
	}

	visible := make([]gpuDevice, 0, len(devices))
	for entry := range strings.SplitSeq(value, ",") {
		device, ok := lookupVisibleDevice(strings.TrimSpace(entry), devices)
		if !ok {
			return visibleResolution{}
		}
		visible = append(visible, device)
	}

	return visibleResolution{devices: visible, ok: true}
}

func cudaVisibleDevices(environ string) (string, bool) {
	const key = "CUDA_VISIBLE_DEVICES="

	for entry := range strings.SplitSeq(environ, "\x00") {
		if value, ok := strings.CutPrefix(entry, key); ok {
			return value, true
		}
	}

	return "", false
}

// lookupVisibleDevice resolves one CUDA_VISIBLE_DEVICES entry: a position in
// the default enumeration, a GPU UUID, or a MIG instance UUID.
func lookupVisibleDevice(entry string, devices []gpuDevice) (gpuDevice, bool) {
	if entry == "" {
		return gpuDevice{}, false
	}

	if position, err := strconv.Atoi(entry); err == nil {
		if position < 0 || position >= len(devices) {
			return gpuDevice{}, false
		}
		return devices[position], true
	}

	uuid := entry
	if parent, ok := migParentUUID(entry); ok {
		uuid = parent
	}
	uuid = strings.TrimPrefix(uuid, gpuUUIDPrefix)

	for _, device := range devices {
		if strings.EqualFold(device.UUID, uuid) {
			return device, true
		}
	}

	return gpuDevice{}, false
}

// migParentUUID extracts the parent GPU UUID from a MIG instance UUID such as
// MIG-GPU-<uuid>/<gi>/<ci>.
func migParentUUID(entry string) (string, bool) {
	rest, ok := strings.CutPrefix(entry, migUUIDPrefix)
	if !ok {
		return "", false
	}

	parent, _, ok := strings.Cut(rest, "/")
	if !ok {
		return "", false
	}

	return parent, true
}

func readKeyValues(path string) map[string]string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}

	fields := map[string]string{}
	for line := range strings.SplitSeq(string(data), "\n") {
		if key, value, ok := splitKeyValue(line); ok {
			fields[key] = value
		}
	}

	return fields
}

func splitKeyValue(line string) (key, value string, ok bool) {
	before, after, ok := strings.Cut(line, ":")
	if !ok {
		return "", "", false
	}

	return strings.TrimSpace(before), strings.TrimSpace(after), true
}

// normalizeBusID canonicalizes a PCI bus address such as "00000000:0B:00.0"
// to the lower-case "0000:0b:00.0" form.
func normalizeBusID(busID string) string {
	busID = strings.ToLower(strings.TrimSpace(busID))

	parts := strings.Split(busID, ":")
	if len(parts) != 3 {
		return busID
	}
	if len(parts[0]) > 4 {
		parts[0] = parts[0][len(parts[0])-4:]
	}

	return parts[0] + ":" + parts[1] + ":" + parts[2]
}
