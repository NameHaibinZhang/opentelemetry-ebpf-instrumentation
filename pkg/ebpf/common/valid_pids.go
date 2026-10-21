// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package ebpfcommon // import "go.opentelemetry.io/obi/pkg/ebpf/common"

import (
	"fmt"
	"log/slog"
	"sync"

	"github.com/cilium/ebpf"

	"go.opentelemetry.io/obi/pkg/appolly/app"
)

const ValidPidsWords = uint32(BpfValidPidsSizeK_validPidsWords)

// ValidPidsBitmap writes the valid_pids BPF map (bpf/pid/maps/valid_pids.h)
// that valid_pid() reads. The map is internal, so every tracer loading it gets
// the same instance: they must write it through the one ValidPidsBitmap of their
// EBPFEventContext, or the words cached by one would go stale after another wrote.
type ValidPidsBitmap struct {
	mu sync.Mutex
	m  *ebpf.Map
	// the words as last written to m
	words []uint64
}

// ValidateValidPidsMap ensures the loaded map matches the index space written
// by Rebuild: a smaller map has no words for the highest pids, which
// valid_pid() then rejects.
func ValidateValidPidsMap(m *ebpf.Map) error {
	if got := m.MaxEntries(); got != ValidPidsWords {
		return fmt.Errorf(
			"valid_pids BPF map holds %d entries, expected %d: BPF and userspace PID filter constants have diverged",
			got, ValidPidsWords)
	}

	return nil
}

// Rebuild sets in m the bits of pids and clears the others, writing only the
// words that changed.
func (b *ValidPidsBitmap) Rebuild(m *ebpf.Map, pids []app.PID) error {
	if m == nil {
		return nil
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	// internal maps are recreated, zeroed, when the tracers are loaded again
	if b.m != m {
		b.m = m
		b.words = make([]uint64, ValidPidsWords)
	}

	log := slog.With("component", "ebpf.ValidPidsBitmap")

	want := map[uint32]uint64{}
	for _, pid := range pids {
		if uint64(pid) >= uint64(ValidPidsWords)*64 {
			log.Warn("pid beyond the BPF PID filter, it won't be instrumented", "pid", pid)
			continue
		}
		want[uint32(pid/64)] |= 1 << (pid % 64)
	}

	for word, bits := range b.words {
		if _, ok := want[uint32(word)]; bits != 0 && !ok {
			want[uint32(word)] = 0
		}
	}

	written := 0
	for word, bits := range want {
		if b.words[word] == bits {
			continue
		}
		if err := m.Put(word, bits); err != nil {
			return fmt.Errorf("writing word %d of the BPF PID filter: %w", word, err)
		}
		b.words[word] = bits
		written++
	}

	log.Debug("BPF PID filter rebuilt", "pids", len(pids), "wordsWritten", written)

	return nil
}
