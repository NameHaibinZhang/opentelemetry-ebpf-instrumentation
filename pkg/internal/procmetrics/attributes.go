// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package procmetrics // import "go.opentelemetry.io/obi/pkg/internal/procmetrics"

import (
	"go.opentelemetry.io/otel/attribute"

	"go.opentelemetry.io/obi/pkg/export/attributes"
	attr "go.opentelemetry.io/obi/pkg/export/attributes/names"
)

// OTELGetters resolves selectable process attributes; service metadata remains
// on the OTLP resource. Exporters add
// process.pid and the required metric dimensions independently of selection so
// each process can be removed without affecting another process's series.
func OTELGetters(name attr.Name) (attributes.Getter[*Snapshot, attribute.KeyValue], bool) {
	switch name {
	case attr.ProcessCommand, attr.ProcessExecutablePath, attr.ProcessParentPID, attr.ProcessOwner:
	default:
		return nil, false
	}
	return func(s *Snapshot) attribute.KeyValue {
		if name == attr.ProcessParentPID {
			return attribute.Int64(string(name), int64(s.ParentPID))
		}

		var value string
		switch name {
		case attr.ProcessCommand:
			value = s.Command
		case attr.ProcessExecutablePath:
			value = s.ExecutablePath
		case attr.ProcessOwner:
			value = s.Owner
		}
		if value == "" {
			return attribute.KeyValue{}
		}
		return attribute.String(string(name), attributes.SanitizeUTF8(value))
	}, true
}

func PromGetters(name attr.Name) (attributes.Getter[*Snapshot, string], bool) {
	get, ok := OTELGetters(name)
	if name == attr.ProcessPID || name == attr.CPUMode || name == attr.DiskIODirection {
		return nil, false
	}
	return func(s *Snapshot) string {
		if ok {
			if kv := get(s); kv.Valid() {
				return kv.Value.Emit()
			}
			return ""
		}
		switch name {
		case attr.ServiceName:
			return s.Service.UID.Name
		case attr.ServiceNamespace:
			return s.Service.UID.Namespace
		case attr.Instance:
			return s.Service.UID.Instance
		case attr.Job:
			return s.Service.Job()
		case attr.HostName:
			return s.Service.HostName
		default:
			return s.Service.Metadata[name]
		}
	}, true
}
