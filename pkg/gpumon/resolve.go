// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package gpumon

import (
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"go.opentelemetry.io/obi/pkg/appolly/app"
	"go.opentelemetry.io/obi/pkg/internal/helpers/container"
	ikube "go.opentelemetry.io/obi/pkg/internal/kube"
	"go.opentelemetry.io/obi/pkg/kube"
	"go.opentelemetry.io/obi/pkg/kube/kubecache/informer"
)

// ProcessResolver resolves the identity of a GPU-using process.
type ProcessResolver interface {
	Resolve(pid int) ProcessIdentity
}

// NewK8sResolver returns a resolver that reads the executable name and the
// RANK environment variable from /proc, and the pod and job names from the
// Kubernetes metadata store. A nil store keeps the pod and job names empty.
func NewK8sResolver(store *kube.Store) ProcessResolver {
	r := &processResolver{
		procRoot:   "/proc",
		infoForPID: container.InfoForPID,
		log:        slog.With("component", "gpumon.processResolver"),
	}
	if store != nil {
		r.podByContainerID = store.PodByContainerID
	}
	return r
}

type processResolver struct {
	procRoot string
	// injectable for testing
	infoForPID       func(pid app.PID) (container.Info, error)
	podByContainerID func(cid string) *ikube.CachedObjMeta
	log              *slog.Logger
}

func (r *processResolver) Resolve(pid int) ProcessIdentity {
	identity := ProcessIdentity{
		Executable: r.executable(pid),
		Rank:       r.rank(pid),
	}
	if r.podByContainerID == nil {
		return identity
	}
	info, err := r.infoForPID(app.PID(pid))
	if err != nil {
		return identity
	}
	pod := r.podByContainerID(info.ContainerID)
	if pod == nil || pod.Meta == nil {
		return identity
	}
	identity.PodName = pod.Meta.Name
	identity.JobName = jobOwnerName(pod.Meta.Pod)
	return identity
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
	for _, entry := range strings.Split(string(data), "\x00") {
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

// jobOwnerName returns the name of the Job owning the pod, from its owner
// references.
func jobOwnerName(pod *informer.PodInfo) string {
	if pod == nil {
		return ""
	}
	for _, owner := range pod.Owners {
		if owner != nil && owner.Kind == "Job" {
			return owner.Name
		}
	}
	return ""
}
