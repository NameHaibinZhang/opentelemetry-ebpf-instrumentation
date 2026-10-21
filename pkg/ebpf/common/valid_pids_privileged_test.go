// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

//go:build linux && privileged_tests

package ebpfcommon

import (
	"testing"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/rlimit"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.opentelemetry.io/obi/pkg/appolly/app"
)

func newTestValidPids(t *testing.T, words uint32) *ebpf.Map {
	t.Helper()
	require.NoError(t, rlimit.RemoveMemlock())

	m, err := ebpf.NewMap(&ebpf.MapSpec{
		Type:       ebpf.Array,
		KeySize:    4,
		ValueSize:  8,
		MaxEntries: words,
	})
	require.NoError(t, err)
	t.Cleanup(func() { m.Close() })

	return m
}

func validPidsWord(t *testing.T, m *ebpf.Map, word uint32) uint64 {
	t.Helper()

	var bits uint64
	require.NoError(t, m.Lookup(word, &bits))

	return bits
}

func TestRebuildValidPidsSetsOneBitPerPid(t *testing.T) {
	m := newTestValidPids(t, ValidPidsWords)
	var bitmap ValidPidsBitmap

	// 41000 and 41007 share word 640; 7 is a pod pid when OBI runs as a sidecar
	require.NoError(t, bitmap.Rebuild(m, []app.PID{41000, 41007, 7}))

	assert.Equal(t, uint64(1<<40|1<<47), validPidsWord(t, m, 640))
	assert.Equal(t, uint64(1<<7), validPidsWord(t, m, 0))
	assert.Equal(t, uint64(0), validPidsWord(t, m, 641))
}

func TestRebuildValidPidsClearsRemovedPids(t *testing.T) {
	m := newTestValidPids(t, ValidPidsWords)
	var bitmap ValidPidsBitmap

	require.NoError(t, bitmap.Rebuild(m, []app.PID{41000, 41007}))

	require.NoError(t, bitmap.Rebuild(m, []app.PID{41007}))
	assert.Equal(t, uint64(1<<47), validPidsWord(t, m, 640))

	require.NoError(t, bitmap.Rebuild(m, nil))
	assert.Equal(t, uint64(0), validPidsWord(t, m, 640))
}

func TestRebuildValidPidsWritesOnlyChangedWords(t *testing.T) {
	m := newTestValidPids(t, ValidPidsWords)
	var bitmap ValidPidsBitmap

	// a word the bitmap never wrote: a rebuild that rewrote every word would zero it
	const untouchedWord = 5
	require.NoError(t, m.Put(uint32(untouchedWord), uint64(0xff)))

	require.NoError(t, bitmap.Rebuild(m, []app.PID{41000}))

	assert.Equal(t, uint64(0xff), validPidsWord(t, m, untouchedWord))
	assert.Equal(t, uint64(1<<40), validPidsWord(t, m, 640))
}

func TestRebuildValidPidsSkipsPidsPastTheBitmap(t *testing.T) {
	m := newTestValidPids(t, ValidPidsWords)
	var bitmap ValidPidsBitmap

	require.NoError(t, bitmap.Rebuild(m, []app.PID{app.PID(ValidPidsWords * 64), 41000}))

	assert.Equal(t, uint64(1<<40), validPidsWord(t, m, 640))
}

func TestRebuildValidPidsResetsOnNewMap(t *testing.T) {
	var bitmap ValidPidsBitmap

	first := newTestValidPids(t, ValidPidsWords)
	require.NoError(t, bitmap.Rebuild(first, []app.PID{41000}))

	// the internal maps are recreated zeroed when the tracers are reloaded
	second := newTestValidPids(t, ValidPidsWords)
	require.NoError(t, bitmap.Rebuild(second, []app.PID{41000}))

	assert.Equal(t, uint64(1<<40), validPidsWord(t, second, 640))
}

func TestValidateValidPidsMapRejectsAnotherSize(t *testing.T) {
	require.NoError(t, ValidateValidPidsMap(newTestValidPids(t, ValidPidsWords)))
	require.Error(t, ValidateValidPidsMap(newTestValidPids(t, ValidPidsWords-1)))
}
