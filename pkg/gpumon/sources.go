// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package gpumon // import "go.opentelemetry.io/obi/pkg/gpumon"

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const (
	nvidiaSMIBinary = "nvidia-smi"
	gpuUUIDPrefix   = "GPU-"
	// lanes per NVLink, constant across the supported GPU generations
	nvlinkWidthLanes = 4
	// NUMA node value meaning "unknown / not applicable"
	unknownNumaNode = -1
)

var aerSeverities = [3]string{"correctable", "nonfatal", "fatal"}

// Sources reads the GPU device state from /proc, sysfs, /dev and nvidia-smi.
// The roots and the SMI function are injectable for testing.
type Sources struct {
	ProcRoot string
	SysRoot  string
	DevRoot  string
	// SMI runs nvidia-smi with the given arguments and returns its stdout.
	SMI func(args ...string) (string, error)
}

func NewSources() *Sources {
	return &Sources{
		ProcRoot: "/proc",
		SysRoot:  "/sys",
		DevRoot:  "/dev",
		SMI:      runSMI,
	}
}

func (s *Sources) log() *slog.Logger {
	return slog.With("component", "gpumon.Sources")
}

func runSMI(args ...string) (string, error) {
	out, err := exec.Command(nvidiaSMIBinary, args...).Output()
	if err != nil {
		return "", fmt.Errorf("running nvidia-smi %s: %w", strings.Join(args, " "), err)
	}
	return string(out), nil
}

// Inventory returns the GPUs present in the system. A nil slice without
// error means there is no NVIDIA driver.
func (s *Sources) Inventory() ([]Device, error) {
	gpusDir := filepath.Join(s.ProcRoot, "driver", "nvidia", "gpus")
	entries, err := os.ReadDir(gpusDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading %s: %w", gpusDir, err)
	}

	migCapable := s.migCapableByBusID()
	devices := make([]Device, 0, len(entries))
	for _, entry := range entries {
		device, ok := s.procDevice(filepath.Join(gpusDir, entry.Name()), entry.Name(), migCapable)
		if ok {
			devices = append(devices, device)
		}
	}
	sort.Slice(devices, func(i, j int) bool { return devices[i].BusID < devices[j].BusID })

	if missingNuma(devices) {
		s.fillNumaFromTopology(devices)
	}
	return devices, nil
}

func (s *Sources) procDevice(dir, fallbackBusID string, migCapable map[string]bool) (Device, bool) {
	fields := readKeyValues(filepath.Join(dir, "information"))

	device := Device{
		UUID:       stripGPUPrefix(fields["GPU UUID"]),
		Model:      fields["Model"],
		BusID:      normalizeBDF(fields["Bus Location"]),
		NumaNode:   unknownNumaNode,
		MigCapable: migCapable[normalizeBDF(fields["Bus Location"])],
	}
	if device.BusID == "" {
		device.BusID = normalizeBDF(fallbackBusID)
	}
	if device.UUID == "" {
		s.log().Debug("GPU without UUID in procfs information; skipping", "bus_id", device.BusID)
		return Device{}, false
	}

	minor, err := strconv.Atoi(strings.TrimSpace(fields["Device Minor"]))
	if err == nil {
		_, statErr := os.Stat(filepath.Join(s.DevRoot, "nvidia"+strconv.Itoa(minor)))
		device.Up = statErr == nil
	} else {
		s.log().Debug("GPU without device minor in procfs information", "bus_id", device.BusID)
	}

	device.NumaNode = s.numaNodeSysfs(device.BusID)
	device.PCIeGeneration, device.PCIeWidth = s.pcieLink(device.BusID)
	return device, true
}

func missingNuma(devices []Device) bool {
	for _, d := range devices {
		if d.NumaNode < 0 {
			return true
		}
	}
	return false
}

