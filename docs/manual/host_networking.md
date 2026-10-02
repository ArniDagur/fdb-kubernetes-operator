# Running with Host Networking

The operator can run the FoundationDB pods in the host network namespace, so that the fdbserver processes listen directly on the IP of their node.
The processes use the default ports `4500` and up, so no two FDB pods may run on the same node; use pod anti-affinity to guarantee that.

## Requirements

* The operator must allow host networking (`--allow-host-network`, default `true`), and the namespace must allow privileged pods in its Pod Security settings.

## Example

```yaml
apiVersion: apps.foundationdb.org/v1beta2
kind: FoundationDBCluster
metadata:
  name: sample-cluster
spec:
  routing:
    publicIPSource: hostNetwork
  # Other settings
```

## Turning host networking on or off

Changing `routing.publicIPSource` replaces process groups, as for every public IP source change.
How many depends on `automationOptions.maxConcurrentReplacements`:
* Unlimited (the default): every process group is replaced. New process groups are created in the new mode, storage data moves to the new storage servers, and the old process groups are excluded and removed. Make sure the cluster has room for twice the pods and storage.
* With a limit: only the process groups of the first reconciliation, up to the limit, are replaced. The operator updates the public IP source annotation of the remaining pods right away, so they are rolled through the pod update strategy instead, and pods that are recreated in place keep their data.
Turning it off works the same way: set `publicIPSource` back to `pod`.
Every process gets a new address, so the operator first moves the coordinators to process groups that already use the new mode.
The previous coordinators keep running at their old address for `automationOptions.coordinatorForwardingGracePeriodSeconds` (10 minutes by default) and forward clients with an outdated cluster file to the new coordinators.

Setting `hostNetwork: true` only in the pod template, without `publicIPSource: hostNetwork`, keeps working as before.

## Limitations

* The upgrade client-compatibility check can't tell apart clients that run on the same node as a server process.
* TLS certificates must accept the node IPs if your peer verification rules check IP addresses.
* Most CNI plugins don't enforce NetworkPolicies for host-networked pods.

## Next

You can continue on to the [next section](backup.md) or go back to the [table of contents](index.md).
