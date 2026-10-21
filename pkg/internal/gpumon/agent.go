// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package gpumon

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"time"
)

// Agent periodically polls the GPU sources and reports a snapshot to the
// registered reporters until the context is cancelled.
type Agent struct {
	interval  time.Duration
	sources   *Sources
	resolver  ProcessResolver
	reporters []Reporter
	log       *slog.Logger

	// AER totals of the previous poll, per GPU UUID and severity, to
	// compute deltas from the cumulative sysfs counters.
	aerPrev map[string]map[string]int64
	// GPU UUIDs observed in the previous poll, to report devices that have
	// gone away as up=0 for one cycle.
	knownDevices map[string]struct{}
	// Whether the previous poll had nothing to report, so the "no GPUs"
	// notice is logged on state change instead of every cycle.
	lastEmpty bool
}

func NewAgent(interval time.Duration, sources *Sources, resolver ProcessResolver, reporters ...Reporter) *Agent {
	return &Agent{
		interval:     interval,
		sources:      sources,
		resolver:     resolver,
		reporters:    reporters,
		log:          slog.With("component", "gpumon.Agent"),
		aerPrev:      map[string]map[string]int64{},
		knownDevices: map[string]struct{}{},
	}
}

// Run polls immediately and then on every interval tick.
func (a *Agent) Run(ctx context.Context) error {
	a.poll()

	ticker := time.NewTicker(a.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			a.poll()
		}
	}
}

func (a *Agent) poll() {
	started := time.Now()
	snapshot, err := a.collect()
	if err != nil {
		a.log.Error("GPU monitoring poll failed", "error", err)
		return
	}
	if snapshot == nil {
		if !a.lastEmpty {
			a.log.Info("nothing to report: no GPUs found on this node")
		}
		a.lastEmpty = true
		return
	}
	a.lastEmpty = false
	for _, reporter := range a.reporters {
		reporter.Report(snapshot)
	}
	a.log.Info("GPU snapshot reported",
		"devices", len(snapshot.Devices),
		"nvlinks", len(snapshot.NVLinks),
		"processes", len(snapshot.Processes),
		"conflicts", len(snapshot.Conflicts),
		"aer_deltas", len(snapshot.AER),
		"duration", time.Since(started).Round(time.Millisecond),
	)
}

// collect gathers the GPU state into a Snapshot. A nil Snapshot without
// error means there is nothing to report: no GPUs, and none that went away.
func (a *Agent) collect() (*Snapshot, error) {
	devices, err := a.sources.Inventory()
	if err != nil {
		return nil, err
	}

	current := make(map[string]struct{}, len(devices))
	for _, d := range devices {
		current[d.UUID] = struct{}{}
	}
	ghosts := a.ghostDevices(current)
	a.knownDevices = current

	if len(devices) == 0 && len(ghosts) == 0 {
		return nil, nil
	}

	snapshot := &Snapshot{Devices: append(devices, ghosts...)}
	snapshot.NVLinks = a.sources.NVLinks(devices)
	snapshot.Processes = a.resolvedProcesses()
	snapshot.Conflicts = detectConflicts(snapshot.Processes)
	snapshot.AER = a.aerDeltas(a.sources.AER(devices))
	if len(snapshot.Conflicts) > 0 {
		a.log.Warn("GPU processes from different pods share the same GPU", "count", len(snapshot.Conflicts))
	}
	return snapshot, nil
}

// ghostDevices returns the devices known in the previous poll that are
// missing from the current one, to be reported as up=0 for one cycle.
func (a *Agent) ghostDevices(current map[string]struct{}) []Device {
	var ghosts []Device
	for uuid := range a.knownDevices {
		if _, ok := current[uuid]; !ok {
			a.log.Warn("GPU disappeared from inventory; reporting it as down", "gpu_uuid", uuid)
			ghosts = append(ghosts, Device{UUID: uuid, Up: false})
		}
	}
	sort.Slice(ghosts, func(i, j int) bool { return ghosts[i].UUID < ghosts[j].UUID })
	return ghosts
}

func (a *Agent) resolvedProcesses() []GPUProcess {
	processes := a.sources.Processes()
	for i := range processes {
		if a.resolver != nil {
			processes[i].Identity = a.resolver.Resolve(processes[i].PID)
		}
		if processes[i].Identity.PodName == "" {
			a.log.Debug("GPU process without pod identity", "pid", processes[i].PID, "gpu_uuid", processes[i].GPUUUID)
		}
	}
	sort.Slice(processes, func(i, j int) bool {
		if processes[i].GPUUUID != processes[j].GPUUUID {
			return processes[i].GPUUUID < processes[j].GPUUUID
		}
		return processes[i].PID < processes[j].PID
	})
	return processes
}

// detectConflicts returns the GPU processes that share a GPU with a process
// from another identity, where the identity of a process is its pod or, when
// no pod could be resolved, its PID. Only processes with a known pod are
// reported, because the conflict metric is labeled by pod.
func detectConflicts(processes []GPUProcess) []GPUProcess {
	byGPU := map[string][]GPUProcess{}
	for _, p := range processes {
		byGPU[p.GPUUUID] = append(byGPU[p.GPUUUID], p)
	}

	var conflicts []GPUProcess
	for _, gpuProcesses := range byGPU {
		identities := map[string]struct{}{}
		for _, p := range gpuProcesses {
			identities[processIdentityKey(p)] = struct{}{}
		}
		if len(identities) < 2 {
			continue
		}
		for _, p := range gpuProcesses {
			if p.Identity.PodName != "" {
				conflicts = append(conflicts, p)
			}
		}
	}
	sort.Slice(conflicts, func(i, j int) bool {
		if conflicts[i].GPUUUID != conflicts[j].GPUUUID {
			return conflicts[i].GPUUUID < conflicts[j].GPUUUID
		}
		return conflicts[i].PID < conflicts[j].PID
	})
	return conflicts
}

func processIdentityKey(p GPUProcess) string {
	if p.Identity.PodName != "" {
		return "pod/" + p.Identity.PodName
	}
	return fmt.Sprintf("pid/%d", p.PID)
}

// aerDeltas converts the cumulative AER counters into deltas since the
// previous poll. The first observation of a counter only records the
// baseline; a counter reset is reported as the new total.
func (a *Agent) aerDeltas(samples []AERSample) []AERDelta {
	current := map[string]map[string]int64{}
	for _, s := range samples {
		if current[s.GPUUUID] == nil {
			current[s.GPUUUID] = map[string]int64{}
		}
		current[s.GPUUUID][s.Severity] = s.Total
	}

	var deltas []AERDelta
	for _, s := range samples {
		prev, seen := a.aerPrev[s.GPUUUID][s.Severity]
		if !seen {
			continue
		}
		delta := s.Total - prev
		if delta < 0 {
			delta = s.Total
		}
		if delta == 0 {
			continue
		}
		deltas = append(deltas, AERDelta{GPUUUID: s.GPUUUID, BusID: s.BusID, Severity: s.Severity, Count: delta})
	}
	a.aerPrev = current
	return deltas
}