// migCapableByBusID queries the MIG mode of each GPU. GPUs missing from the
// answer, or a failed query, are reported as not capable.
func (s *Sources) migCapableByBusID() map[string]bool {
	out, err := s.SMI("--query-gpu=pci_bus_id,mig.mode.current", "--format=csv,noheader")
	if err != nil {
		s.log().Debug("MIG mode query failed; assuming GPUs are not MIG capable", "error", err)
		return nil
	}
	capable := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Split(line, ",")
		if len(fields) != 2 {
			continue
		}
		capable[normalizeBDF(fields[0])] = strings.EqualFold(strings.TrimSpace(fields[1]), "Enabled")
	}
	return capable
}

// fillNumaFromTopology resolves the NUMA node of devices whose sysfs
// numa_node is unknown, matching the GPU CPU affinity reported by
// nvidia-smi topo against the CPU lists of the system NUMA nodes.
func (s *Sources) fillNumaFromTopology(devices []Device) {
	indexes := s.gpuIndexesByBusID()
	if len(indexes) == 0 {
		return
	}
	affinities := s.topoCPUAffinities()
	if len(affinities) == 0 {
		return
	}
	nodeCPUs := s.numaNodeCPULists()
	if len(nodeCPUs) == 0 {
		return
	}

	for i := range devices {
		if devices[i].NumaNode >= 0 {
			continue
		}
		affinity := affinities[indexes[devices[i].BusID]]
		node, ok := nodeContaining(nodeCPUs, affinity)
		if ok {
			devices[i].NumaNode = node
		}
	}
}

// gpuIndexesByBusID maps each GPU bus address to its nvidia-smi index.
func (s *Sources) gpuIndexesByBusID() map[string]string {
	out, err := s.SMI("--query-gpu=index,pci_bus_id", "--format=csv,noheader")
	if err != nil {
		s.log().Debug("GPU index query failed", "error", err)
		return nil
	}
	indexes := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Split(line, ",")
		if len(fields) != 2 {
			continue
		}
		indexes[normalizeBDF(fields[1])] = strings.TrimSpace(fields[0])
	}
	return indexes
}

// topoCPUAffinities maps each GPU index (as printed by "nvidia-smi topo -m",
// e.g. "0") to its CPU affinity list.
func (s *Sources) topoCPUAffinities() map[string]string {
	out, err := s.SMI("topo", "-m")
	if err != nil {
		s.log().Debug("GPU topology query failed", "error", err)
		return nil
	}
	affinities := map[string]string{}
	affinityCol := -1
	for _, line := range strings.Split(out, "\n") {
		cols := splitColumns(line)
		if len(cols) == 0 {
			continue
		}
		if affinityCol < 0 {
			for i, col := range cols {
				if strings.TrimSpace(col) == "CPU Affinity" {
					affinityCol = i
					break
				}
			}
			continue
		}
		if len(cols) <= affinityCol {
			continue
		}
		name := strings.TrimSpace(cols[0])
		if index, ok := strings.CutPrefix(name, "GPU"); ok && index != name {
			affinities[index] = strings.TrimSpace(cols[affinityCol])
		}
	}
	return affinities
}

