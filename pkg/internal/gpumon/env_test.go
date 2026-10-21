// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package gpumon

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDisabledByEnv(t *testing.T) {
	cases := []struct {
		value  string
		want   bool
		setEnv bool
	}{
		{value: "", want: false, setEnv: false},
		{value: "", want: false, setEnv: true},
		{value: "false", want: false, setEnv: true},
		{value: "0", want: false, setEnv: true},
		{value: "true", want: true, setEnv: true},
		{value: "1", want: true, setEnv: true},
		{value: " true ", want: true, setEnv: true},
		{value: "TRUE", want: true, setEnv: true},
		{value: "banana", want: false, setEnv: true},
	}
	for _, c := range cases {
		if c.setEnv {
			t.Setenv(EnvDisabled, c.value)
		}
		assert.Equal(t, c.want, DisabledByEnv(), "GPU_MONITOR_DISABLED=%q", c.value)
	}
}
