// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package gpumon

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	otherContainerIDFixture = "ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12"
	thirdContainerIDFixture = "cd34cd34cd34cd34cd34cd34cd34cd34cd34cd34cd34cd34cd34cd34cd34cd34"
)

func TestRuntimeContainerID(t *testing.T) {
	for name, tc := range map[string]struct {
		statusID string
		want     string
	}{
		"containerd":    {statusID: "containerd://" + containerIDFixture, want: containerIDFixture},
		"docker":        {statusID: "docker://" + containerIDFixture, want: containerIDFixture},
		"uppercase":     {statusID: "containerd://" + "AB12AB12AB12AB12AB12AB12AB12AB12AB12AB12AB12AB12AB12AB12AB12AB12", want: otherContainerIDFixture},
		"no scheme":     {statusID: containerIDFixture, want: ""},
		"short id":      {statusID: "containerd://abc123", want: ""},
		"empty":         {statusID: "", want: ""},
		"scheme only":   {statusID: "containerd://", want: ""},
		"trailing junk": {statusID: "containerd://" + containerIDFixture + "x", want: ""},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, runtimeContainerID(tc.statusID))
		})
	}
}

func TestPodMetaFromItem(t *testing.T) {
	item := podItem{}
	item.Metadata.Name = "trainer-0"
	item.Metadata.OwnerReferences = []struct {
		Kind string `json:"kind"`
		Name string `json:"name"`
	}{
		{Kind: "ReplicaSet", Name: "rs-1"},
		{Kind: "Job", Name: "job-1"},
	}
	item.Status.ContainerStatuses = []containerStatus{
		{ContainerID: "containerd://" + containerIDFixture},
		{ContainerID: "not-a-container-id"},
	}
	item.Status.InitContainerStatuses = []containerStatus{
		{ContainerID: "docker://" + otherContainerIDFixture},
	}

	got := podMetaFromItem(item)
	assert.Equal(t, PodMeta{Name: "trainer-0", JobName: "job-1"}, got.meta)
	assert.Equal(t, []string{containerIDFixture, otherContainerIDFixture}, got.cids)
}

func TestPodMetaFromItemWithoutJob(t *testing.T) {
	item := podItem{}
	item.Metadata.Name = "standalone"
	got := podMetaFromItem(item)
	assert.Equal(t, PodMeta{Name: "standalone"}, got.meta)
	assert.Empty(t, got.cids)
}

func writeToken(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(path, []byte("test-token\n"), 0o600))
	return path
}

// podListServer serves two pages of a pod list and records the queries it
// received.
func podListServer(t *testing.T, queries *[]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*queries = append(*queries, r.URL.RawQuery)
		assert.Equal(t, "/api/v1/pods", r.URL.Path)
		assert.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))

		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("continue") == "" {
			fmt.Fprintf(w, `{"metadata":{"continue":"next-page"},"items":[
				{"metadata":{"name":"pod-1","ownerReferences":[{"kind":"Job","name":"job-1"}]},
				 "status":{"containerStatuses":[{"containerID":"containerd://%s"}]}}]}`, containerIDFixture)
			return
		}
		fmt.Fprintf(w, `{"metadata":{"continue":""},"items":[
			{"metadata":{"name":"pod-2"},
			 "status":{"containerStatuses":[{"containerID":"docker://%s"}],
			           "initContainerStatuses":[{"containerID":"containerd://%s"}]}}]}`,
			otherContainerIDFixture, thirdContainerIDFixture)
	}))
}

func TestKubeClientListPods(t *testing.T) {
	var queries []string
	server := podListServer(t, &queries)
	defer server.Close()

	client := &kubeClient{
		node:      "node1",
		base:      server.URL,
		tokenPath: writeToken(t),
		http:      server.Client(),
	}

	items, err := client.listPods(context.Background())
	require.NoError(t, err)
	require.Len(t, items, 2)

	assert.Equal(t, PodMeta{Name: "pod-1", JobName: "job-1"}, items[0].meta)
	assert.Equal(t, []string{containerIDFixture}, items[0].cids)
	assert.Equal(t, PodMeta{Name: "pod-2"}, items[1].meta)
	assert.Equal(t, []string{otherContainerIDFixture, thirdContainerIDFixture}, items[1].cids)

	require.Len(t, queries, 2)
	assert.Contains(t, queries[0], "fieldSelector=spec.nodeName%3Dnode1")
	assert.Contains(t, queries[0], "limit=500")
	assert.Contains(t, queries[1], "continue=next-page")
}

func TestKubeClientListPodsHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer server.Close()

	client := &kubeClient{
		node:      "node1",
		base:      server.URL,
		tokenPath: writeToken(t),
		http:      server.Client(),
	}

	_, err := client.listPods(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "403")
}

func TestPodStoreRefreshIndexesByContainerID(t *testing.T) {
	var queries []string
	server := podListServer(t, &queries)
	defer server.Close()

	store := &PodStore{
		client: &kubeClient{
			node:      "node1",
			base:      server.URL,
			tokenPath: writeToken(t),
			http:      server.Client(),
		},
		log:   slog.Default(),
		byCID: map[string]PodMeta{},
	}

	store.refresh(context.Background())

	pod := store.PodByContainerID(containerIDFixture)
	require.NotNil(t, pod)
	assert.Equal(t, PodMeta{Name: "pod-1", JobName: "job-1"}, *pod)

	pod = store.PodByContainerID(otherContainerIDFixture)
	require.NotNil(t, pod)
	assert.Equal(t, PodMeta{Name: "pod-2"}, *pod)

	pod = store.PodByContainerID(thirdContainerIDFixture)
	require.NotNil(t, pod)
	assert.Equal(t, PodMeta{Name: "pod-2"}, *pod)

	assert.Nil(t, store.PodByContainerID("unknown"))
}

func TestPodStoreRefreshFailureKeepsPreviousIndex(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer server.Close()

	store := &PodStore{
		client: &kubeClient{
			node:      "node1",
			base:      server.URL,
			tokenPath: writeToken(t),
			http:      server.Client(),
		},
		log:   slog.Default(),
		byCID: map[string]PodMeta{containerIDFixture: {Name: "kept"}},
	}

	store.refresh(context.Background())

	pod := store.PodByContainerID(containerIDFixture)
	require.NotNil(t, pod)
	assert.Equal(t, "kept", pod.Name)
}

func TestNewPodStoreWithoutNodeName(t *testing.T) {
	t.Setenv("NODE_NAME", "")
	assert.Nil(t, NewPodStore())
}