// numaNodeCPULists maps each NUMA node to its CPU set, from sysfs.
func (s *Sources) numaNodeCPULists() map[int]map[int]struct{} {
	paths, err := filepath.Glob(filepath.Join(s.SysRoot, "devices", "system", "node", "node*", "cpulist"))
	if err != nil || len(paths) == 0 {
		return nil
	}
	nodes := map[int]map[int]struct{}{}
	for _, path := range paths {
		node, err := strconv.Atoi(strings.TrimPrefix(filepath.Base(filepath.Dir(path)), "node"))
		if err != nil {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if cpus := parseCPUList(string(data)); len(cpus) > 0 {
			nodes[node] = cpus
		}
	}
	return nodes
}

func nodeContaining(nodes map[int]map[int]struct{}, affinity string) (int, bool) {
	affinitySet := parseCPUList(affinity)
	if len(affinitySet) == 0 {
		return unknownNumaNode, false
	}
	for node, cpus := range nodes {
		if containsAll(cpus, affinitySet) {
			return node, true
		}
	}
	return unknownNumaNode, false
}

func (s *Sources) numaNodeSysfs(busID string) int {
	if busID == "" {
		return unknownNumaNode
	}
	node, err := readIntFile(filepath.Join(s.SysRoot, "bus", "pci", "devices", busID, "numa_node"))
	if err != nil {
		return unknownNumaNode
	}
	return int(node)
}

func (s *Sources) pcieLink(busID string) (generation, width int) {
	if busID == "" {
		return 0, 0
	}
	devDir := filepath.Join(s.SysRoot, "bus", "pci", "devices", busID)
	speed, err := os.ReadFile(filepath.Join(devDir, "current_link_speed"))
	if err != nil {
		return 0, 0
	}
	generation = pcieGenerationFromSpeed(string(speed))

	widthStr, err := os.ReadFile(filepath.Join(devDir, "current_link_width"))
	if err != nil {
		return generation, 0
	}
	w, err := strconv.Atoi(strings.TrimSpace(string(widthStr)))
	if err != nil {
		return generation, 0
	}
	return generation, w
}

// pcieGenerationFromSpeed maps a sysfs link speed such as "16.0 GT/s PCIe"
// or "16 GT/s" to the PCIe generation number (1 for 2.5GT/s through 7 for
// 128GT/s). Zero means unknown.
func pcieGenerationFromSpeed(speed string) int {
	fields := strings.Fields(strings.TrimSpace(speed))
	if len(fields) == 0 {
		return 0
	}
	gt, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0
	}
	switch {
	case gt < 3:
		return 1
	case gt < 6.5:
		return 2
	case gt < 12:
		return 3
	case gt < 24:
		return 4
	case gt < 48:
		return 5
	case gt < 96:
		return 6
	case gt < 192:
		return 7
	}
	return 0
}

// NVLinks returns the NVLink state of the given devices. Remote GPUs are
// resolved to their UUID through the inventory; NVSwitch remotes keep the
// "NVSwitch:<bus id>" identifier.
func (s *Sources) NVLinks(devices []Device) []NVLink {
	out, err := s.SMI("-q", "-d", "NVLINK")
	if err != nil {
		s.log().Debug("NVLink query failed", "error", err)
		return nil
	}
	uuidByBus := make(map[string]string, len(devices))
	for _, d := range devices {
		uuidByBus[d.BusID] = d.UUID
	}
	links := parseNVLINK(out, uuidByBus)
	s.fillNvlinkRates(links)
	for i := range links {
		links[i].Width = nvlinkWidthLanes
	}
	return links
}

var (
	nvlinkGPUHeaderRe  = regexp.MustCompile(`^GPU\s+\S+\s*(?:\(UUID:\s*(.+?)\s*\))?$`)
	nvlinkLegacyLinkRe = regexp.MustCompile(`^Link\s+(\d+)\s*:\s*(Active|Inactive)$`)
	nvlinkLinkHeaderRe = regexp.MustCompile(`^Link\s+(\d+)$`)
)

