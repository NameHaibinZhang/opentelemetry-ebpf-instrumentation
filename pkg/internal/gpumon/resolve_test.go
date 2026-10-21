// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package gpumon

import (
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// containerIDFixture is a 64 hex character container ID, the form used by
// containerd and CRI-O on cgroup v1 and v2.
const containerIDFixture = "40c03570b6f4c30bc8d69923d37ee698f5cfcced92c7b7df1c47f6f7887378a9"

func writeProcFile(t *testing.T, procRoot string, pid int, name, content string) {
	t.Helper()
	dir := filepath.Join(procRoot, strconv.Itoa(pid))
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644))
}

func newTestResolver(t *testing.T, procRoot string, lookup podLookup) *processResolver {
	t.Helper()
	return &processResolver{
		procRoot: procRoot,
		podByCID: lookup,
		log:      slog.Default(),
	}
}

type fakePodLookup struct {
	meta *PodMeta
}

func (f fakePodLookup) PodByContainerID(string) *PodMeta {
	return f.meta
}

func TestResolveIdentityFromProc(t *testing.T) {
	procRoot := t.TempDir()
	writeProcFile(t, procRoot, 1, "comm", "python3\n")
	writeProcFile(t, procRoot, 1, "environ", "PATH=/usr/bin\x00RANK=3\x00HOME=/root")
	writeProcFile(t, procRoot, 1, "cgroup", "0::/kubepods/besteffort/pod123/"+containerIDFixture)

	resolver := newTestResolver(t, procRoot, fakePodLookup{
		meta: &PodMeta{Name: "pod-a", JobName: "job-a"},
	})

	identity := resolver.Resolve(1)
	assert.Equal(t, ProcessIdentity{
		Executable: "python3",
		Rank:       "3",
		PodName:    "pod-a",
		JobName:    "job-a",
	}, identity)
}

func TestResolveWithoutPodLookup(t *testing.T) {
	procRoot := t.TempDir()
	writeProcFile(t, procRoot, 1, "comm", "train.py\n")
	writeProcFile(t, procRoot, 1, "environ", "RANK=0\x00")
	writeProcFile(t, procRoot, 1, "cgroup", "0::/kubepods/pod123/"+containerIDFixture)

	resolver := newTestResolver(t, procRoot, nil)
	identity := resolver.Resolve(1)
	assert.Equal(t, "train.py", identity.Executable)
	assert.Equal(t, "0", identity.Rank)
	assert.Empty(t, identity.PodName)
	assert.Empty(t, identity.JobName)
}

func TestResolveUnknownPod(t *testing.T) {
	procRoot := t.TempDir()
	writeProcFile(t, procRoot, 1, "comm", "python3\n")
	writeProcFile(t, procRoot, 1, "cgroup", "0::/kubepods/pod123/"+containerIDFixture)

	resolver := newTestResolver(t, procRoot, fakePodLookup{meta: nil})
	identity := resolver.Resolve(1)
	assert.Equal(t, "python3", identity.Executable)
	assert.Empty(t, identity.PodName)
}

func TestResolveMissingFiles(t *testing.T) {
	resolver := newTestResolver(t, t.TempDir(), nil)
	assert.Equal(t, ProcessIdentity{}, resolver.Resolve(1))
}

func TestRank(t *testing.T) {
	for name, tc := range map[string]struct {
		environ string
		rank    string
	}{
		"integer rank":  {environ: "RANK=12\x00PATH=/bin", rank: "12"},
		"not a number":  {environ: "RANK=worker-0\x00", rank: ""},
		"unset":         {environ: "PATH=/bin\x00", rank: ""},
		"empty":         {environ: "RANK=\x00", rank: ""},
		"substring key": {environ: "MY_RANK=5\x00", rank: ""},
	} {
		t.Run(name, func(t *testing.T) {
			procRoot := t.TempDir()
			writeProcFile(t, procRoot, 1, "environ", tc.environ)
			resolver := newTestResolver(t, procRoot, nil)
			assert.Equal(t, tc.rank, resolver.rank(1))
		})
	}
}

func TestContainerIDFormats(t *testing.T) {
	for name, tc := range map[string]struct {
		cgroup string
		want   string
	}{
		"containerd systemd scope v1": {
			cgroup: "1:name=systemd:/system.slice/containerd-" + containerIDFixture + ".scope",
			want:   containerIDFixture,
		},
		"kubepods v1": {
			cgroup: "0::/kubepods/besteffort/pod1234/" + containerIDFixture,
			want:   containerIDFixture,
		},
		"k8s.io": {
			cgroup: "0::/k8s.io/" + containerIDFixture,
			want:   containerIDFixture,
		},
		"kubepods slice with scope": {
			cgroup: "0::/kubepods.slice/kubepods-burstable.slice/cri-containerd-" + containerIDFixture + ".scope",
			want:   containerIDFixture,
		},
		"relative path": {
			cgroup: "0::/../../" + containerIDFixture,
			want:   containerIDFixture,
		},
		"uppercase is lowercased": {
			cgroup: "1:name=systemd:/system.slice/containerd-40C03570B6F4C30BC8D69923D37EE698F5CFCCED92C7B7DF1C47F6F7887378A9.scope",
			want:   containerIDFixture,
		},
		"no match": {
			cgroup: "0::/user.slice/session-1.scope",
			want:   "",
		},
	} {
		t.Run(name, func(t *testing.T) {
			procRoot := t.TempDir()
			writeProcFile(t, procRoot, 1, "cgroup", tc.cgroup+"\n")
			resolver := newTestResolver(t, procRoot, nil)
			assert.Equal(t, tc.want, resolver.containerID(1))
		})
	}
}

func TestContainerIDFirstMatchingLineWins(t *testing.T) {
	procRoot := t.TempDir()
	other := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	writeProcFile(t, procRoot, 1, "cgroup",
		"0::/user.slice/session.scope\n"+
			"1::/k8s.io/"+other+"\n"+
			"2::/k8s.io/"+containerIDFixture+"\n")

	resolver := newTestResolver(t, procRoot, nil)
	assert.Equal(t, other, resolver.containerID(1))
}

func TestNewK8sResolverNilStoreDoesNotPanic(t *testing.T) {
	resolver := NewK8sResolver(nil)
	require.NotNil(t, resolver)
	// On any platform the missing /proc entry degrades to an empty identity.
	assert.Equal(t, ProcessIdentity{}, resolver.Resolve(1<<30))
}
