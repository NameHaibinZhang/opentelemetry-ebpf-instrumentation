// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

// Package procmetrics samples /proc for the processes discovered by OBI and
// publishes process-level metrics (CPU, memory, disk and network I/O) to the
// metrics pipeline.
package procmetrics // import "go.opentelemetry.io/obi/pkg/internal/procmetrics"

import (
	"context"
	"log/slog"
	"runtime"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/procfs"

	"go.opentelemetry.io/obi/pkg/appolly/app"
	"go.opentelemetry.io/obi/pkg/appolly/app/svc"
	"go.opentelemetry.io/obi/pkg/appolly/discover/exec"
	"go.opentelemetry.io/obi/pkg/pipe/msg"
	"go.opentelemetry.io/obi/pkg/pipe/swarm"
)

// DefaultSamplingInterval is the default /proc sampling cadence.
const DefaultSamplingInterval = 5 * time.Second

// userHZ is the number of clock ticks per second used by /proc/<pid>/stat
// CPU time fields.
const userHZ = 100

const (
	// CPUModeUser and CPUModeSystem identify the cpu.mode attribute values.
	CPUModeUser   = "user"
	CPUModeSystem = "system"

	// DiskDirectionRead and DiskDirectionWrite identify the disk.io.direction attribute values.
	DiskDirectionRead  = "read"
	DiskDirectionWrite = "write"

	// NetworkDirectionReceive and NetworkDirectionTransmit identify the network.io.direction attribute values.
	NetworkDirectionReceive  = "receive"
	NetworkDirectionTransmit = "transmit"
)

// Snapshot carries one sampling interval of process metrics. Cumulative
// values (CPU time, disk and network I/O) are reported as deltas since the
// previous sample of the same PID; gauges (utilization, memory) are absolute.
// A Snapshot with Removed set is a tombstone: it carries no values and tells
// exporters to drop the series of the given PID.
type Snapshot struct {
	Service svc.Attrs
	PID     app.PID
	Time    time.Time
	Removed bool

	Command        string
	ExecutablePath string
	ParentPID      app.PID
	Owner          string

	CPUTimeUserSeconds   float64
	CPUTimeSystemSeconds float64
	CPUUtilizationUser   float64
	CPUUtilizationSystem float64
	MemoryUsageBytes     int64
	MemoryVirtualBytes   int64
	DiskReadBytes        uint64
	DiskWriteBytes       uint64
	NetworkReceiveBytes  uint64
	NetworkTransmitBytes uint64
}

type trackedProcess struct {
	service   svc.Attrs
	startTime uint64

	command        string
	executablePath string
	parentPID      app.PID
	owner          string

	lastSample    time.Time
	cpuUserTicks  uint64
	cpuSystemTick uint64
	diskRead      uint64
	diskWrite     uint64
	netReceive    uint64
	netTransmit   uint64
	initialized   bool
}

type processCollector struct {
	events   <-chan exec.ProcessEvent
	output   *msg.Queue[[]Snapshot]
	interval time.Duration
	clock    func() time.Time
	numCPU   float64

	mu      sync.Mutex
	tracked map[app.PID]*trackedProcess
}

// ProcessCollector returns a swarm node that tracks processes from the
// process events queue and samples their /proc entry every interval,
// publishing a batch of Snapshots to the output queue.
func ProcessCollector(
	events *msg.Queue[exec.ProcessEvent],
	output *msg.Queue[[]Snapshot],
	interval time.Duration,
) swarm.InstanceFunc {
	return func(_ context.Context) (swarm.RunFunc, error) {
		if events == nil || output == nil {
			return swarm.EmptyRunFunc()
		}
		if interval <= 0 {
			interval = DefaultSamplingInterval
		}
		numCPU := float64(runtime.NumCPU())
		if numCPU <= 0 {
			numCPU = 1
		}
		c := &processCollector{
			events:   events.Subscribe(msg.SubscriberName("procmetrics.ProcessCollector")),
			output:   output,
			interval: interval,
			clock:    time.Now,
			numCPU:   numCPU,
			tracked:  map[app.PID]*trackedProcess{},
		}
		return c.run, nil
	}
}

func (c *processCollector) run(ctx context.Context) {
	log := slog.With("component", "procmetrics.ProcessCollector", "interval", c.interval)
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Debug("context done, stopping process metrics collector")
			return
		case ev, ok := <-c.events:
			if !ok {
				log.Debug("process events channel closed, stopping process metrics collector")
				return
			}
			c.onProcessEvent(&ev)
		case now := <-ticker.C:
			c.harvestAll(now)
		}
	}
}