// parseNVLINK understands both the current "Link N" block format (with
// Active/Remote Device Type/Remote Device PCI Bus ID fields) and the legacy
// single-line "Link N : Active" format.
func parseNVLINK(out string, uuidByBus map[string]string) []NVLink {
	var links []NVLink
	var gpuUUID string
	var link NVLink
	var remoteType, remoteBus string
	inLink := false

	flush := func() {
		if !inLink {
			return
		}
		if remoteType != "" && remoteBus != "" {
			if strings.Contains(remoteType, "Switch") {
				link.SwitchID = "NVSwitch:" + remoteBus
			} else {
				link.PeerUUID = uuidByBus[remoteBus]
			}
		}
		links = append(links, link)
		inLink = false
	}

	for _, rawLine := range strings.Split(out, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}

		if matches := nvlinkGPUHeaderRe.FindStringSubmatch(line); matches != nil && strings.HasPrefix(line, "GPU ") {
			flush()
			uuid := stripGPUPrefix(matches[1])
			if uuid == "" {
				busAndRest := strings.TrimPrefix(line, "GPU ")
				busID := normalizeBDF(strings.Fields(busAndRest)[0])
				uuid = uuidByBus[busID]
			}
			gpuUUID = uuid
			continue
		}
		if matches := nvlinkLegacyLinkRe.FindStringSubmatch(line); matches != nil {
			flush()
			linkID, _ := strconv.Atoi(matches[1])
			links = append(links, NVLink{
				GPUUUID: gpuUUID,
				LinkID:  linkID,
				Active:  matches[2] == "Active",
			})
			continue
		}
		if matches := nvlinkLinkHeaderRe.FindStringSubmatch(line); matches != nil {
			flush()
			linkID, _ := strconv.Atoi(matches[1])
			link = NVLink{GPUUUID: gpuUUID, LinkID: linkID}
			remoteType, remoteBus = "", ""
			inLink = true
			continue
		}

		if !inLink {
			continue
		}
		key, value, ok := splitKeyValue(line)
		if !ok {
			continue
		}
		switch key {
		case "Active":
			link.Active = strings.TrimSpace(value) == "Active"
		case "Remote Device Type":
			remoteType = strings.TrimSpace(value)
		case "Remote Device PCI Bus ID":
			remoteBus = normalizeBDF(value)
		}
	}
	flush()

	return links
}

// fillNvlinkRates fills the link generation number from the transfer rates
// reported by "nvidia-smi nvlink -gt d".
func (s *Sources) fillNvlinkRates(links []NVLink) {
	out, err := s.SMI("nvlink", "-gt", "d")
	if err != nil {
		s.log().Debug("NVLink transfer rate query failed", "error", err)
		return
	}
	rates := map[string]map[int]int{}
	var gpuUUID string
	for _, rawLine := range strings.Split(out, "\n") {
		line := strings.TrimSpace(rawLine)
		if uuid, ok := nvlinkRateGPUUUID(line); ok {
			gpuUUID = uuid
			continue
		}
		if gpuUUID == "" {
			continue
		}
		if linkID, gbps, ok := parseNvlinkRateLine(line); ok && gbps > 0 {
			if rates[gpuUUID] == nil {
				rates[gpuUUID] = map[int]int{}
			}
			rates[gpuUUID][linkID] = nvlinkGeneration(gbps)
		}
	}
	for i := range links {
		if rate, ok := rates[links[i].GPUUUID][links[i].LinkID]; ok {
			links[i].Rate = rate
		}
	}
}

var (
	nvlinkRateGPURe = regexp.MustCompile(`^GPU\s+\d+\s*:.*\(UUID:\s*(.+?)\s*\)$`)
	nvlinkRateRe    = regexp.MustCompile(`^Link\s+(\d+)\s*:\s*([\d.]+)\s*GB/s$`)
)

func nvlinkRateGPUUUID(line string) (string, bool) {
	matches := nvlinkRateGPURe.FindStringSubmatch(line)
	if matches == nil {
		return "", false
	}
	return stripGPUPrefix(matches[1]), true
}

func parseNvlinkRateLine(line string) (linkID int, gbps float64, ok bool) {
	matches := nvlinkRateRe.FindStringSubmatch(line)
	if matches == nil {
		return 0, 0, false
	}
	linkID, err := strconv.Atoi(matches[1])
	if err != nil {
		return 0, 0, false
	}
	gbps, err = strconv.ParseFloat(matches[2], 64)
	if err != nil {
		return 0, 0, false
	}
	return linkID, gbps, true
}

// nvlinkGeneration maps a link transfer rate in GB/s to the NVLink
// generation number: 20 for NVLink1, 25 for NVLink2, 26 for NVLink3, 32
// for NVLink4 (H100) and 40+ for NVLink5.
func nvlinkGeneration(gbps float64) int {
	switch {
	case gbps < 22.5:
		return 1
	case gbps < 25.4:
		return 2
	case gbps < 26.1:
		return 3
	case gbps < 38:
		return 4
	default:
		return 5
	}
}

