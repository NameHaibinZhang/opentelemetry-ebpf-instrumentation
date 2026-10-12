// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"testing"
)

func TestGPUMonitorMode_UnmarshalText(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    GPUMonitorMode
		wantErr bool
	}{
		{
			name:  "on",
			input: "on",
			want:  GPUMonitorModeOn,
		},
		{
			name:  "off",
			input: "off",
			want:  GPUMonitorModeOff,
		},
		{
			name:  "auto",
			input: "auto",
			want:  GPUMonitorModeAuto,
		},
		{
			name:  "on with spaces",
			input: "  on  ",
			want:  GPUMonitorModeOn,
		},
		{
			name:  "off with spaces",
			input: " off ",
			want:  GPUMonitorModeOff,
		},
		{
			name:  "auto with spaces",
			input: "	auto	",
			want:  GPUMonitorModeAuto,
		},
		{
			name:    "invalid value",
			input:   "invalid",
			wantErr: true,
		},
		{
			name:    "empty string",
			input:   "",
			wantErr: true,
		},
		{
			name:    "uppercase",
			input:   "ON",
			wantErr: true,
		},
		{
			name:    "mixed case",
			input:   "Auto",
			wantErr: true,
		},
		{
			name:    "numeric",
			input:   "1",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got GPUMonitorMode
			err := got.UnmarshalText([]byte(tt.input))

			if (err != nil) != tt.wantErr {
				t.Errorf("UnmarshalText() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if !tt.wantErr && got != tt.want {
				t.Errorf("UnmarshalText() got = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGPUMonitorMode_MarshalText(t *testing.T) {
	tests := []struct {
		name    string
		mode    GPUMonitorMode
		want    string
		wantErr bool
	}{
		{
			name: "on",
			mode: GPUMonitorModeOn,
			want: "on",
		},
		{
			name: "off",
			mode: GPUMonitorModeOff,
			want: "off",
		},
		{
			name: "auto",
			mode: GPUMonitorModeAuto,
			want: "auto",
		},
		{
			name:    "zero value",
			mode:    GPUMonitorMode(0),
			wantErr: true,
		},
		{
			name:    "out of range",
			mode:    GPUMonitorMode(4),
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.mode.MarshalText()

			if (err != nil) != tt.wantErr {
				t.Errorf("MarshalText() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if !tt.wantErr && string(got) != tt.want {
				t.Errorf("MarshalText() got = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestGPUMonitorMode_Valid(t *testing.T) {
	tests := []struct {
		name string
		mode GPUMonitorMode
		want bool
	}{
		{"on", GPUMonitorModeOn, true},
		{"off", GPUMonitorModeOff, true},
		{"auto", GPUMonitorModeAuto, true},
		{"zero value", GPUMonitorMode(0), false},
		{"out of range", GPUMonitorMode(4), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.mode.Valid(); got != tt.want {
				t.Errorf("Valid() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGPUMonitorMode_RoundTrip(t *testing.T) {
	for _, mode := range []GPUMonitorMode{GPUMonitorModeAuto, GPUMonitorModeOn, GPUMonitorModeOff} {
		text, err := mode.MarshalText()
		if err != nil {
			t.Fatalf("MarshalText() for %d: %v", mode, err)
		}

		var got GPUMonitorMode
		if err := got.UnmarshalText(text); err != nil {
			t.Fatalf("UnmarshalText(%q): %v", text, err)
		}

		if got != mode {
			t.Errorf("round trip of %d via %q got %d", mode, text, got)
		}
	}
}
