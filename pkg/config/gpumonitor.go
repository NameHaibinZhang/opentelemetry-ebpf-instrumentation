// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package config // import "go.opentelemetry.io/obi/pkg/config"

import (
	"fmt"
	"strings"
)

type GPUMonitorMode uint8

const (
	GPUMonitorModeAuto = GPUMonitorMode(iota + 1)
	GPUMonitorModeOn
	GPUMonitorModeOff
)

func (b *GPUMonitorMode) UnmarshalText(text []byte) error {
	switch strings.TrimSpace(string(text)) {
	case "on":
		*b = GPUMonitorModeOn
		return nil
	case "off":
		*b = GPUMonitorModeOff
		return nil
	case "auto":
		*b = GPUMonitorModeAuto
		return nil
	}

	return fmt.Errorf("invalid GPU monitoring mode value: '%s'", text)
}

func (b GPUMonitorMode) MarshalText() ([]byte, error) {
	switch b {
	case GPUMonitorModeOn:
		return []byte("on"), nil
	case GPUMonitorModeOff:
		return []byte("off"), nil
	case GPUMonitorModeAuto:
		return []byte("auto"), nil
	}

	return nil, fmt.Errorf("invalid GPU monitoring mode value: %d", b)
}

func (b GPUMonitorMode) Valid() bool {
	switch b {
	case GPUMonitorModeOn, GPUMonitorModeOff, GPUMonitorModeAuto:
		return true
	}

	return false
}
