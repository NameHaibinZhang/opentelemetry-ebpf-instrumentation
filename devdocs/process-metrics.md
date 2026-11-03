# Process metrics

Enable `metrics.features: [process]` (or add `process` alongside other desired
features) to collect CPU, memory and disk metrics for attached processes. The
feature is disabled by default and is excluded from `all` and `*`. It can also
be selected through a service's `metrics.features` configuration. At least one
metrics exporter must be enabled.

The collector samples `/proc` every five seconds and exports:

| Metric | Instrument | Unit | Required dimensions |
| --- | --- | --- | --- |
| `process.cpu.time` | Counter | `s` | `cpu.mode=user,system` |
| `process.cpu.utilization` | Gauge | `1` | `cpu.mode=user,system` |
| `process.memory.usage` | UpDownCounter | `By` | None |
| `process.memory.virtual` | UpDownCounter | `By` | None |
| `process.disk.io` | Counter | `By` | `disk.io.direction=read,write` |

Prometheus exports memory values as gauges. CPU and disk counters accumulate
changes observed after their first successful sample; the first sample establishes
the baseline and reports current memory usage. Processes that exit before their
first sample do not produce metrics. A failed disk read does not establish a
baseline or advance an existing one.

Every metric retains `process.pid` and its required dimensions regardless of
attribute selection, so process removal can clean up its series independently.
`process.command` is selected by default. `process.executable.path`,
`process.parent_pid` and `process.owner` can be enabled with `attributes.select`
for each metric. The owner is resolved using the target's `/etc/passwd`; it is
omitted when a username cannot be resolved. OTLP includes service and Kubernetes
metadata on the resource. Prometheus includes the selected application metadata
as labels. Process termination and PID reuse remove the old process's series.

`process.network.io` is intentionally absent: `/proc/<pid>/net/dev` reports
network namespace totals, shared by processes in a Kubernetes Pod, rather than
per-process traffic. Exporting those totals per PID would duplicate Pod network
traffic. True process network accounting requires a separate implementation.