func (c *processCollector) onProcessEvent(ev *exec.ProcessEvent) {
	if ev.File == nil {
		return
	}
	pid := ev.File.Pid()

	c.mu.Lock()
	defer c.mu.Unlock()

	switch ev.Type {
	case exec.ProcessEventCreated:
		if _, ok := c.tracked[pid]; ok {
			return
		}
		c.tracked[pid] = &trackedProcess{
			service:        ev.ServiceFile().ServiceAttrs(),
			startTime:      ev.File.StartTime(),
			command:        ev.File.ExecutableName(),
			executablePath: ev.File.CmdExePath(),
			parentPID:      ev.File.Ppid(),
		}
	case exec.ProcessEventTerminated:
		tp, ok := c.tracked[pid]
		if !ok {
			return
		}
		delete(c.tracked, pid)
		c.output.Send([]Snapshot{{
			Service: tp.service,
			PID:     pid,
			Time:    c.clock(),
			Removed: true,
		}})
	}
}

func (c *processCollector) harvestAll(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if len(c.tracked) == 0 {
		return
	}

	snapshots := make([]Snapshot, 0, len(c.tracked))
	for pid, tp := range c.tracked {
		snapshots = append(snapshots, c.harvest(pid, tp, now))
	}

	if len(snapshots) > 0 {
		c.output.Send(snapshots)
	}
}

// harvest samples a single process. When the PID has been reused or the
// process vanished, it untracks the process and emits a tombstone.
func (c *processCollector) harvest(pid app.PID, tp *trackedProcess, now time.Time) Snapshot {
	proc, err := procfs.NewProc(int(pid))
	if err != nil {
		delete(c.tracked, pid)
		return Snapshot{Service: tp.service, PID: pid, Time: now, Removed: true}
	}

	stat, err := proc.Stat()
	if err != nil || stat.Starttime != tp.startTime {
		// The process terminated without an event, or its PID was reused.
		delete(c.tracked, pid)
		return Snapshot{Service: tp.service, PID: pid, Time: now, Removed: true}
	}

	snap := Snapshot{
		Service:        tp.service,
		PID:            pid,
		Time:           now,
		Command:        tp.command,
		ExecutablePath: tp.executablePath,
		ParentPID:      tp.parentPID,
		Owner:          tp.owner,
	}

	wallSeconds := now.Sub(tp.lastSample).Seconds()

	if !tp.initialized {
		tp.initialized = true
		tp.lastSample = now
		tp.cpuUserTicks = uint64(stat.UTime)
		tp.cpuSystemTick = uint64(stat.STime)
		if status, err := proc.NewStatus(); err == nil {
			snap.Owner = trackedOwner(status.UIDs)
			tp.owner = snap.Owner
		}
		// IO and network baselines: leave the deltas at zero for this sample.
		return snap
	}

	cpuUser := uint64(stat.UTime)
	cpuSystem := uint64(stat.STime)
	snap.CPUTimeUserSeconds = float64(delta(tp.cpuUserTicks, cpuUser)) / userHZ
	snap.CPUTimeSystemSeconds = float64(delta(tp.cpuSystemTick, cpuSystem)) / userHZ
	if wallSeconds > 0 {
		snap.CPUUtilizationUser = snap.CPUTimeUserSeconds / wallSeconds / c.numCPU
		snap.CPUUtilizationSystem = snap.CPUTimeSystemSeconds / wallSeconds / c.numCPU
	}
	tp.cpuUserTicks = cpuUser
	tp.cpuSystemTick = cpuSystem

	if status, err := proc.NewStatus(); err == nil {
		snap.MemoryUsageBytes = int64(status.VmRSS)
		snap.MemoryVirtualBytes = int64(status.VmSize)
	}

	if io, err := proc.IO(); err == nil {
		snap.DiskReadBytes = delta(tp.diskRead, io.ReadBytes)
		snap.DiskWriteBytes = delta(tp.diskWrite, io.WriteBytes)
		tp.diskRead = io.ReadBytes
		tp.diskWrite = io.WriteBytes
	}

	if netDev, err := proc.NetDev(); err == nil {
		total := netDev.Total()
		snap.NetworkReceiveBytes = delta(tp.netReceive, total.RxBytes)
		snap.NetworkTransmitBytes = delta(tp.netTransmit, total.TxBytes)
		tp.netReceive = total.RxBytes
		tp.netTransmit = total.TxBytes
	}

	tp.lastSample = now
	return snap
}

func delta(previous, current uint64) uint64 {
	if current < previous {
		return 0
	}
	return current - previous
}

func trackedOwner(uids [4]uint64) string {
	if uids[0] == 0 && uids[1] == 0 && uids[2] == 0 && uids[3] == 0 {
		return ""
	}
	return strconv.FormatUint(uids[0], 10)
}
