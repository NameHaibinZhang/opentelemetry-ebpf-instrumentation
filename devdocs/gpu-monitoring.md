# GPU monitoring

OBI observes NVIDIA GPUs in two complementary ways:

- **CUDA API instrumentation** (`ebpf.instrument_cuda`): uprobes on the CUDA
  Runtime and Driver report the GPU work an application performs — kernel
  launches, memory copies, allocations, stream and event operations — as spans
  and `gpu.cuda.*` metrics. This is execution telemetry about the
  application's GPU work.
- **Device-level monitoring** (`gpu` config section): a polling monitor reads
  the GPU inventory of the node from sysfs, procfs and `nvidia-smi`, and
  reports `obi.gpu.*` metrics — identity, PCIe link state, NUMA affinity,
  NVLink topology, the processes holding each GPU, and PCIe AER errors. This
  complements the CUDA telemetry with the state of the GPUs themselves.

Neither subsystem replaces device health telemetry such as utilization,
temperature, or power, which comes from tools like `nvidia-smi` or DCGM. The
`gpu.uuid` attribute of the device-level metrics uses the same bare UUID as
DCGM's `UUID` label, so series from both stacks can be joined.

## Enabling

CUDA API instrumentation is off unless enabled through `ebpf.instrument_cuda`
(`OTEL_EBPF_INSTRUMENT_CUDA`):

| Value          | Behavior                                      |
|:---------------|:----------------------------------------------|
| `auto` (default) | Enabled when `nvidia-smi` is on the `PATH` of the OBI process. |
| `on`           | Always enabled.                               |
| `off`          | Always disabled.                              |

When enabled, `newCommonTracersGroup` in `pkg/appolly/discover/finder.go` adds
the gpuevent tracer to the common tracer set. The tracer is not required: if it
fails to load, the rest of the instrumentation still works.

Device-level monitoring is configured independently through the `gpu` section:

```yaml
gpu:
  mode: auto        # on | off | auto
  interval: 30s
  prometheus:
    port: 0         # standalone Prometheus endpoint, 0 disables it
    path: /metrics
```

In `auto` mode (the default) the monitor starts only when the GPU inventory is
non-empty; `on` logs a warning when no GPUs are found but keeps running. The
metrics go to the configured OTLP metrics exporter, and additionally to the
standalone Prometheus endpoint when `gpu.prometheus.port` is set. The two
subsystems are independent: device-level metrics are collected even when no
instrumented application uses the GPUs.

## Where the code lives

| Piece                         | Location                                        |
|:------------------------------|:------------------------------------------------|
| eBPF programs and maps        | `bpf/gpuevent/cuda.c`                           |
| Event structs shared with Go  | `bpf/gpuevent/cuda.h`                           |
| Ring buffer definition        | `bpf/gpuevent/gpu_ringbuf.h`                    |
| Go tracer (probes, decoding)  | `pkg/internal/ebpf/gpuevent/gpuevent.go`        |
| Span types and fields         | `pkg/appolly/app/request/span.go`               |
| OTLP metric emitters          | `pkg/export/otel/metrics.go`                    |
| Prometheus metric emitters    | `pkg/export/prom/prom.go`                       |
| Schema registry               | `schemas/obi/groups/gpu/`                       |
| Integration test              | `internal/test/integration/gpu_metrics_test.go` |