// AER returns the cumulative PCIe AER error counts of the given devices.
// GPUs without AER support in sysfs are skipped.
func (s *Sources) AER(devices []Device) []AERSample {
	var samples []AERSample
	for _, d := range devices {
		if d.BusID == "" {
			continue
		}
		devDir := filepath.Join(s.SysRoot, "bus", "pci", "devices", d.BusID)
		for _, severity := range aerSeverities {
			total, err := readIntFile(filepath.Join(devDir, "aer_dev_"+severity))
			if err != nil {
				continue
			}
			samples = append(samples, AERSample{GPUUUID: d.UUID, BusID: d.BusID, Severity: severity, Total: total})
		}
	}
	return samples
}

// Processes returns the processes using a GPU. The identity is left empty;
// the agent resolves it afterwards. MIG instance UUIDs are skipped because
// they do not identify a whole GPU.
func (s *Sources) Processes() []GPUProcess {
	out, err := s.SMI("--query-compute-apps=pid,gpu_uuid", "--format=csv,noheader")
	if err != nil {
		s.log().Debug("compute apps query failed", "error", err)
		return nil
	}
	var processes []GPUProcess
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Split(line, ",")
		if len(fields) != 2 {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(fields[0]))
		if err != nil {
			continue
		}
		uuid := strings.TrimSpace(fields[1])
		if uuid == "" || strings.HasPrefix(uuid, "MIG-") {
			continue
		}
		processes = append(processes, GPUProcess{GPUUUID: stripGPUPrefix(uuid), PID: pid})
	}
	return processes
}

func stripGPUPrefix(uuid string) string {
	return strings.TrimPrefix(strings.TrimSpace(uuid), gpuUUIDPrefix)
}

// normalizeBDF canonicalizes a PCI bus address such as "00000000:0B:00.0"
// to the sysfs form "0000:0b:00.0".
func normalizeBDF(busID string) string {
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

func readKeyValues(path string) map[string]string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	fields := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		if key, value, ok := splitKeyValue(line); ok {
			fields[key] = value
		}
	}
	return fields
}

func splitKeyValue(line string) (key, value string, ok bool) {
	idx := strings.Index(line, ":")
	if idx < 0 {
		return "", "", false
	}
	return strings.TrimSpace(line[:idx]), strings.TrimSpace(line[idx+1:]), true
}

func readIntFile(path string) (int64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	value := strings.TrimSpace(string(data))
	total, err := strconv.ParseInt(value, 10, 64)
	if err == nil {
		return total, nil
	}
	// AER counters may be formatted as hexadecimal
	return strconv.ParseInt(value, 0, 64)
}

// splitColumns splits a nvidia-smi topology row. Rows are tab separated,
// with multi-space separation as fallback.
func splitColumns(line string) []string {
	if strings.TrimSpace(line) == "" {
		return nil
	}
	cols := strings.Split(line, "\t")
	if len(cols) > 1 {
		return cols
	}
	return regexp.MustCompile(`\s{2,}`).Split(strings.TrimSpace(line), -1)
}

// parseCPUList parses a CPU list such as "0-3,8,10-12" into a CPU set.
func parseCPUList(list string) map[int]struct{} {
	cpus := map[int]struct{}{}
	for _, token := range strings.Split(strings.TrimSpace(list), ",") {
		token = strings.TrimSpace(token)
		if token == "" {
			continue
		}
		bounds := strings.SplitN(token, "-", 2)
		first, err := strconv.Atoi(bounds[0])
		if err != nil {
			continue
		}
		if len(bounds) == 1 {
			cpus[first] = struct{}{}
			continue
		}
		last, err := strconv.Atoi(bounds[1])
		if err != nil || last < first {
			continue
		}
		for cpu := first; cpu <= last; cpu++ {
			cpus[cpu] = struct{}{}
		}
	}
	return cpus
}

func containsAll(set, subset map[int]struct{}) bool {
	for cpu := range subset {
		if _, ok := set[cpu]; !ok {
			return false
		}
	}
	return true
}
