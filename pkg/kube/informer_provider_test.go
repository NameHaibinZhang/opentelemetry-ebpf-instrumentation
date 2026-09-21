// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package kube

import (
	"context"
	"io"
	"log/slog"
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func nodeNamed(name string, addrs ...corev1.NodeAddress) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status:     corev1.NodeStatus{Addresses: addrs},
	}
}

func localAddrs(t *testing.T, cidrs []string) []net.Addr {
	t.Helper()
	addrs := make([]net.Addr, 0, len(cidrs))
	for _, cidr := range cidrs {
		ip, ipnet, err := net.ParseCIDR(cidr)
		require.NoError(t, err)
		ipnet.IP = ip
		addrs = append(addrs, ipnet)
	}
	return addrs
}

func TestCheckLocalHostNameWithNodeName(t *testing.T) {
	ackNode := nodeNamed("cn-hangzhou.10.160.26.198",
		corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: "10.160.26.198"},
		corev1.NodeAddress{Type: corev1.NodeHostName, Address: "cn-hangzhou.10.160.26.198"},
	)
	otherAckNode := nodeNamed("cn-hangzhou.10.153.135.208",
		corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: "10.153.135.208"},
	)

	testCases := []struct {
		name         string
		nodes        []runtime.Object
		localAddrs   []string
		hostName     string
		expectedName string
	}{
		{
			name:         "exact node name match",
			nodes:        []runtime.Object{nodeNamed("ip-10-0-0-1")},
			hostName:     "ip-10-0-0-1",
			expectedName: "ip-10-0-0-1",
		},
		{
			name:         "unique prefix match",
			nodes:        []runtime.Object{nodeNamed("ip-10-0-0-1.ec2.internal")},
			hostName:     "ip-10-0-0-1",
			expectedName: "ip-10-0-0-1.ec2.internal",
		},
		{
			name:         "host network pod matched by node address",
			nodes:        []runtime.Object{ackNode, otherAckNode},
			localAddrs:   []string{"127.0.0.1/8", "10.160.26.198/24"},
			hostName:     "izbp1653wbtvgbsdhc0ljyz",
			expectedName: "cn-hangzhou.10.160.26.198",
		},
		{
			name:         "unresolvable host name and no matching address",
			nodes:        []runtime.Object{ackNode},
			localAddrs:   []string{"127.0.0.1/8", "10.1.2.3/24"},
			hostName:     "izbp1653wbtvgbsdhc0ljyz",
			expectedName: "izbp1653wbtvgbsdhc0ljyz",
		},
		{
			name: "ambiguous address match",
			nodes: []runtime.Object{ackNode, nodeNamed("duplicate",
				corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: "10.160.26.198"})},
			localAddrs:   []string{"10.160.26.198/24"},
			hostName:     "izbp1653wbtvgbsdhc0ljyz",
			expectedName: "izbp1653wbtvgbsdhc0ljyz",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			restore := interfaceAddrs
			interfaceAddrs = func() ([]net.Addr, error) { return localAddrs(t, tc.localAddrs), nil }
			t.Cleanup(func() { interfaceAddrs = restore })

			nodeName, err := checkLocalHostNameWithNodeName(
				context.Background(), testLogger(), fake.NewClientset(tc.nodes...), tc.hostName)

			require.NoError(t, err)
			assert.Equal(t, tc.expectedName, nodeName)
		})
	}
}

func TestClusterNameFromNodeLabels(t *testing.T) {
	t.Run("ACK cluster label", func(t *testing.T) {
		name, err := clusterNameFromNodeLabels(map[string]string{
			"ack.aliyun.com": "cc9fbbc663966494e8aa531997c12e2f7",
			"unrelated":      "ignored",
		})
		require.NoError(t, err)
		assert.Equal(t, "cc9fbbc663966494e8aa531997c12e2f7", name)
	})

	t.Run("no known cluster label", func(t *testing.T) {
		name, err := clusterNameFromNodeLabels(map[string]string{"unrelated": "ignored"})
		require.Error(t, err)
		assert.Empty(t, name)
	})
}
