// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package gpumon

import (
	"os"
	"strconv"
	"strings"
)

// EnvDisabled turns off GPU collection and metric reporting entirely; the
// process then only serves an empty /metrics endpoint so probes stay green.
const EnvDisabled = "GPU_MONITOR_DISABLED"

// DisabledByEnv reports whether GPU_MONITOR_DISABLED holds a truthy value
// ("1", "t", "true", ...). Empty or unparseable values leave monitoring on.
func DisabledByEnv() bool {
	v, err := strconv.ParseBool(strings.TrimSpace(os.Getenv(EnvDisabled)))
	return err == nil && v
}
