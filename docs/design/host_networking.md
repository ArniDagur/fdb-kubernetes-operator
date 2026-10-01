# Host Networking for FoundationDB Pods

## Metadata

* Authors: (to be filled in)
* Created: 2026-09-29
* Updated: 2026-09-30

## Background

Some users want FoundationDB pods to run in the host network namespace (`hostNetwork: true`), for example to avoid the latency and CPU overhead of an overlay network, or because clients outside the Kubernetes cluster must reach the FDB processes directly.

The operator already lets users set `hostNetwork: true` in a process class pod template (guarded by the `--allow-host-network` flag, default `true`). This only works safely when no two FDB pods ever share a node. When two host-networked FDB pods land on the same node, three things break:

1. **fdbserver ports collide.** Every pod uses the same fixed ports (`4500`/`4501` for process 1, `4502`/`4503` for process 2, and so on). The second pod's processes cannot bind their ports.
2. **The fdb-kubernetes-monitor metrics listener collides.** In the unified image the main container runs `fdb-kubernetes-monitor`, which listens on `:8081` and calls `os.Exit(1)` when the bind fails. The second pod crash-loops.
3. **An IP address no longer identifies a pod.** All pods on a node share the node's IP. Several operator code paths use a bare IP to mean "this process group": the removal safety check for log processes, inclusion after removal, incompatible-process restarts, and the admin client, which strips ports from every exclusion. On a shared IP these paths can block removals forever or exclude every process on the node.

The pods declare no container ports today, so the Kubernetes scheduler cannot see these conflicts and happily co-locates the pods. The only workaround is required pod anti-affinity that allows one FDB pod per node, which rules out dense nodes (for example one storage pod per local NVMe disk).

This design lets the operator run many host-networked pods of the same cluster on one node.

## General Design Goals

* Allow any number of host-networked FDB pods of one cluster on the same node, without port conflicts and without scheduling constraints between them.
* Keep existing clusters byte-for-byte unchanged: same pod specs, same ConfigMap entries, same ports, unless the user opts in.
* Support enabling and disabling host networking on a running cluster through the existing pod update strategies, keeping PVCs wherever the strategy recreates pods instead of replacing them.
* Keep the operator safe when several process groups share an IP: never exclude, include, or restart another process group's processes by accident.
* Keep the required `fdb-kubernetes-monitor` change small and generic. Make it fail loudly, not silently, on monitors that don't have it.
* Land the change as a series of small PRs where every PR before the last one is inert for users.

Non-goals:

