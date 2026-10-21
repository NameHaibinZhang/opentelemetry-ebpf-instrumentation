// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package gpumon

import (
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// ProcessResolver resolves the identity of a GPU-using process.
type ProcessResolver interface {
	Resolve(pid int) ProcessIdentity
}

// podLookup maps a container ID to the pod metadata owning it.
type podLookup interface {
	PodByContainerID(cid string) *PodMeta
}

// NewK8sResolver returns a resolver that reads the executable name and the
// RANK environment variable from /proc, and the pod and job names from the
// pod store. A nil store keeps the pod and job names empty.
func NewK8sResolver(store *PodStore) ProcessResolver {
	r := &processResolver{
		procRoot: "/proc",
		log:      slog.With("component", "gpumon.processResolver"),
	}
	if store != nil {
		r.podByCID = store
	}
	return r
}

type processResolver struct {
	procRoot string
	podByCID podLookup
	log      *slog.Logger
}

func (r *processResolver) Resolve(pid int) ProcessIdentity {
	identity := ProcessIdentity{
		Executable: r.executable(pid),
		Rank:       r.rank(pid),
	}
	if r.podByCID == nil {
		return identity
	}
	cid := r.containerID(pid)
	if cid == "" {
		return identity
	}
	pod := r.podByCID.PodByContainerID(cid)
	if pod == nil {
		return identity
	}
	identity.PodName = pod.Name
	identity.JobName = pod.JobName
	return identity
}

// containerID parses the container ID out of /proc/<pid>/cgroup. The formats
// cover containerd and CRI-O on both cgroup v1 and v2, including systemd
// scopes and kubepods hierarchies.
var cgroupFormats = []*regexp.Regexp{
	regexp.MustCompile(`^\d+:.*:.*/.*-([\da-fA-F]{64})\.scope`),
	regexp.MustCompile(`^\d+:.*:/kubepods.*([0-9a-f]{64})`),
	regexp.MustCompile(`^\d+:.*:/k8s\.io/([0-9a-f]{64})`),
	regexp.MustCompile(`^\d+:.*:.*/.*/.*/([0-9a-fA-F]{64})`),
	regexp.MustCompile(`^\d+:.*:/(?:\.\./)+([0-9a-fA-F]{64})`),
}

func (r *processResolver) containerID(pid int) string {
	data, err := os.ReadFile(filepath.Join(r.procRoot, strconv.Itoa(pid), "cgroup"))
	if err != nil {
		return ""
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		for _, format := range cgroupFormats {
			if matches := format.FindStringSubmatch(line); matches != nil {
				return strings.ToLower(matches[1])
			}
		}
	}
	return ""
}

// executable returns the name of the process's main executable, from
// /proc/<pid>/comm.
func (r *processResolver) executable(pid int) string {
	data, err := os.ReadFile(filepath.Join(r.procRoot, strconv.Itoa(pid), "comm"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// rank returns the training rank of the process, from the RANK environment
// variable set by distributed training launchers such as torchrun. The value
// is kept only when it is a plain integer.
func (r *processResolver) rank(pid int) string {
	data, err := os.ReadFile(filepath.Join(r.procRoot, strconv.Itoa(pid), "environ"))
	if err != nil {
		return ""
	}
	for entry := range strings.SplitSeq(string(data), "\x00") {
		value, ok := strings.CutPrefix(entry, "RANK=")
		if !ok {
			continue
		}
		if _, err := strconv.Atoi(value); err != nil {
			return ""
		}
		return value
	}
	return ""
}
