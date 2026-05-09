// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package gotracer

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGoProbesIncludesOpenAIChatCompletionServiceNew(t *testing.T) {
	tracer := &Tracer{}

	probes := tracer.GoProbes()
	key := "github.com/openai/openai-go/v3.(*ChatCompletionService).New"

	descs, ok := probes[key]
	require.True(t, ok)
	require.Len(t, descs, 1)

	assert.Nil(t, descs[0].Start)
	assert.Nil(t, descs[0].End)
}