* Split image (`fdbmonitor` + sidecar) support. The split image also needs a unique sidecar HTTP port, which the operator dials on `:8080`. The unified image is the default and uses pod annotations instead of HTTP.
* `publicIPSource: service`. It exists to give each pod a stable service IP, which conflicts with using the node's IP.
* Global synchronization mode (multi-operator coordination) in the first version. It stores bare IPs in the coordination state. See [Limitations](#limitations).
* Backup agents. They do not listen on ports.
* Coordinating port ranges across different FDB clusters. The operator only guarantees uniqueness within one cluster.

## Current Implementation

**Ports.** `GetProcessPort(processNumber, tls)` in `api/v1beta2/foundationdb_process_address.go` is the only port formula: TLS is `4498 + 2k` and non-TLS is `4499 + 2k` for process number `k` (1-based). `GetFullAddressList` and `cluster.GetFullAddress` build `IP:port` addresses from it. The operator uses these for the per-pod Services (`publicIPSource: service`), for the initial coordinators (`locality.InfoFromSidecar`), and in the mock admin client.

**Monitor configuration.** The unified image reads a `monitorapi.ProcessConfiguration` from the cluster ConfigMap. There is one entry per process class (`fdbmonitor-conf-<class>-json`). A single `config-map` volume item maps it to `config.json` for every pod of the class. The fdbserver command line is built by the monitor from arguments. The port of `--public_address` is a `ProcessNumber` argument with `Multiplier: 2` and `Offset: port - 2` (`internal/monitor_conf.go`, `buildIPArgument`).

The monitor argument language supports `Literal`, `Concatenate`, `Environment`, `ProcessNumber`, and `IPList`. There is no way to add an environment value to a number, so a per-pod port cannot be expressed as "base from an env var plus process offset". If a configuration references a missing environment variable, the monitor rejects it (`readConfiguration` validates it with process number 1).

The operator computes the expected configuration and command line per pod from the pod's properties (image type, servers per pod, IP family). This happens in `updatePodDynamicConf` in `controllers/cluster_controller.go` and in `internal.GetStartCommand`. The operator compares the result with what the monitor reports in the pod annotations. Some properties of an existing pod are read back from the pod itself. For example, `HasListenIPsForAllPods` checks whether the pod spec has the `FDB_POD_IP` env var.

**Pod updates.** A pod spec change is rolled out per `automationOptions.podUpdateStrategy`:

* `Delete` recreates the pod and keeps the PVC.
* `Replace` replaces the process group.
* The default, `ReplaceTransactionSystem`, replaces log and stateless process groups and recreates storage pods.

**Process identity.** Status processes are matched to process groups through the `process_id`/`instance_id` locality, so that part is IP-independent. Exclusions use `locality_instance_id:<id>` when `UseLocalitiesForExclusion()` is true, which is the default on FDB 7.1.42+ and 7.3.26+. Some paths still use bare IPs from `ProcessGroupStatus.Addresses`:

| Code path | What it does with bare IPs |
| --- | --- |
| `internal/removals/remove.go` (`getAddressesToValidateBeforeRemoval`) | Adds the bare IP for log process groups even with locality exclusions, so partitioned log servers are detected. |
| `pkg/fdbstatus/status_checks.go` (`getRemainingAndExcludedFromStatus`) | Keys the exclusion check by `MachineAddress()`; with a shared IP any other process on the node counts. |
| `controllers/remove_process_groups.go` (`getProcessesToInclude`, `processGroupAddressesRemaining`) and `ProcessGroupStatus.AllAddressesExcluded` | Include and check bare IPs, always for log groups. |
| `fdbclient/admin_client.go` (`getAddressStringsWithoutPorts`, `getAddressesAndLocalities`) | Reset every port to 0 before exclude/include ("the operator will always exclude whole pods"). |
| `controllers/remove_incompatible_processes.go` | Drops incompatible addresses whose IP matches any process in the status, then matches process groups by IP. |
| `internal/buggify/buggify.go` (`FilterIgnoredProcessGroups`) | Filters restart addresses by IP. |
| `controllers/check_client_compatibility.go` | Ignores clients whose IP matches a server process IP. |
| `controllers/exclude_processes.go` (IP path) | Excludes bare IPs when locality exclusions are disabled. |
| `internal/coordination/coordination.go` | Stores bare IPs in the global coordination state. |

Kill, bounce, coordinator selection, and coordinator validation already use full `IP:port` addresses.

**Process group IDs** are random numbers between 1 and 99999 per class (`GetNextRandomProcessGroupIDWithExclusions`), so ports cannot be derived from them.

## Proposed Design

### Overview

1. A new opt-in setting `spec.routing.hostNetwork` enables host networking and defines a port range for the cluster.
2. Each host-networked pod gets a **port block**: consecutive ports from the range, sized for the pod's number of fdbserver processes (`2n + 1` ports for `n` processes). The block is recorded in the pod spec and in its process group status, and it never changes size. Blocks are unique within the cluster, so pods of the same cluster never conflict, wherever they are scheduled. Pods declare their ports as host ports, so the scheduler keeps them away from other pods that use the same ports.
3. `fdb-kubernetes-monitor` gets a new `Sum` argument type. A separate ConfigMap entry per process class for host-networked pods computes each port as `FDB_PORT_BLOCK_START` plus an offset from the process number.
4. A process group whose pod uses a port block is identified by `IP:port` instead of bare IP everywhere the operator excludes, includes, or matches processes. Locality-based exclusions are required.

### API

```go
// RoutingConfig (existing struct, new field)

	// HostNetwork configures the FoundationDB pods to run in the host network namespace.
	// +optional
	HostNetwork *HostNetworkConfig `json:"hostNetwork,omitempty"`

// HostNetworkConfig defines how the FoundationDB pods use the host network.
type HostNetworkConfig struct {
	// Enabled defines whether the FoundationDB pods use the host network. When enabled, the operator
	// assigns every pod a unique block of ports from the port range, so that multiple pods of this
	// cluster can run on the same node. Defaults to false.
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// PortRangeStart defines the first port of the range the operator assigns port blocks from.
	// Required when Enabled is true.
	// +kubebuilder:validation:Minimum=1024
	// +kubebuilder:validation:Maximum=65533
	// +optional
	PortRangeStart *int `json:"portRangeStart,omitempty"`

	// PortRangeEnd defines the last port (inclusive) of the range the operator assigns port blocks from.
	// Required when Enabled is true.
	// +kubebuilder:validation:Minimum=1026
	// +kubebuilder:validation:Maximum=65535
	// +optional
	PortRangeEnd *int `json:"portRangeEnd,omitempty"`
}
```

```go
// ProcessGroupStatus (existing struct, new field)

	// PortBlock is the block of ports used by this process group's host-networked pod.
	// Unset if the process group's pod uses the pod network.
	// +optional
	PortBlock *PortBlock `json:"portBlock,omitempty"`

// PortBlock defines a block of consecutive ports used by one host-networked pod.
type PortBlock struct {
	// Start is the first port of the block.
	Start int `json:"start"`

	// ServersPerPod is the number of fdbserver processes the block holds. The block has
	// 2 * ServersPerPod + 1 ports: a TLS and a non-TLS port per process, and the metrics port.
	ServersPerPod int `json:"serversPerPod"`
}
```

The range has no default. Any default would contain ports that some environments use for node-level listeners, which the operator cannot see (for example Weave `6783`/`6784` or Consul `8300`–`8302` and `8500`–`8502`). It would also contain the ports of existing FDB pods that set `hostNetwork` in their template. Those pods declare no ports, so the scheduler cannot see them either. The platform owner has to pick the range.

The pod records its block in two env vars on the main container:

* `FDB_PORT_BLOCK_START`, which is new and holds `PortBlock.Start`;
* `<CLASS>_SERVERS_PER_POD` (for example `STORAGE_SERVERS_PER_POD`), which every pod already has. It holds `PortBlock.ServersPerPod` and is read by `GetServersPerPodForPod`.

Env vars in a pod spec cannot change after creation. That makes them the operator's reliable record of how a pod was created. `internal.GetPortBlock(pod)` reads both. The same pattern is used today to detect `FDB_POD_IP` for `HasListenIPsForAllPods`. `Validate` rejects pod templates that set `FDB_PORT_BLOCK_START` themselves, whether host networking is enabled or not, so the marker is always the operator's.

New helpers on `FoundationDBCluster`, next to the existing routing getters:

* `UseHostNetwork() bool` and `GetHostNetworkPortRange() (start, end int)`.
* `GetProcessGroupFullAddress(pg, ip, processNumber)`: like `GetFullAddress`, but it uses `pg.PortBlock` when set.
* `GetProcessGroupNetworkAddresses(pg) []ProcessAddress`: the addresses that identify the group's processes on the network (see [Process identity](#process-identity)).

`GetProcessPort` and `GetFullAddressList` stay as they are, as wrappers around new block-start-aware variants, so the exported API does not break.

An explicit field is used instead of inferring the mode from `podTemplate.spec.hostNetwork`. Users who already run one host-networked pod per node keep their ports and behavior. The operator also needs to own the related pod settings (DNS policy, ports, args, env), which it cannot do reliably for a user-set field. The operator never reads `pod.Spec.HostNetwork` to decide anything. It only reads its own `FDB_PORT_BLOCK_START` variable. A pod template with `hostNetwork: true` and `routing.hostNetwork` disabled keeps working exactly as today.

### Port layout

A block for a pod with `n` fdbserver processes has `2n + 1` ports, starting at `B` (`PortBlock.Start`):

| Port | Use |
| --- | --- |
| `B + 2(k - 1)` | process `k`, TLS |
| `B + 2(k - 1) + 1` | process `k`, non-TLS |
| `B + 2n` | fdb-kubernetes-monitor metrics (`--listen-address`) |

For example, a pod with one process uses `B` to `B+2`, and a pod with two processes uses `B` to `B+4`. The fdbserver ports follow today's formula with `B` in place of `4500`. Both TLS and non-TLS ports are reserved, because a process listens on both during a TLS migration. This also means a TLS migration never changes the pod spec.

There is no limit on `n` beyond the size of the range. Sizing each block by its pod keeps ports from being wasted: most pods run a single process and need 3 ports.

### Port block lifecycle

**The block describes the pod.** `PortBlock` is set when the process group's current pod uses host networking, or when `addPods` is about to create one. It is cleared only once the pod is known to be on the pod network (see *Clearing*). This makes process identity precise: a process group on the pod network, with its own IP, is identified by bare IP as today. A process group on the host network is identified by `IP:port`.

**Assignment.** `addPods` creates the missing pods. When `UseHostNetwork()` is true, it does this in two passes:

1. It collects the process groups whose pods are missing, applying today's skip rule for groups that are marked for removal and fully excluded. It gives every collected group without a block a new block, sized for the class's desired servers per pod, and then persists the status with `updateOrApply`.
2. It creates the pods.

`addPods` also runs before `updateStatus` when DNS names are used in the cluster file. The two passes do not depend on that order.

**Allocation.** The allocator (`internal/port_blocks.go`) is a small pure function. It works first-fit, like a simple memory allocator:

1. It collects every port that is taken:
   * the blocks of all process groups in the status;
   * the blocks of all existing pods of the cluster, including terminating pods, read from their env vars. This covers a status that is behind the pods, for example after the status was lost or while the cache is stale;
   * every port that appears in an `IP:port` entry of FDB's exclusion list. The list is read from the machine-readable status passed to the reconciler, the same way process group ID selection skips excluded localities. The check is skipped when no status is available.
2. It sorts the taken ports.
3. It returns the first gap in the range that is big enough for the new block.

The free space is recomputed from the taken ports every time, so there is no free list to maintain, and adjacent gaps merge automatically. If no gap is big enough, `addPods` records an event, skips those pods, and requeues.

Fragmentation can only happen while blocks of different sizes coexist. All pods of a class normally have the same `n`, so this only happens during an `n` change: the gaps left by removed old-size blocks may be too small for new blocks until neighboring blocks are released as well. The range needs headroom for this (see [Validation](#validation)).

**Blocks never grow.** A process group's block is fixed for its whole lifetime, and its pod always matches it:

* When a process group has a block, `GetPodSpec` takes the number of servers from `PortBlock.ServersPerPod`, not from the cluster's desired servers per pod.
* Changing `storageServersPerPod` or `logServersPerPod` already replaces the affected process groups, whatever the update strategy (`replacements.go`). `updatePods` never recreates such a group's pod in place. Each new process group gets a new block, sized for the new `n`.
* If an old process group's pod disappears before its replacement finishes (node failure, eviction, manual delete), `addPods` recreates it with the `n` of its block, not the new `n`. Without this rule, a pod recreated with a bigger `n` would use ports beyond its block and overlap a neighboring block. The recreated pod is still replaced as planned.

This differs from the pod network in one case: there, a pod recreated after an `n` change comes back with the new `n`. Keeping the old `n` also matches the pod's data directories, which were laid out for it. The rule also means a process group never has to change blocks, so its `IP:port` identity never changes during a removal.

**Mirroring.** For every process group with a pod, `updateStatus` sets `PortBlock` from the pod's env vars when the pod has `FDB_PORT_BLOCK_START`. It does this in the same place where it refreshes `Addresses`. This also restores the block for process groups that `updateStatus` rediscovers from existing pods.

**Clearing.** `updateStatus` clears `PortBlock` only when all of these hold:

* host networking is disabled;
* the group's pod exists and has no `FDB_PORT_BLOCK_START`;
* `updateStatus` replaces the group's `Addresses` with the current pod IPs in the same pass. That is, the group is not marked for removal and the database is available; otherwise `AddAddresses` keeps old addresses.

Because the block and the addresses change together, a group without a block never has an old node IP in `Addresses`, so a bare node IP is never excluded. Blocks are never cleared while host networking is enabled. A stale cache that briefly shows an old pod-network pod therefore cannot free a block that a new host-networked pod already uses. And while host networking is disabled, `addPods` assigns no blocks at all.

**Release and reuse.** A block is released when its process group leaves `status.processGroups`, or when the block is cleared. A process group leaves the status only after its pod is fully deleted and its exclusions are included (`confirmRemoval` keeps terminating pods). Together with the pod check in the allocator, a new pod of the same cluster can never get a block that another pod still holds.

**Changing the range.** Existing blocks are kept, so changing the range does not restart any pod. New blocks come from the new range and never overlap existing ones. To move existing pods into a new range, replace their process groups.

### Pod spec

Two predicates drive the pod spec, and both are checked only in `GetPodSpec`:

* host-network settings: `UseHostNetwork()`;
* block-dependent settings: `UseHostNetwork() && PortBlock != nil`.

When host networking is disabled, a leftover `PortBlock` has no effect on the spec.

When `UseHostNetwork()` is true, `internal.GetPodSpec` additionally:

* sets `hostNetwork: true`;
* sets `dnsPolicy: ClusterFirstWithHostNet` unless the pod template sets a DNS policy, so cluster DNS keeps working (needed for DNS names in the cluster file);
* mounts the host-network ConfigMap entry (next section) as `config.json`.

When the process group also has a `PortBlock`, it:

* sets the env var `FDB_PORT_BLOCK_START` = `B` on the main container;
* uses `PortBlock.ServersPerPod` as the pod's number of servers, everywhere `GetPodSpec` reads `GetDesiredServersPerPod` today (for example the `<CLASS>_SERVERS_PER_POD` env var and the monitor's process count);
* adds `--listen-address :<B+2n>` to the main container args;
* declares container ports on the main container for every port of the block: TLS and non-TLS for process numbers 1 to `n`, and the metrics port. They use the names from `generateServicePorts` (`tls`, `non-tls`, `tls-2`, ...) plus `metrics`, with `hostPort` set explicitly to the same value. The scheduler's NodePorts filter then refuses nodes where another pod already declared one of the ports. Ports in the template with the same names are replaced. Other template ports are kept, and with host networking they become host ports as well (documented).

A host-network spec without a block is only used to compute the pod spec hash. It differs from the running pod-network pod's hash, so the normal update flow starts (`updateStatus`, `replacements`, and `updatePods` only hash or compare it). `addPods` assigns the block before it builds a pod, so the operator never creates a pod from such a spec. The exported `podmanager.GetPodSpec`, which builds a spec for a process group that is not in the status, returns an error when host networking is enabled, so custom pod lifecycle managers cannot create incomplete pods either. When host networking is disabled, the spec difference rolls the pods back to the pod network.

### fdb-kubernetes-monitor change

The monitor's argument language gets one new type, `Sum`. It generates each child argument, parses each result as an integer, and returns the sum. It mirrors `Concatenate`, which joins the child results as strings. The change goes into `fdbkubernetesmonitor/api/config.go` in `apple/foundationdb`:

```go
// SumArgumentType defines an argument that is the sum of other arguments.
// Every sub-value must generate an integer.
SumArgumentType = "Sum"

// in GenerateArgument:
case SumArgumentType:
	sum := 0
	for _, childArgument := range argument.Values {
		childValue, err := childArgument.GenerateArgument(processNumber, env)
		if err != nil {
			return "", err
		}
		number, err := strconv.Atoi(childValue)
		if err != nil {
			return "", fmt.Errorf("value %q of sum argument is not an integer: %w", childValue, err)
		}
		sum += number
	}
	return strconv.Itoa(sum), nil
```

Nothing else in the monitor changes. `retrieveEnvironmentVariables` already walks `Values` for every argument type, so `FDB_PORT_BLOCK_START` is published in the `foundationdb.org/launcher-environment` annotation like any other variable. `--listen-address` already exists as a flag.

This is a new type rather than a new field, such as an env-var offset on `ProcessNumber`, because of how older monitors fail. They ignore unknown JSON fields, so a new field would silently produce today's port and collide. With a new type, an older monitor rejects the configuration (`unsupported argument type Sum`) when it validates it in `readConfiguration`. It then starts no fdbserver, and the operator reports the process group as missing processes.

The monitor ships inside the versioned unified FDB image (`foundationdb/fdb-kubernetes-monitor:<version>`). So the change has to be backported to every FDB release branch that should support host networking, and it becomes usable with the next patch release of each branch. The operator gates the feature on those versions (see [Validation](#validation)). The operator also bumps its `github.com/apple/foundationdb/fdbkubernetesmonitor` dependency, because it uses the same package to compute the expected command line of each process.

### Monitor configuration

**Computing the port.** The host-network variant of the process configuration adds `FDB_PORT_BLOCK_START` to the process-number term that is used today. For the TLS public address this is:

```json
{"type": "Concatenate", "values": [
  {"value": "--public_address=["},
  {"type": "Environment", "source": "FDB_PUBLIC_IP"},
  {"value": "]:"},
  {"type": "Sum", "values": [
    {"type": "Environment", "source": "FDB_PORT_BLOCK_START"},
    {"type": "ProcessNumber", "multiplier": 2, "offset": -2}
  ]},
  {"value": ":tls"}
]}
```

For process `k` this yields `B + 2(k - 1)` (TLS) and, with `offset: -1`, `B + 2(k - 1) + 1` (non-TLS). This is exactly today's formula with `B` in place of `4500`. It covers the dual TLS/non-TLS addresses used during TLS migrations and `IPList` arguments.

The change lives in `buildIPArgument`, behind a `hostNetwork` parameter. It has table tests for every process number, both TLS modes, and both IP argument types. The pod-network variant is unchanged, so clusters without host networking keep working with every existing monitor. `extractPlaceholderEnvVars` in `internal/monitor_conf.go` currently only recurses into `Concatenate` arguments. It must recurse into `Sum` as well.

**ConfigMap entries.** A pod-network pod has no `FDB_PORT_BLOCK_START`, and the monitor rejects a configuration that references a missing variable. A running pod would keep its old configuration: it could not pick up any configuration change, it would stay `IncorrectConfigMap`, and it would block bounces. A pod whose main container restarts would start no fdbserver at all.

Pods of one class can be in either mode during a transition. So both variants live side by side, the same way the ConfigMap keeps split and unified entries during an image type migration:

* `fdbmonitor-conf-<class>-json`: unchanged, always generated.
* `fdbmonitor-conf-<class>-host-network-json`: generated when `UseHostNetwork()` is true or any process group has a `PortBlock`.

**Which variant a pod uses** is decided by the presence of `FDB_PORT_BLOCK_START` in its spec, never by `pod.Spec.HostNetwork`.

* `GetConfigMapMonitorConfEntry`, `GetDynamicConfHash`, `GetMonitorProcessConfiguration`, and `GetStartCommandWithSubstitutions` take a `hostNetwork` flag.
* For existing pods, the flag is `internal.GetPortBlock(pod) != nil`. This applies to `updateStatus`, `updatePodConfig`, `updatePodDynamicConf`, and the `IncorrectCommandLine` check.
* When a pod is created (`addPods`), the flag is `UseHostNetwork()`.
* The monitor publishes every environment variable referenced by the configuration, including `FDB_PORT_BLOCK_START`, in the `foundationdb.org/launcher-environment` annotation. So the expected command line is computed from the same values the monitor used.
* For the mock pod client, `FDB_PORT_BLOCK_START` is added to the copyable substitutions in `GetSubstitutionsFromClusterAndPod`.

### Process identity

`GetProcessGroupNetworkAddresses(pg)` returns:

* without a `PortBlock`: the bare IPs in `pg.Addresses`, as today;
* with a `PortBlock`: for every IP in `pg.Addresses`, the `IP:port` of each of the block's processes (process numbers 1 to `PortBlock.ServersPerPod`), in each required address mode (`Status.RequiredAddresses`). A pod with one TLS process yields one address per IP.

The addresses carry no flags. Blocks are unique within the cluster, so each address belongs to exactly this process group. The call sites change as follows:

| Code path | Change |
| --- | --- |
| `internal/removals/remove.go` | Log process groups add `GetProcessGroupNetworkAddresses` instead of bare IPs. This keeps the partitioned-log-server safety check without matching neighbors. |
| `pkg/fdbstatus/status_checks.go` | Key addresses by `StringWithoutFlags()`, which equals `MachineAddress()` for port-less addresses. Each status process contributes its locality, IP, and `IP:port` (its primary address). This is a no-op for existing clusters. |
| `controllers/remove_process_groups.go`, `ProcessGroupStatus.AllAddressesExcluded` | Include and look up `GetProcessGroupNetworkAddresses` instead of `pg.Addresses`, with the same key function as the remaining map. |
| `fdbclient/admin_client.go` | Keep ports. Build fdbcli arguments and management API keys with `StringWithoutFlags()`, and stop mutating the caller's slice. All callers already pass port-less addresses for pod-network groups. |
| `controllers/remove_incompatible_processes.go` | A process group is incompatible when one of its IPs appears in `incompatible_connections` and none of its processes (found by `instance_id` locality) appears in the status. This works in both modes, independent of ports. |
| `internal/buggify/buggify.go` | Filter by the `IP:port` of the processes found through the `instance_id` locality. This is correct in both modes. |
| `controllers/exclude_processes.go` (IP path) | Unreachable: host networking requires locality-based exclusions. |
| `controllers/check_client_compatibility.go` | Unchanged. Clients use ephemeral ports, so they can only be matched by IP. A client on the same node as a server process is ignored by the upgrade check (documented). |
| `pkg/fdbadminclient/mock` | Store exclusions with ports, and synthesize process addresses from the pod's block. |

### Coordinators and the initial cluster file

Coordinator selection, validation, and changes use `IP:port` addresses from the machine-readable status and follow the real ports. Only bootstrap computes an address itself: `locality.InfoFromSidecar` uses `cluster.GetFullAddress(FDB_PUBLIC_IP, 1)`. It reads `FDB_PORT_BLOCK_START` from the substitutions it already has. The monitor publishes them even while `runServers` is false. When the variable is present it uses the block's port, so the signature does not change. With DNS names in the cluster file, the coordinator address uses the process's real port, which is already copied from the locality address (`coordinator.GetCoordinatorAddress`).

### Validation

`Validate` runs at the start of every reconcile and stops the whole reconcile on an error, so it only checks the spec. When `routing.hostNetwork.enabled` is true, `FoundationDBCluster.Validate` rejects the cluster unless:

* `spec.version` contains the monitor's `Sum` argument (`Version.SupportsHostNetworking()`, with a minimum patch version per release branch, like `SupportsLocalityBasedExclusions`);
* `portRangeStart` and `portRangeEnd` are set, and `portRangeEnd` is greater than `portRangeStart`;
* the range is at least as big as the blocks of all desired process groups together, that is the sum over all process classes of `count * (2 * serversPerPod + 1)`;
* the unified image is used;
* `UseLocalitiesForExclusion()` is true (FDB 7.1.42+ / 7.3.26+, not disabled);
* `publicIPSource` is `pod`;
* the operator's `--allow-host-network` flag is not `false`;
* the synchronization mode is `local`.

The running version is not validated, because a validation error would also stop the upgrade that fixes it. Instead, `UseHostNetwork()` returns true only when the running version supports it as well. If a user enables host networking together with an upgrade from an older version, the upgrade runs first on the pod network, and the pods then move to the host network. A pod is therefore never created from an image whose monitor lacks `Sum`, except for custom images, which fail loudly as described above.

The documentation recommends headroom beyond that minimum. Replacements need blocks while the old process groups still exist. This includes the transaction process groups replaced when the feature is enabled with the default update strategy. An `n` change needs room for new-size blocks while old-size gaps are still fragmented.

### Enabling and disabling on a running cluster

Enabling:

1. The user sets `routing.hostNetwork` with a port range.
2. `updateConfigMap` adds the `-host-network-json` entries. No existing pod references them, so nothing restarts.
3. Every process group's desired pod spec changes. The configured update strategy rolls it out, one fault domain at a time:
   * Pods that are recreated (`Delete`, and storage pods with the default strategy) keep their PVC. `addPods` assigns a block right before creating the new host-networked pod.
   * Process groups that are replaced (`Replace`, and log and stateless groups with the default strategy) are removed as pod-network groups, identified by their own pod IPs. The new process groups get blocks when their pods are created.
4. Process addresses change as pods move, just as pod IPs change today when pods are recreated. The existing coordinator logic moves coordinators when their addresses change. Coordinators are in different fault domains, so quorum is preserved.

During the transition, host-networked and pod-network pods talk to each other through node-to-pod routing, which standard CNIs provide.

Disabling is the reverse. Pods are recreated or replaced on the pod network, `updateStatus` clears their blocks, and the `-host-network-json` entries disappear once no block is left.

**Operator rollback.** An operator version without the `PortBlock` field drops it when it writes the status, and then runs today's bare-IP logic. Before downgrading to a version older than PR 1, disable host networking and wait until no process group has a block. Versions that contain PRs 1–4 are safe to roll back to: they know the field and reject the enabled setting in `Validate`, so they stop reconciling instead of acting on shared IPs.

### Clients with an outdated cluster file

Turning host networking on or off changes the address of every process (IP and port), so every coordinator is replaced. An FDB client tries the coordinators in its cluster file one at a time until any one answers (`fdbclient/MonitorLeader.cpp`). After a coordinator change, every previous coordinator durably stores the new connection string and answers clients that still use the old one with it (`fdbserver/coordinator/Coordination.cpp`, `setForward` and `serveOpenDatabaseRequests`). One such answer is enough for the client to switch and rewrite its cluster file. Every fdbserver runs the coordination server, so a process keeps forwarding for as long as it runs at its old address.

Without further measures, the old coordinator processes disappear right after the coordinator change: replaced process groups are removed, and recreated pods come back at a new address. A client that wasn't connected during the change then finds nothing at any address in its cluster file.

`automationOptions.coordinatorForwardingGracePeriodSeconds` (default 0, which keeps the previous behavior) closes that gap:

1. `changeCoordinators` moves the coordinators away from every process group whose address is about to change, before that happens. New coordinators are only selected among process groups that already have their final address. Invalid coordinators are still replaced right away, falling back to any process group if needed.
2. The previous coordinators get `processGroups[].forwardingCoordinatorSince`.
3. `updatePods` and `removeProcessGroups` don't change the address of such a process group, or remove it, until the grace period is over. A process that isn't running can't forward, so it is not held back.

A process group's address is about to change when it is marked for removal, when its pod moves between the pod network and the host network, when its public IP source changes, or when its pod is recreated while the cluster file uses IP addresses. So the setting also protects clients during public IP source changes, coordinator removals, and rolling updates of clusters that don't use DNS names in the cluster file.

### kubectl-fdb

The plugin ships separately from the operator, so it reads each pod's block from the pod spec's env vars (`FDB_PORT_BLOCK_START` and `<CLASS>_SERVERS_PER_POD`) instead of from the operator's helpers.

* `recover-multi-region-cluster` hard-codes ports `4500`/`4501` for new coordinators, on both the IP and the DNS path, and matches coordinators and candidates by IP. It uses the pod's block and `IP:port` matching instead.
* `fix-coordinator-ips` maps coordinators to process groups by IP. For pods with a block it matches `IP:port`.

### Testing

* **Unit tests:**
  * port math: tables for block starts, block sizes, process numbers, and TLS modes;
  * the allocator: first fit, blocks of different sizes, merging of adjacent gaps, gaps too small for a block, blocks held by pods or referenced by exclusions, exhaustion;
  * a pod recreated after an `n` change keeps the `n` of its block;
  * mirroring and clearing: no clearing while host networking is enabled, while the group is marked for removal, or while the database is unavailable;
  * blocks held only by pods that are missing from the status or terminating;
  * the host-network monitor configuration: generated arguments for several process numbers, TLS and non-TLS, `Environment` and `IPList`;
  * ConfigMap variants, pod spec fields (including "disabled, block still set"), and the identity helpers;
  * `getRemainingAndExcludedFromStatus` with two process groups on one IP;
  * admin client exclude/include with ports, through both fdbcli and the management API;
  * the incompatible-process rule and validation.
* **Controller tests (mock clients):** the mock admin client gives several pods the same node IP. The tests cover:
  * a new cluster where multiple process groups share an IP reconciles;
  * removing a log group that shares an IP with a non-excluded group completes and excludes nothing else;
  * enabling on a running cluster converges with the `Delete`, `Replace`, and default strategies;
  * disabling converges, and clears blocks and ConfigMap entries;
  * changing `storageServersPerPod` replaces the process groups with correctly sized blocks, and a pod deleted during the replacement comes back with its old `n` and no overlap.
* **e2e:** a new suite `e2e/test_operator_host_network` creates a cluster with host networking and `storageServersPerPod: 2`, constrained to fewer nodes than pods so that pods share nodes.
  * It verifies availability, replacements, bounces, an upgrade, and migrating a running cluster to host networking and back.
  * e2e clusters share nodes, so the fixtures give each test cluster its own port range.
  * The fixtures need to know about blocks: the tester deployment's fixed `:4500` and the `prometheus.io/port: 8081` annotation.
  * The suite does not use chaos-mesh network chaos, which would hit every pod on the node.

### Documentation

* A new manual page `docs/manual/host_networking.md` covers:
  * requirements (privileged Pod Security level, `--allow-host-network`);
  * choosing a port range, including headroom;
  * scraping metrics through the `metrics` container port (static `prometheus.io/port` annotations do not work because the port differs per pod). The metrics endpoint listens on all node interfaces;
  * user-defined probes and container ports on fixed ports;
  * TLS certificates that must accept node IPs;
  * the limitations below.
* `docs/manual/warnings.md`: update "Limitations on FDB Port Customization".
* `docs/cluster_spec.md`: regenerated.

### Limitations

* Unified image only.
* Requires an FDB version whose `fdb-kubernetes-monitor` has the `Sum` argument. Older versions have to upgrade first.
* Requires locality-based exclusions (FDB 7.1.42+ / 7.3.26+).
* Global synchronization mode is not supported yet. `internal/coordination` stores bare IPs, which would leave `IP:port` exclusions of log groups behind after removal. Supporting it means storing `GetProcessGroupNetworkAddresses` in the coordination state; this is a follow-up.
* The operator cannot see listeners that are not pods (kubelet `10250`, kube-proxy `10249`/`10256`, other host daemons), and the range must avoid them. Ports in the Linux ephemeral range (default `32768`–`60999`) can be taken by outgoing connections, so ranges should stay below it.
* Port overlaps between different FDB clusters are not prevented by the operator. Declared host ports make the scheduler keep conflicting pods apart, which can leave pods `Pending` if the ranges overlap. Each cluster needs its own range.
* The upgrade client-compatibility check cannot tell apart clients that run on the same node as a server process.
* Most CNI plugins do not enforce NetworkPolicies for host-networked pods.

## Alternatives Considered

* **Required anti-affinity, one FDB pod per node (no operator change).** This works today, but it rules out dense nodes, which is the main reason to want this feature.
* **Port offsets per process class.** Only the ConfigMap would change, but this still allows only one pod per class per node.
* **Slots unique per node instead of per cluster.** The operator has to pick ports before the scheduler picks the node. With a small slot pool, pods go `Pending` whenever the chosen slot is taken on every feasible node, even though other slots are free. Picking the slot after scheduling is impossible, because env vars and volumes are immutable.
* **Per-process-group ConfigMap entries or ConfigMaps.** The monitor could then use literal ports, but the ConfigMap grows by about 2 KB per process group and hits the 1 MiB limit at a few hundred pods. Separate ConfigMaps turn every configuration change into N API writes.
* **Operator-only encoding by string concatenation.** The pod would get `FDB_PORT_PREFIX = B / 10`, and the configuration would append one digit from the process number (`Concatenate[Environment(FDB_PORT_PREFIX), ProcessNumber(multiplier 2, offset -2)]`, so `453` + `2` = `4532`). It works with every existing monitor, so it needs no FDB release and no version gate. However, it is arithmetic hidden in string concatenation, it only works while the digit stays below 10, and it forces range starts to be multiples of 10. It is the fallback if the host-networking feature has to work with FDB versions released before the monitor change. Switching to `Sum` later only changes `buildIPArgument` and the env var.
* **An env-var offset field on the existing `ProcessNumber` argument** instead of a new `Sum` type. The monitor change would be equally small, but older monitors ignore unknown fields. They would silently compute today's ports, and pods would collide instead of failing.
* **`hostPort` on the pod network.** Every pod would still need its own advertised port in `--public_address`, so the monitor configuration problem stays. It also adds NAT and a public/listen address split without the benefits of host networking.
* **Deriving ports from the process group ID.** IDs are random numbers between 1 and 99999 per class, so the ports would be unbounded and would collide across classes.
* **Assigning blocks to all process groups as soon as the feature is enabled.** Blocks would then be known before the pods move, but a process group would carry a block while its pod still runs on the pod network. Its `IP:port` identity would miss its real processes on port 4500. With the default update strategy, every replaced log group is removed in exactly that state, which weakens the partitioned-log-server check. Assigning the block when the host-networked pod is created avoids this state entirely.
* **Fixed-size blocks** (for example 10 ports). The allocator only needs a start per block, but the block size caps the number of processes per pod. Every pod also pays for the largest size: a pod with one process would use 3 of its 10 ports.
* **Growing or moving a block when a pod is recreated with a bigger `n`.** This follows the cluster's desired `n` like the pod network does. But a process group marked for removal would change its `IP:port` identity mid-removal, and the exclusions of its old block could be left behind. Keeping the block's `n` avoids both.
* **Replacing every process group when toggling the feature.** The invariant would be slightly simpler (a group's network mode never changes), but it moves all data. Following the configured update strategy keeps PVCs where the user asked for that.

## Key Decisions

1. **Explicit opt-in field (`spec.routing.hostNetwork`) with a required port range.** Existing users who set `hostNetwork` themselves keep their ports. Only the platform owner knows which ports are free on the nodes.
2. **The operator-owned `FDB_PORT_BLOCK_START` env var decides a pod's mode; `pod.Spec.HostNetwork` is never read.** Pod spec env vars cannot change, unlike annotations that users or other controllers can edit. This keeps template-managed host networking unaffected, and it follows the existing `FDB_POD_IP` check.
3. **Port blocks unique within the cluster, assigned when a host-networked pod is created, and mirrored into `ProcessGroupStatus.PortBlock`.** The operator can only decide before scheduling, so uniqueness within the cluster is the only guarantee that holds wherever pods land. Because the block always describes the actual pod, process identity is exact in both directions of a migration.
4. **Blocks sized per pod (`2n + 1` ports), first-fit allocation, and a fixed size for the block's lifetime.** There is no cap on processes per pod and no wasted ports. The pod always runs the `n` of its block, so blocks never grow and cannot overlap. Changing `n` goes through replacement, as it already does.
5. **A generic `Sum` argument in `fdb-kubernetes-monitor` computes the ports.** Port math stays plain arithmetic, the range start has no alignment constraint, and older monitors fail loudly instead of colliding. The cost is a cross-repository change, and a minimum FDB version per release branch.
6. **Separate ConfigMap entries per network mode, chosen per pod.** Mixed-mode transitions stay safe, and the existing entries stay byte-for-byte identical.
7. **`IP:port` identity for host-networked groups; locality exclusions required; the admin client keeps ports.** This is the smallest change that removes every bare-IP ambiguity while keeping the partitioned-log-server safety check.
8. **Declare container ports with explicit host ports.** The scheduler then keeps conflicting pods of different clusters apart instead of letting them crash-loop.
9. **Narrow first scope (unified image, local synchronization mode, no services).** This keeps the change reviewable. Each exclusion is enforced by validation and has a clear follow-up.
10. **Staged PRs behind a temporary operator gate.** Every PR before the last one can be merged and released without any effect on users. Rolling back to one of those releases is safe.

## Open Questions

1. **Release branches:** which FDB release branches get the `Sum` backport (for example 7.3 and 7.4)? This sets the minimum versions in `SupportsHostNetworking()` and decides which clusters can use the feature.
2. **Global synchronization mode:** is it acceptable to exclude it from the first version?

## PR Plan

**PR 0 (`apple/foundationdb`) — `Sum` argument for fdb-kubernetes-monitor**
* Files: `fdbkubernetesmonitor/api/config.go` (`SumArgumentType` and its case in `GenerateArgument`) and `fdbkubernetesmonitor/api/config_test.go` (a table test: adding positive and negative values, values that are not integers, and errors of values). Then backports to the chosen release branches.
* Status: open on the fork as [ArniDagur/foundationdb#1](https://github.com/ArniDagur/foundationdb/pull/1), against the `next` branch.
* Depends on: none.
* Review focus: about 20 lines in a package that has no other behavior change. The operator PRs can start in parallel. PR 4 needs PR 0 merged, and using the feature needs FDB releases that contain it.

Every operator PR keeps existing clusters unchanged. PR 1 adds a temporary reconciler field, `EnableHostNetworking`, which defaults to false and is passed to `Validate`. While it is false, `Validate` rejects `routing.hostNetwork.enabled: true`. Tests in PRs 2–4 set the field, so they can exercise the reconcile loop. PR 5 removes the gate.

**PR 1 — API: host networking fields (inert)**
* Files: `api/v1beta2/foundationdbcluster_types.go` (`HostNetworkConfig`, `RoutingConfig.HostNetwork`, `PortBlock`, `ProcessGroupStatus.PortBlock`, getters, constants, `Validate` rules, the temporary gate), `api/v1beta2/foundationdb_version.go` (`SupportsHostNetworking()`; minimum versions filled in once PR 0 is released), `api/v1beta2/foundationdb_env_variables.go` (`FDB_PORT_BLOCK_START`), `controllers/cluster_controller.go` (gate field), generated deepcopy, apply configurations, CRDs (`config/crd`, `charts`), `docs/cluster_spec.md`, and tests.
* Depends on: none.
* Review focus: API shape and validation rules. Most of the diff is generated.

**PR 2 — Port-aware addresses and exclusion matching**
* Files: `api/v1beta2/foundationdb_process_address.go` and `foundationdbcluster_types.go` (block-start-aware port helpers, `GetProcessGroupFullAddress`, `GetProcessGroupNetworkAddresses`, `AllAddressesExcluded`), `internal/removals/remove.go`, `pkg/fdbstatus/status_checks.go`, `controllers/remove_process_groups.go`, `fdbclient/admin_client.go`, `controllers/remove_incompatible_processes.go`, `internal/buggify/buggify.go`, `pkg/fdbadminclient/mock` (exclusions with ports), and tests.
* Depends on: PR 1.
* Behavior for existing clusters is unchanged: without blocks the helpers return bare IPs, and the existing tests pass unmodified. New unit tests build a status where two process groups share an IP.

**PR 3 — Port block allocation and mirroring**
* Files: `internal/port_blocks.go` (first-fit allocator), `internal/pod_helper.go` (`GetPortBlock(pod)` from the pod's env vars), `controllers/update_status.go` (mirror and clear `PortBlock`, restore it for rediscovered process groups), and tests.
* Depends on: PR 1. Can be developed in parallel with PR 2.
* Inert: no pod has `FDB_PORT_BLOCK_START` yet.

**PR 4 — Host-network monitor configuration and ConfigMap entries**
* Files: `go.mod` (bump `github.com/apple/foundationdb/fdbkubernetesmonitor` to include `Sum`), `internal/monitor_conf.go` (`hostNetwork` parameter, `Sum` in `buildIPArgument`, `extractPlaceholderEnvVars` recursion), `internal/configmap_helper.go` (`-host-network-json` entries, key and hash helpers), `controllers/cluster_controller.go` (`updatePodDynamicConf`), `controllers/update_status.go`, `controllers/update_pod_config.go`, `controllers/add_pods.go` (hash flag), `internal/pod_client.go` (`FDB_PORT_BLOCK_START` in the mock substitutions), `internal/locality/locality.go` (`InfoFromSidecar` port), and tests.
* Depends on: PR 0 (merged upstream) and PR 3, for `GetPortBlock(pod)`.
* Inert: the variant is only generated behind the gate or when blocks exist. Pods only select it through `FDB_PORT_BLOCK_START`, which no pod has yet.

**PR 5 — Host-networked pods; enable the feature**
* Files: `internal/pod_models.go` (`hostNetwork`, DNS policy, container ports, `FDB_PORT_BLOCK_START`, servers per pod from the block, `--listen-address`, ConfigMap key, `UseHostNetwork()` also requiring a supported running version), `controllers/add_pods.go` (two-pass block assignment before creating pods), `pkg/podmanager/pod_lifecycle_manager.go` (exported `GetPodSpec` rejects host networking), `pkg/fdbadminclient/mock` (process addresses from blocks, shared node IPs), removal of the temporary gate, controller tests (new cluster, shared IPs, log removal, enable with every update strategy, disable, `n` change), `docs/manual/host_networking.md`, and `docs/manual/warnings.md`.
* Depends on: PRs 2, 3, and 4.

**PR 6 — kubectl-fdb support**
* Files: `kubectl-fdb/cmd/recover_multi_region_cluster.go` (IP and DNS paths), `kubectl-fdb/cmd/fix_coordinator_ips.go`, and tests.
* Depends on: PR 1 (`FDB_PORT_BLOCK_START` constant).

**PR 7 — e2e test suite for host networking**
* Files: `e2e/test_operator_host_network/`, `e2e/fixtures` (a cluster config option with a unique port range per test cluster; block-aware tester and metrics settings), and the e2e `Makefile` target.
* Depends on: PR 5, and FDB images that contain PR 0 (released, or built from the branch for CI).

Follow-ups outside this plan: global synchronization mode, and enabling host networking per process class.

## Related Links

* [Running Multiple Storage Servers per Disk](implemented/multiple_storage_per_disk.md), which introduced the per-process port scheme.
* [Unified sidecar and process monitor](unified_sidecar_and_process_monitor.md).
* [Pod template restrictions](../manual/pod_template_restrictions.md) (`--allow-host-network`).
* [fdb-kubernetes-monitor argument types](https://github.com/apple/foundationdb/blob/main/fdbkubernetesmonitor/api/config.go).