The eBPF program is derived from the `gpuevent_snoop` program in
[strobelight](https://github.com/facebookincubator/strobelight); the header of
`bpf/gpuevent/cuda.c` records the upstream revision.

## Instrumented APIs

OBI attaches uprobes to the CUDA Runtime (`libcudart.so`) and the CUDA Driver
(`libcuda.so`). Calls whose arguments need to be correlated with a result use
entry and return probes; the rest use a single entry probe.

| Library     | Probes          | Symbols |
|:------------|:----------------|:--------|
| `libcudart` | entry + return  | cudaLaunchKernel, cudaGraphLaunch, cudaMalloc, cudaFree, cudaSetDevice, cudaGetDevice, cudaGetDeviceProperties, cudaGetDeviceProperties_v2 |
| `libcudart` | entry           | cudaMemcpy, cudaMemcpyAsync, cudaMemset, cudaStreamCreate, cudaStreamCreateWithFlags, cudaStreamCreateWithPriority, cudaStreamDestroy, cudaEventRecord, cudaEventRecordWithFlags, cudaEventSynchronize, cudaStreamSynchronize, cudaDeviceSynchronize, cudaHostRegister |
| `libcuda`   | entry + return  | cuDeviceGetUuid, cuDeviceGetUuid_v2, cuDeviceGetName |
| `libcuda`   | entry           | cuLaunchKernel, cuLaunchKernelEx, cuGraphLaunch |

Return probes exist where the outcome only becomes known after the call:
`cudaMalloc` reports the allocated pointer and size, `cudaFree` confirms the
release, `cudaSetDevice` confirms which device is now current, and the
introspection calls read the buffers that the callee filled in. Driver API
launches are entry-only because deduplication against the runtime API is done
with a per-thread marker, not with a return value.

## Events and the ring buffer

The eBPF programs push fixed-size events into `gpu_events`, a 64 KB ring
buffer. `processCudaEvent` in `pkg/internal/ebpf/gpuevent/gpuevent.go` decodes
them into spans.

| Event                        | Span type                             | Metric |
|:-----------------------------|:--------------------------------------|:-------|
| `k_event_kernel_launch`      | `EventTypeGPUCudaKernelLaunch`        | `gpu.cuda.kernel.launch.calls`, `gpu.cuda.kernel.grid.size`, `gpu.cuda.kernel.block.size` |
| `k_event_graph_launch`       | `EventTypeGPUCudaGraphLaunch`         | `gpu.cuda.graph.launch.calls` |
| `k_event_malloc`             | `EventTypeGPUCudaMalloc`              | `gpu.cuda.memory.allocations` |
| `k_event_memcpy`             | `EventTypeGPUCudaMemcpy`              | `gpu.cuda.memory.copies` |
| `k_event_free`               | `EventTypeGPUCudaFree`                | `gpu.cuda.memory.free.bytes` |
| `k_event_memset`             | `EventTypeGPUCudaMemset`              | `gpu.cuda.memset.bytes` |
| `k_event_stream_create`      | `EventTypeGPUCudaStreamCreate`        | `gpu.cuda.stream.create.calls` |
| `k_event_stream_destroy`     | `EventTypeGPUCudaStreamDestroy`       | `gpu.cuda.stream.destroy.calls` |
| `k_event_event_record`       | `EventTypeGPUCudaEventRecord`         | `gpu.cuda.event.record.calls` |
| `k_event_event_synchronize`  | `EventTypeGPUCudaEventSynchronize`    | `gpu.cuda.event.synchronize.calls` |
| `k_event_stream_synchronize` | `EventTypeGPUCudaStreamSynchronize`   | `gpu.cuda.stream.synchronize.calls` |
| `k_event_device_synchronize` | `EventTypeGPUCudaDeviceSynchronize`   | `gpu.cuda.device.synchronize.calls` |
| `k_event_host_register`      | `EventTypeGPUCudaHostRegister`        | `gpu.cuda.host.register.bytes` |
| `k_event_device_info`        | —                                     | — |

`k_event_device_info` carries the device UUID and model that an introspection
call revealed. It produces no span; the Go tracer only caches the pair for
labelling later events.

## Device attribution

Spans and metrics are labelled with the device the calling thread was bound to
when that binding was observed:

| Attribute           | Meaning |
|:--------------------|:--------|
| `cuda.device.index` | Process-local device index. Present when the thread selected or queried a device through `cudaSetDevice` or `cudaGetDevice`; omitted when no binding was observed. |
| `cuda.device.uuid`  | Bare device UUID, the `uuid` column of `nvidia-smi` with its `GPU-` prefix removed. Omitted until the process asked CUDA about the device. |
| `cuda.device.model` | Device name, for example `NVIDIA H20-3e`. Omitted until observed. |

The thread-to-device binding is learned from `cudaSetDevice` and `cudaGetDevice`
and kept per host thread, because CUDA's current device is per host thread. A
failed `cudaSetDevice` does not bind the thread, and a `cudaGetDevice` query does
not change it. Identity is cached per process, and partial observations merge:
an observation that reveals only the UUID or only the name does not clear the
other part.

Because `CUDA_VISIBLE_DEVICES` remaps indices, the same index can refer to
different physical GPUs in different processes; the UUID identifies the
physical device.

## Runtime and driver deduplication

`libcudart` implements the launch APIs by calling into `libcuda`, so a process
that maps both libraries would report the same launch twice. A per-thread
in-flight marker is set at the runtime entry probe and consumed by the driver
entry probes, which then report nothing. The runtime return probes clear the
marker for calls that never reach the driver.

## Byte-accurate free accounting

`cudaFree` is not told how large the freed block is, so frees are accounted
using the size recorded at allocation time. The `cudaMalloc` return probe
stores the returned pointer and size, keyed by process and device pointer. The
`cudaFree` probes look the size up and report it once the return probe confirms
the free.

## eBPF maps

Maps are pinned internal to OBI. The per-thread in-flight context maps are
regular hash maps: their matching return probes consume and delete entries
unconditionally, so a deselected or exiting target cannot leak them. The
long-lived state maps (`cuda_alloc_sizes`, `cuda_thread_device`,
`cuda_device_info`) are LRU hashes so entries for processes that exit without
cleaning up are evicted rather than growing without bound.

| Map                       | Type      | Key → value                | Entries | Purpose |
|:--------------------------|:----------|:---------------------------|--------:|:--------|
| `cuda_malloc_ctx`         | hash      | thread → malloc arguments  | 1024    | Correlate `cudaMalloc` arguments with its return value. |
| `cuda_free_ctx`           | hash      | thread → free arguments    | 1024    | Report a free only once its return code confirms it. |
| `cuda_alloc_sizes`        | LRU hash  | (process, pointer) → size  | 65536   | Size lookup for `cudaFree`. |
| `cuda_runtime_launch_ctx` | hash      | thread → marker            | 1024    | Suppress driver launches that a runtime call is proxying. |
| `cuda_thread_device`      | LRU hash  | thread → device index      | 8192    | Current device of each host thread. |
| `cuda_device_info`        | LRU hash  | (process, index) → identity | 4096   | UUID and name revealed by introspection. |
| `cuda_introspect_ctx`     | hash      | thread → context           | 1024    | Carry introspection arguments from entry to return. |
| `cuda_set_device_ctx`     | hash      | thread → context           | 1024    | Bind a thread only after `cudaSetDevice` succeeds. |

## Span fields

GPU span types reuse the generic `Span` fields instead of adding per-call data
to the span:

| Span                              | `ContentLength`          | `SubType`                  |
|:----------------------------------|:-------------------------|:---------------------------|
| kernel launch                     | grid.x × grid.y × grid.z | block.x × block.y × block.z |
| memcpy                            | bytes copied             | `cudaMemcpyKind`           |
| malloc, free, memset, host register | bytes                  | —                          |
| graph launch, stream/event/sync calls | —                    | —                          |

The spans expose these values as `gridSize`, `blockSize`, `size`, and `kind`
span attributes, built by `spanAttributes` in
`pkg/appolly/app/request/span.go`.

## Metrics

GPU metrics are `development` stability and declared in
`schemas/obi/groups/gpu/metrics.yaml`. They carry `cuda.device.index`,
`cuda.device.uuid`, and `cuda.device.model` when the device binding and identity
were observed; `gpu.cuda.memory.copies` also carries `cuda.memcpy.kind`. An em
dash in the unit column means the metric is declared without a unit, a pure
count.

| Metric                             | Instrument | Unit |
|:-----------------------------------|:-----------|:-----|
| `gpu.cuda.kernel.launch.calls`     | counter    | —    |
| `gpu.cuda.kernel.grid.size`        | histogram  | 1    |
| `gpu.cuda.kernel.block.size`       | histogram  | 1    |
| `gpu.cuda.graph.launch.calls`      | counter    | —    |
| `gpu.cuda.memory.allocations`      | counter    | By   |
| `gpu.cuda.memory.copies`           | histogram  | By   |
| `gpu.cuda.memory.free.bytes`       | counter    | By   |
| `gpu.cuda.memset.bytes`            | counter    | By   |
| `gpu.cuda.stream.create.calls`     | counter    | —    |
| `gpu.cuda.stream.destroy.calls`    | counter    | —    |
| `gpu.cuda.event.record.calls`      | counter    | —    |
| `gpu.cuda.event.synchronize.calls` | counter    | —    |
| `gpu.cuda.stream.synchronize.calls` | counter   | —    |
| `gpu.cuda.device.synchronize.calls` | counter   | —    |
| `gpu.cuda.host.register.bytes`     | counter    | By   |

The registry is checked against the emitting surface: the GPU integration test
runs weaver in enforce mode, so a metric that is emitted but not declared, or
declared but not emitted, fails the test.

The OTLP exporter emits these metrics in `pkg/export/otel/metrics.go`, and the
Prometheus exporter mirrors them in `pkg/export/prom/prom.go`, both gated by
the GPU instrumentation being enabled.

### Adding a new GPU metric

1. Add the value to the span in `pkg/appolly/app/request/span.go` (and a
   getter for it if the metric needs a new attribute value). If a new event is
   needed, add the C struct to `bpf/gpuevent/cuda.h`, the event to
   `bpf/gpuevent/cuda.c`, then regenerate the bpf2go bindings with
   `make generate` (or `make docker-generate` when not on a Linux host).
2. Declare the metric, and any new attribute, in
   `schemas/obi/groups/gpu/metrics.yaml` and
   `schemas/obi/groups/gpu/registry.yaml`.
3. Register the metric and its attributes in
   `pkg/export/attributes/metric.go` and
   `pkg/export/attributes/attr_defs.go`, adding new attribute names to
   `pkg/export/attributes/names/attrs.go`.
4. Emit it in `pkg/export/otel/metrics.go` and mirror the emitter in
   `pkg/export/prom/prom.go`.
5. Run `make generate-schema-docs` to refresh the reference docs under
   `site/docs/`.
6. If the change renames emitted telemetry, record the transformation in
   [telemetry-schema.md](telemetry-schema.md).

## Testing without a GPU

`TestGPUCudaMetrics` in `internal/test/integration/gpu_metrics_test.go` runs
the whole pipeline without NVIDIA hardware. The test builds fake `libcudart.so`
and `libcuda.so.1` libraries from the stub sources in
`internal/test/integration/components/gpu-cuda-tester/`, and
`docker-compose-gpu.yml` forces `OTEL_EBPF_INSTRUMENT_CUDA=on` so the gpuevent
tracer loads.

## Device-level GPU metrics

The `pkg/gpumon` package polls the node's GPU state on a fixed interval and
hands a snapshot to the exporters. It does not use eBPF: everything comes from
files and `nvidia-smi`, so it works on any kernel and in every deployment
where the NVIDIA driver is loaded.

| Piece                              | Location                                  |
|:-----------------------------------|:------------------------------------------|
| Snapshot types and `Reporter`      | `pkg/gpumon/snapshot.go`                  |
| Sources (sysfs, procfs, nvidia-smi)| `pkg/gpumon/sources.go`                   |
| Poll loop, ghosts, AER deltas, conflicts | `pkg/gpumon/agent.go`              |
| Process identity resolution        | `pkg/gpumon/resolve.go`                   |
| OTLP reporter                      | `pkg/export/otel/metrics_gpu.go`          |
| Prometheus reporter                | `pkg/gpumon/prom.go`                      |
| Config                             | `pkg/obi/gpu_cfg.go`                      |
| Instrumenter wiring                | `setupGPUMonitor` in `pkg/instrumenter/instrumenter.go` |

### Collection sources

| Data                  | Source |
|:----------------------|:-------|
| GPU presence, UUID, model, device minor | `/proc/driver/nvidia/gpus/<bus>/information` |
| Device node accessibility (`up`)        | `/dev/nvidia<minor>` |
| MIG capability                          | `nvidia-smi --query-gpu=pci_bus_id,mig.mode.current` |
| NUMA node                               | sysfs `numa_node`, falling back to matching the `nvidia-smi topo -m` CPU affinity against `/sys/devices/system/node/node*/cpulist` |
| PCIe generation and width               | sysfs `current_link_speed` / `current_link_width` |
| NVLink state and peers                  | `nvidia-smi -q -d NVLINK` |
| NVLink rate                             | `nvidia-smi nvlink -gt d` (GB/s, mapped to a generation number) |
| Compute processes                       | `nvidia-smi --query-compute-apps=pid,gpu_uuid` |
| PCIe AER errors                         | sysfs `aer_dev_correctable`, `aer_dev_nonfatal`, `aer_dev_fatal` |

The collection is best-effort: a failing source logs at debug level and the
snapshot carries what could be read. AER counters are cumulative in sysfs, so
the agent turns them into per-interval deltas; the first observation of a
counter only records the baseline.

### Process identity

Each GPU compute process is labelled with what could be resolved:

- `process.executable.name` from `/proc/<pid>/comm`;
- `k8s.pod.name` and `k8s.job.name` from the Kubernetes metadata store, by
  resolving the PID's container ID through the informer;
- `ml.rank` from the `RANK` environment variable set by distributed training
  launchers such as torchrun, kept only when it is a plain integer.

Pod and job resolution requires the Kubernetes metadata to be enabled; without
it the labels are simply omitted.

### Sharing conflicts

`obi.gpu.occupancy.conflict` counts GPU sharing conflicts: on every cycle in
which a GPU is held by processes from at least two distinct workload
identities (pod name, or PID outside Kubernetes), each involved process with
a known pod contributes one increment. A sustained conflict therefore shows up
as a steady rate, which is what alerts should target.

### Disappearing GPUs

A GPU that was present in the previous cycle and is missing from the current
one is reported for one more cycle as a ghost: `obi.gpu.up` drops to 0 and
only the `gpu.uuid` label is set. On the following cycle, without inventory,
the series end: gauge series are removed and the `*_info` updowncounters are
decremented, so exporters that support it drop the series and stale ones age
out with their own TTL.

### Metrics

All metrics are `development` stability, declared in
`schemas/obi/groups/gpu/metrics.yaml`, and labelled with `gpu.uuid` (the bare
UUID matching DCGM's `UUID` label).

| Metric                          | Instrument      | Extra attributes |
|:--------------------------------|:----------------|:-----------------|
| `obi.gpu.info`                  | updowncounter   | `gpu.pci.bus.id`, `hw.model`, `gpu.mig.capable` |
| `obi.gpu.up`                    | gauge           | — |
| `obi.gpu.numa.node`             | gauge           | — |
| `obi.gpu.pcie.generation`       | gauge           | — |
| `obi.gpu.pcie.width`            | gauge           | — |
| `obi.gpu.nvlink.up`             | gauge           | `gpu.nvlink.link.id`, `gpu.peer.uuid` or `gpu.nvlink.switch.id` |
| `obi.gpu.nvlink.rate`           | gauge           | same as `obi.gpu.nvlink.up` |
| `obi.gpu.nvlink.width`          | gauge           | same as `obi.gpu.nvlink.up` |
| `obi.gpu.process.info`          | updowncounter   | `process.pid`, `process.executable.name`, `k8s.pod.name`, `k8s.job.name`, `ml.rank` |
| `obi.gpu.occupancy.conflict`    | counter         | `k8s.pod.name`, `k8s.job.name`, `ml.rank` |
| `obi.gpu.pcie.aer.errors`       | counter         | `gpu.pcie.aer.severity`, `gpu.pci.bus.id` |

The `*_info` metrics are Prometheus-style inventory meta-metrics emitted with
value 1 for each present GPU or process. NVLink series with a direct peer
carry `gpu.peer.uuid`; NVSwitch-routed links carry `gpu.nvlink.switch.id`
instead. `obi.gpu.nvlink.width` is the constant 4 lanes per port of every
NVLink generation the driver reports.

The unit tests in `pkg/gpumon` and `pkg/export/otel/metrics_gpu_test.go` cover
the parsers and the reporter against fixture files, without NVIDIA hardware.
