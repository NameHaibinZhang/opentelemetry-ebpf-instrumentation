// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package gpumon

import (
	"log/slog"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

// EnvResourceAttributes is the standard OpenTelemetry variable holding static
// attributes as a comma-separated k=v list (e.g.
// "deployment.environment=prod,team=pay"), wired from the
// gpuMonitor.extraLabels chart value. gpu-monitor reports every pair as a
// label on each metric it exports, on both the Prometheus endpoint and the
// OTLP attributes. Keys are normalized to valid label names first, so
// deployment.environment is reported as deployment_environment everywhere.
const EnvResourceAttributes = "OTEL_RESOURCE_ATTRIBUTES"

// ExtraLabel is one static key=value label pair reported on every metric.
type ExtraLabel struct {
	Key   string
	Value string
}

// ExtraLabels is the ordered set of static labels, sorted by key so that
// series identity is deterministic across restarts.
type ExtraLabels []ExtraLabel

// labelKeyInvalidChars covers everything a label name may not contain.
var labelKeyInvalidChars = regexp.MustCompile(`[^a-zA-Z0-9_]`)

// ParseExtraLabels parses the OTEL_RESOURCE_ATTRIBUTES syntax: entries split
// on commas, each key=value on the first "=". Values are percent-decoded per
// the OpenTelemetry resource specification. Keys are normalized to valid
// label names: every character outside [a-zA-Z0-9_] becomes "_" and a leading
// digit is prefixed with "_" (deployment.environment becomes
// deployment_environment); collisions after renaming keep the last value.
// Entries without "=" or with an empty key are skipped with a warning instead
// of failing the agent; the last occurrence of a duplicated key wins. An
// empty input yields nil.
func ParseExtraLabels(raw string) ExtraLabels {
	if strings.TrimSpace(raw) == "" {
		return nil
	}

	byKey := map[string]string{}
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		key, value, found := strings.Cut(entry, "=")
		if !found || strings.TrimSpace(key) == "" {
			slog.Warn("ignoring invalid resource attribute entry", "env", EnvResourceAttributes, "entry", entry)
			continue
		}
		if decoded, err := url.PathUnescape(strings.TrimSpace(value)); err == nil {
			value = decoded
		}
		byKey[sanitizeLabelKey(strings.TrimSpace(key))] = strings.TrimSpace(value)
	}
	if len(byKey) == 0 {
		return nil
	}

	labels := make(ExtraLabels, 0, len(byKey))
	for key, value := range byKey {
		labels = append(labels, ExtraLabel{Key: key, Value: value})
	}
	sort.Slice(labels, func(i, j int) bool { return labels[i].Key < labels[j].Key })
	return labels
}

// sanitizeLabelKey rewrites a key to a valid label name, warning when the key
// changes so misconfigurations stay visible in the agent log.
func sanitizeLabelKey(key string) string {
	name := labelKeyInvalidChars.ReplaceAllString(key, "_")
	if name != "" && name[0] >= '0' && name[0] <= '9' {
		name = "_" + name
	}
	if name != key {
		slog.Warn("normalizing label key", "key", key, "label", name)
	}
	return name
}

// keys returns the label names in order.
func (l ExtraLabels) keys() []string {
	keys := make([]string, 0, len(l))
	for _, pair := range l {
		keys = append(keys, pair.Key)
	}
	return keys
}

// values returns the label values in order, to be appended after the
// per-metric values of the same length.
func (l ExtraLabels) values() []string {
	values := make([]string, 0, len(l))
	for _, pair := range l {
		values = append(values, pair.Value)
	}
	return values
}
