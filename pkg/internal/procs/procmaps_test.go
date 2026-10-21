// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

// some tests here rely on concrete values for os.GetPageSize() that might differ in non-Linux environments
//go:build linux

package procs

import (
	"debug/elf"
	"testing"

	"github.com/prometheus/procfs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestModulePathMatching(t *testing.T) {
	maps := makeProcFSMaps([]string{"/something/something/libssl.so.3", "anon_inode:[io_uring]"})

	assert.Nil(t, LibPath("node", maps))
	assert.Equal(t, procPSFromPath("/something/something/libssl.so.3"), LibPath("libssl.so", maps))

	maps = makeProcFSMaps([]string{"libssl.so", "/node"})

	assert.Equal(t, procPSFromPath("/node"), LibPath("node", maps))
	assert.Nil(t, LibPath("libssl.so", maps))
}

func TestModulePathsReturnsAllUniqueMappings(t *testing.T) {
	cuda12 := procPSFromPath("/tmp/a/libcudart.so.12")
	cuda12.Dev = 1
	cuda12.Inode = 12
	cuda12Duplicate := procPSFromPath("/tmp/a/libcudart.so.12")
	cuda12Duplicate.Dev = 1
	cuda12Duplicate.Inode = 12
	cuda13 := procPSFromPath("/opt/cuda/libcudart.so.13")
	cuda13.Dev = 1
	cuda13.Inode = 13
	nonExecutable := procPSFromPath("/opt/cuda/libcudart.so.14")
	nonExecutable.Dev = 1
	nonExecutable.Inode = 14
	nonExecutable.Perms.Execute = false

	maps := []*procfs.ProcMap{cuda12, cuda12Duplicate, cuda13, nonExecutable}

	assert.Equal(t, []*procfs.ProcMap{cuda12, cuda13}, LibPaths("libcudart.so", maps))
	assert.Equal(t, cuda12, LibPath("libcudart.so", maps))
}

func makeProcFSMaps(paths []string) []*procfs.ProcMap {
	res := []*procfs.ProcMap{}

	for _, path := range paths {
		p := procfs.ProcMap{Pathname: path, Perms: &procfs.ProcMapPermissions{Execute: true}}
		res = append(res, &p)
	}

	return res
}

func procPSFromPath(path string) *procfs.ProcMap {
	return &procfs.ProcMap{Pathname: path, Perms: &procfs.ProcMapPermissions{Execute: true}}
}

func TestExeLoadBias(t *testing.T) {
	maps := []*procfs.ProcMap{{
		StartAddr: 0x7f0000400000,
		Offset:    0,
		Pathname:  "/proc/123/exe",
	}}
	progs := []*elf.Prog{{
		ProgHeader: elf.ProgHeader{
			Type:  elf.PT_LOAD,
			Off:   0,
			Vaddr: 0x400000,
		},
	}}

	bias, err := exeLoadBias("/proc/123/exe", maps, progs)
	require.NoError(t, err)
	assert.Equal(t, uint64(0x7f0000000000), bias)
}

func TestExeLoadBiasETExec(t *testing.T) {
	maps := []*procfs.ProcMap{{
		StartAddr: 0x400000,
		Offset:    0,
		Pathname:  "/proc/123/exe",
	}}
	progs := []*elf.Prog{{
		ProgHeader: elf.ProgHeader{
			Type:  elf.PT_LOAD,
			Off:   0,
			Vaddr: 0x400000,
		},
	}}

	bias, err := exeLoadBias("/proc/123/exe", maps, progs)
	require.NoError(t, err)
	assert.Equal(t, uint64(0), bias)
}

func TestExeLoadBiasMatchesMappingOffset(t *testing.T) {
	maps := []*procfs.ProcMap{{
		StartAddr: 0x7f0000401000,
		Offset:    0x1000,
		Pathname:  "/proc/123/exe",
	}}
	progs := []*elf.Prog{{
		ProgHeader: elf.ProgHeader{
			Type:  elf.PT_LOAD,
			Off:   0x1000,
			Vaddr: 0x401000,
		},
	}}

	bias, err := exeLoadBias("/proc/123/exe", maps, progs)
	require.NoError(t, err)
	assert.Equal(t, uint64(0x7f0000000000), bias)
}

func TestFindMappingLoadBias(t *testing.T) {
	maps := []*procfs.ProcMap{{
		StartAddr: 0x7f0000401000,
		Offset:    0x1000,
		Dev:       42,
		Inode:     99,
	}}
	progs := []*elf.Prog{{ProgHeader: elf.ProgHeader{
		Type: elf.PT_LOAD, Off: 0x1000, Vaddr: 0x401000,
	}}}

	bias, err := FindMappingLoadBias(maps, progs, 42, 99)
	require.NoError(t, err)
	assert.Equal(t, uint64(0x7f0000000000), bias)
}
