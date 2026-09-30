# Running with Host Networking

The operator can run the FoundationDB pods in the host network namespace, so that the fdbserver processes listen directly on the IP of their node.
Every pod gets its own block of ports, which allows many pods of the same cluster to run on the same node.
See the [design document](../design/host_networking.md) for the details.

## Requirements

* The unified image (`imageType: unified`, the default).
* An FDB version whose `fdb-kubernetes-monitor` supports the `Sum` argument. No released FDB version contains it yet.
  If you build your own images with a monitor that contains it, start the operator with `--skip-host-network-version-check`.
* Locality-based exclusions, which are the default on FDB 7.1.42+ and 7.3.26+.
* `publicIPSource: pod` (the default) and the `local` synchronization mode (the default).
* The operator must allow host networking (`--allow-host-network`, default `true`), and the namespace must allow privileged pods in its Pod Security settings.

## Example

```yaml
apiVersion: apps.foundationdb.org/v1beta2
kind: FoundationDBCluster
metadata:
  name: sample-cluster
spec:
  routing:
    hostNetwork:
      enabled: true
      portRangeStart: 20000
      portRangeEnd: 22999
  # Other settings
```

## Ports

A pod with `n` fdbserver processes gets a block of `2n + 1` consecutive ports starting at `B`:

| Port | Use |
| --- | --- |
| `B + 2(k - 1)` | process `k`, TLS |
| `B + 2(k - 1) + 1` | process `k`, non-TLS |
| `B + 2n` | the metrics endpoint of the `fdb-kubernetes-monitor` |

The operator records the block in the `portBlock` field of the process group status and in the `FDB_PORT_BLOCK_START` environment variable of the pod.
It declares the ports as host ports of the main container, named `tls`, `non-tls`, `tls-2`, `non-tls-2`, ... and `metrics`, so the scheduler keeps other pods that use the same ports off the node.
Ports in your pod template with the same names are replaced; other ports of the template are kept and become host ports as well.

Choosing the port range:

* The range must be big enough for the blocks of all process groups, plus headroom for replacements, which need a block while the old process group still exists.
  Validation rejects a range that can't hold the blocks of the desired process groups.
* The range must not contain ports of listeners that aren't pods, for example the kubelet (`10250`), kube-proxy (`10249`, `10256`), or other daemons on your nodes.
  The operator can't see them.
* The range should stay below the Linux ephemeral port range (default `32768`-`60999`), whose ports can be taken by outgoing connections.
* Every FDB cluster that shares nodes with another one needs its own range.
  The operator only guarantees unique ports within one cluster.
* Changing the range doesn't restart any pod. New blocks come from the new range.

Changing `storageServersPerPod` or `logServersPerPod` replaces the process groups, as without host networking.
The new process groups get blocks of the new size.

## Metrics

The metrics port differs per pod, so static `prometheus.io/port` annotations don't work.
Scrape the container port named `metrics` instead, for example with a `PodMonitor`.
The metrics endpoint listens on all interfaces of the node.

## Turning host networking on or off

Setting `routing.hostNetwork.enabled` on a running cluster rolls the pods through the configured pod update strategy.
Pods that are recreated keep their volumes, and process groups that are replaced move to the new mode as new process groups.
Turning it off works the same way, and the operator clears the port blocks once the pods run on the pod network again.

Setting `hostNetwork: true` only in the pod template keeps working as before, without port blocks.

Before downgrading to an operator version without host networking support, turn host networking off and wait until no process group has a `portBlock`.

## Limitations

* The upgrade client-compatibility check can't tell apart clients that run on the same node as a server process.
* TLS certificates must accept the node IPs if your peer verification rules check IP addresses.
* Most CNI plugins don't enforce NetworkPolicies for host-networked pods.

## Next

You can continue on to the [next section](backup.md) or go back to the [table of contents](index.md).
