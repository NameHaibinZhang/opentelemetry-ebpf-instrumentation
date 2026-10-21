// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package gpumon

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	serviceAccountDir = "/var/run/secrets/kubernetes.io/serviceaccount"
	podRefreshPeriod  = 30 * time.Second
	podListTimeout    = 10 * time.Second
	podListLimit      = 500
	maxPodListPages   = 10
	containerIDHexLen = 64
)

// PodMeta is the pod information needed to label a GPU-using process.
type PodMeta struct {
	Name    string
	JobName string
}

// PodStore indexes the pods of the local node by container ID, refreshed
// periodically from the Kubernetes API using the pod's service account.
type PodStore struct {
	client *kubeClient
	log    *slog.Logger

	mu    sync.RWMutex
	byCID map[string]PodMeta
}

// NewPodStore returns a PodStore for the local node. It returns nil when the
// process does not run inside Kubernetes or the API client cannot be built;
// in that case the resolver reports empty pod and job names.
func NewPodStore() *PodStore {
	node := os.Getenv("NODE_NAME")
	if node == "" {
		slog.With("component", "gpumon.PodStore").
			Info("NODE_NAME not set; pod metadata resolution disabled")
		return nil
	}
	client, err := newKubeClient(node)
	if err != nil {
		slog.With("component", "gpumon.PodStore").
			Warn("Kubernetes API client unavailable; pod metadata resolution disabled", "error", err)
		return nil
	}
	return &PodStore{
		client: client,
		log:    slog.With("component", "gpumon.PodStore"),
		byCID:  map[string]PodMeta{},
	}
}

// Start refreshes the pod index immediately and then on every refresh period
// until the context is cancelled.
func (s *PodStore) Start(ctx context.Context) {
	s.refresh(ctx)

	ticker := time.NewTicker(podRefreshPeriod)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.refresh(ctx)
		}
	}
}

// refresh reloads the pod index. On failure the previous index is kept.
func (s *PodStore) refresh(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, podListTimeout)
	defer cancel()

	pods, err := s.client.listPods(ctx)
	if err != nil {
		s.log.Warn("reloading pod index failed; keeping previous data", "error", err)
		return
	}

	byCID := map[string]PodMeta{}
	for _, pod := range pods {
		for _, cid := range pod.cids {
			byCID[cid] = pod.meta
		}
	}

	s.mu.Lock()
	s.byCID = byCID
	s.mu.Unlock()
}

// PodByContainerID returns the pod owning the given container ID, or nil when
// the container is unknown.
func (s *PodStore) PodByContainerID(cid string) *PodMeta {
	s.mu.RLock()
	defer s.mu.RUnlock()

	meta, ok := s.byCID[cid]
	if !ok {
		return nil
	}
	return &meta
}

// kubeClient is a minimal in-cluster Kubernetes API client that only lists
// pods. The bearer token is re-read on every request to survive rotation.
type kubeClient struct {
	node      string
	base      string
	tokenPath string
	http      *http.Client
}

func newKubeClient(node string) (*kubeClient, error) {
	host := os.Getenv("KUBERNETES_SERVICE_HOST")
	port := os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, fmt.Errorf("KUBERNETES_SERVICE_HOST and KUBERNETES_SERVICE_PORT not set")
	}

	tokenPath := serviceAccountDir + "/token"
	if _, err := os.Stat(tokenPath); err != nil {
		return nil, fmt.Errorf("service account token: %w", err)
	}

	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	caData, err := os.ReadFile(serviceAccountDir + "/ca.crt")
	if err != nil {
		return nil, fmt.Errorf("service account CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caData) {
		return nil, fmt.Errorf("service account CA: no certificates found")
	}
	tlsConfig.RootCAs = pool

	return &kubeClient{
		node:      node,
		base:      "https://" + net.JoinHostPort(host, port),
		tokenPath: tokenPath,
		http: &http.Client{
			Transport: &http.Transport{TLSClientConfig: tlsConfig},
		},
	}, nil
}

type podMetaItem struct {
	meta PodMeta
	cids []string
}

// listPods returns the pods scheduled on the client's node.
func (c *kubeClient) listPods(ctx context.Context) ([]podMetaItem, error) {
	var (
		items         []podMetaItem
		continueToken string
	)
	for page := 0; page < maxPodListPages; page++ {
		list, err := c.listPage(ctx, continueToken)
		if err != nil {
			return nil, err
		}
		for _, item := range list.Items {
			items = append(items, podMetaFromItem(item))
		}
		if list.Metadata.Continue == "" {
			break
		}
		continueToken = list.Metadata.Continue
	}
	return items, nil
}

type podListResponse struct {
	Metadata struct {
		Continue string `json:"continue"`
	} `json:"metadata"`
	Items []podItem `json:"items"`
}

type podItem struct {
	Metadata struct {
		Name            string `json:"name"`
		OwnerReferences []struct {
			Kind string `json:"kind"`
			Name string `json:"name"`
		} `json:"ownerReferences"`
	} `json:"metadata"`
	Status struct {
		ContainerStatuses     []containerStatus `json:"containerStatuses"`
		InitContainerStatuses []containerStatus `json:"initContainerStatuses"`
	} `json:"status"`
}

type containerStatus struct {
	ContainerID string `json:"containerID"`
}

func (c *kubeClient) listPage(ctx context.Context, continueToken string) (*podListResponse, error) {
	reqURL := c.base + "/api/v1/pods?fieldSelector=" +
		url.QueryEscape("spec.nodeName="+c.node) +
		fmt.Sprintf("&limit=%d", podListLimit)
	if continueToken != "" {
		reqURL += "&continue=" + url.QueryEscape(continueToken)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}
	token, err := os.ReadFile(c.tokenPath)
	if err != nil {
		return nil, fmt.Errorf("reading service account token: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("listing pods: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var list podListResponse
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, fmt.Errorf("decoding pod list: %w", err)
	}
	return &list, nil
}

// podMetaFromItem extracts the pod name, the owning job name and the
// container IDs of a pod list item.
func podMetaFromItem(item podItem) podMetaItem {
	meta := PodMeta{Name: item.Metadata.Name}
	for _, owner := range item.Metadata.OwnerReferences {
		if owner.Kind == "Job" {
			meta.JobName = owner.Name
			break
		}
	}

	var cids []string
	for _, status := range item.Status.ContainerStatuses {
		if cid := runtimeContainerID(status.ContainerID); cid != "" {
			cids = append(cids, cid)
		}
	}
	for _, status := range item.Status.InitContainerStatuses {
		if cid := runtimeContainerID(status.ContainerID); cid != "" {
			cids = append(cids, cid)
		}
	}
	return podMetaItem{meta: meta, cids: cids}
}

// runtimeContainerID extracts the container ID from a container runtime
// reference such as "containerd://<id>", in the form used by /proc cgroups.
func runtimeContainerID(statusID string) string {
	_, id, ok := strings.Cut(statusID, "://")
	if !ok {
		return ""
	}
	id = strings.ToLower(strings.TrimSpace(id))
	if len(id) != containerIDHexLen {
		return ""
	}
	return id
}
