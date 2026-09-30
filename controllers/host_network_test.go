/*
 * host_network_test.go
 *
 * This source file is part of the FoundationDB open source project
 *
 * Copyright 2026 Apple Inc. and the FoundationDB project authors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package controllers

import (
	"context"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"

	fdbv1beta2 "github.com/FoundationDB/fdb-kubernetes-operator/v2/api/v1beta2"
	"github.com/FoundationDB/fdb-kubernetes-operator/v2/internal"
	"github.com/FoundationDB/fdb-kubernetes-operator/v2/pkg/fdbadminclient/mock"
	"github.com/go-logr/logr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// hostNetworkNode is the node that all pods of the test cluster run on, so all of them share its IP.
const hostNetworkNode = "node-a"

// getMainContainerEnv returns the value of an environment variable of the main container of the pod.
func getMainContainerEnv(pod *corev1.Pod, name string) (string, bool) {
	for _, container := range pod.Spec.Containers {
		if container.Name != fdbv1beta2.MainContainerName {
			continue
		}

		for _, env := range container.Env {
			if env.Name == name {
				return env.Value, true
			}
		}
	}

	return "", false
}

// expectNonOverlappingBlocks checks that every process group has a port block of the right size inside the range,
// and that no two blocks overlap.
func expectNonOverlappingBlocks(cluster *fdbv1beta2.FoundationDBCluster) {
	start, end := cluster.GetHostNetworkPortRange()
	blocks := make([]*fdbv1beta2.PortBlock, 0, len(cluster.Status.ProcessGroups))
	for _, processGroup := range cluster.Status.ProcessGroups {
		Expect(
			processGroup.PortBlock,
		).NotTo(BeNil(), "process group %s has no port block", processGroup.ProcessGroupID)
		Expect(processGroup.PortBlock.Start).To(BeNumerically(">=", start))
		Expect(processGroup.PortBlock.End()).To(BeNumerically("<=", end))
		blocks = append(blocks, processGroup.PortBlock)
	}

	slices.SortFunc(blocks, func(a, b *fdbv1beta2.PortBlock) int {
		return a.Start - b.Start
	})
	for idx := 1; idx < len(blocks); idx++ {
		Expect(blocks[idx].Start).To(
			BeNumerically(">", blocks[idx-1].End()),
			"block %v overlaps block %v", *blocks[idx], *blocks[idx-1],
		)
	}
}

// getClusterPods returns the pods of the cluster by process group ID.
func getClusterPods(
	cluster *fdbv1beta2.FoundationDBCluster,
) map[fdbv1beta2.ProcessGroupID]*corev1.Pod {
	pods := &corev1.PodList{}
	Expect(k8sClient.List(context.TODO(), pods, getListOptions(cluster)...)).To(Succeed())
	result := make(map[fdbv1beta2.ProcessGroupID]*corev1.Pod, len(pods.Items))
	for idx := range pods.Items {
		pod := &pods.Items[idx]
		result[fdbv1beta2.ProcessGroupID(pod.Labels[fdbv1beta2.FDBProcessGroupIDLabel])] = pod
	}

	return result
}

// getProcessGroup returns the first process group of the given class that is not marked for removal.
func getProcessGroup(
	cluster *fdbv1beta2.FoundationDBCluster,
	processClass fdbv1beta2.ProcessClass,
) *fdbv1beta2.ProcessGroupStatus {
	for _, processGroup := range cluster.Status.ProcessGroups {
		if processGroup.ProcessClass == processClass && !processGroup.IsMarkedForRemoval() {
			return processGroup
		}
	}

	Fail(fmt.Sprintf("no process group of class %s", processClass))
	return nil
}

// newHostNetworkCluster returns a cluster whose pods all run on the same node.
func newHostNetworkCluster(hostNetwork bool) *fdbv1beta2.FoundationDBCluster {
	cluster := internal.CreateDefaultCluster()
	cluster.Spec.ImageType = ptr.To(fdbv1beta2.ImageTypeUnified)
	cluster.Spec.StorageServersPerPod = 2
	cluster.Spec.Processes = map[fdbv1beta2.ProcessClass]fdbv1beta2.ProcessSettings{
		fdbv1beta2.ProcessClassGeneral: {
			PodTemplate: &corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{NodeName: hostNetworkNode},
			},
		},
	}
	cluster.Spec.Routing.HostNetwork = &fdbv1beta2.HostNetworkConfig{
		Enabled:        ptr.To(hostNetwork),
		PortRangeStart: ptr.To(20000),
		PortRangeEnd:   ptr.To(20999),
	}
	// With DNS names in the cluster file the mock admin client reports DNS names as process addresses. FDB always
	// reports IP:port for processes, which is what matters when several process groups share an IP.
	cluster.Spec.Routing.UseDNSInClusterFile = ptr.To(false)

	return cluster
}

var _ = Describe("host networking", func() {
	var cluster *fdbv1beta2.FoundationDBCluster
	var adminClient *mock.AdminClient

	BeforeEach(func() {
		// No released FDB version ships the Sum argument of the fdb-kubernetes-monitor yet.
		clusterReconciler.AllowedPodModifications = &fdbv1beta2.AllowedPodModifications{
			SkipHostNetworkVersionCheck: ptr.To(true),
		}
	})

	When("creating a cluster with host networking", func() {
		BeforeEach(func() {
			cluster = newHostNetworkCluster(true)
			Expect(k8sClient.Create(context.TODO(), cluster)).To(Succeed())

			result, err := reconcileCluster(cluster)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeZero())
			_, err = reloadCluster(cluster)
			Expect(err).NotTo(HaveOccurred())

			adminClient, err = mock.NewMockAdminClientUncast(cluster, k8sClient)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should give every pod its own port block on the shared node IP", func() {
			expectNonOverlappingBlocks(cluster)

			pods := getClusterPods(cluster)
			Expect(pods).To(HaveLen(len(cluster.Status.ProcessGroups)))
			nodeIPs := map[string]fdbv1beta2.None{}
			for _, processGroup := range cluster.Status.ProcessGroups {
				expectedServers := 1
				if processGroup.ProcessClass == fdbv1beta2.ProcessClassStorage {
					expectedServers = 2
				}
				Expect(processGroup.PortBlock.ServersPerPod).To(Equal(expectedServers))

				pod := pods[processGroup.ProcessGroupID]
				Expect(pod.Spec.HostNetwork).To(BeTrue())
				nodeIPs[pod.Status.PodIP] = fdbv1beta2.None{}

				start, ok := getMainContainerEnv(pod, fdbv1beta2.EnvNamePortBlockStart)
				Expect(ok).To(BeTrue())
				Expect(start).To(Equal(strconv.Itoa(processGroup.PortBlock.Start)))
			}
			Expect(nodeIPs).To(HaveLen(1), "all pods should share the IP of their node")
		})

		It(
			"should run every process on a distinct port inside the block of its process group",
			func() {
				status, err := adminClient.GetStatus()
				Expect(err).NotTo(HaveOccurred())

				blocks := map[string]*fdbv1beta2.PortBlock{}
				expectedProcesses := 0
				for _, processGroup := range cluster.Status.ProcessGroups {
					blocks[string(processGroup.ProcessGroupID)] = processGroup.PortBlock
					expectedProcesses += processGroup.PortBlock.ServersPerPod
				}

				// All processes share the IP of the node, so every process needs a port of its own.
				ports := map[int]fdbv1beta2.None{}
				for _, process := range status.Cluster.Processes {
					block := blocks[process.Locality[fdbv1beta2.FDBLocalityInstanceIDKey]]
					Expect(block).NotTo(BeNil())
					Expect(process.Address.Port).To(BeNumerically(">=", block.Start))
					// The last port of the block belongs to the metrics endpoint.
					Expect(process.Address.Port).To(BeNumerically("<", block.MetricsPort()))
					ports[process.Address.Port] = fdbv1beta2.None{}
				}
				Expect(ports).To(HaveLen(expectedProcesses))
			},
		)

		It("should add the host network monitor configuration to the ConfigMap", func() {
			configMap := &corev1.ConfigMap{}
			Expect(k8sClient.Get(context.TODO(), client.ObjectKey{
				Namespace: cluster.Namespace,
				Name:      cluster.Name + "-config",
			}, configMap)).To(Succeed())
			Expect(configMap.Data).To(HaveKey("fdbmonitor-conf-storage-host-network-json"))
			Expect(
				configMap.Data["fdbmonitor-conf-storage-host-network-json"],
			).To(ContainSubstring(`"type":"Sum"`))
		})

		When(
			"removing a log and a storage process group that share the node IP with other process groups",
			func() {
				var removed []*fdbv1beta2.ProcessGroupStatus
				var others []*fdbv1beta2.ProcessGroupStatus
				var nodeIP string

				BeforeEach(func() {
					others = nil
					removed = []*fdbv1beta2.ProcessGroupStatus{
						getProcessGroup(cluster, fdbv1beta2.ProcessClassLog).DeepCopy(),
						getProcessGroup(cluster, fdbv1beta2.ProcessClassStorage).DeepCopy(),
					}
					Expect(removed[0].Addresses).To(Equal(removed[1].Addresses))
					nodeIP = removed[0].Addresses[0]

					for _, processGroup := range cluster.Status.ProcessGroups {
						if processGroup.ProcessGroupID != removed[0].ProcessGroupID &&
							processGroup.ProcessGroupID != removed[1].ProcessGroupID {
							others = append(others, processGroup.DeepCopy())
						}
					}

					cluster.Spec.ProcessGroupsToRemove = []fdbv1beta2.ProcessGroupID{
						removed[0].ProcessGroupID,
						removed[1].ProcessGroupID,
					}
					Expect(k8sClient.Update(context.TODO(), cluster)).To(Succeed())

					result, err := reconcileCluster(cluster)
					Expect(err).NotTo(HaveOccurred())
					Expect(result.RequeueAfter).To(BeZero())
					_, err = reloadCluster(cluster)
					Expect(err).NotTo(HaveOccurred())
				})

				It("should remove both process groups", func() {
					for _, processGroup := range removed {
						Expect(fdbv1beta2.ContainsProcessGroupID(
							cluster.Status.ProcessGroups,
							processGroup.ProcessGroupID,
						)).To(BeFalse())
					}
				})

				It(
					"should exclude the removed process groups by locality and the log process by IP:port",
					func() {
						// Every address that was excluded was included again after the removal.
						Expect(adminClient.ExcludedAddresses).To(BeEmpty())
						// The cluster doesn't use TLS, so the log process listens on the non-TLS port of process 1 of its block.
						logProcess := fdbv1beta2.ProcessAddress{
							IPAddress: net.ParseIP(nodeIP),
							Port:      removed[0].PortBlock.Start + 1,
						}
						Expect(
							adminClient.ReincludedAddresses,
						).To(HaveKey(removed[0].GetExclusionString()))
						Expect(
							adminClient.ReincludedAddresses,
						).To(HaveKey(removed[1].GetExclusionString()))
						Expect(adminClient.ReincludedAddresses).To(HaveKey(logProcess.String()))
					},
				)

				It(
					"should never exclude the bare node IP or anything of the other process groups",
					func() {
						excluded := map[string]fdbv1beta2.None{}
						for address := range adminClient.ReincludedAddresses {
							excluded[address] = fdbv1beta2.None{}
						}
						for address := range adminClient.ExcludedAddresses {
							excluded[address] = fdbv1beta2.None{}
						}

						Expect(excluded).NotTo(HaveKey(nodeIP))
						for _, processGroup := range others {
							Expect(excluded).NotTo(HaveKey(processGroup.GetExclusionString()))
							for _, address := range cluster.GetProcessGroupNetworkAddresses(processGroup) {
								Expect(excluded).NotTo(HaveKey(address.String()))
							}
						}
					},
				)

				It(
					"should give the replacement process groups blocks that don't overlap any other block",
					func() {
						Expect(cluster.Status.ProcessGroups).To(HaveLen(len(others) + 2))
						expectNonOverlappingBlocks(cluster)
					},
				)
			},
		)

		When("removing a log process group whose processes are missing from the status", func() {
			var removed *fdbv1beta2.ProcessGroupStatus

			BeforeEach(func() {
				removed = getProcessGroup(cluster, fdbv1beta2.ProcessClassLog).DeepCopy()
				adminClient.MockMissingProcessGroup(removed.ProcessGroupID, true)

				cluster.Spec.ProcessGroupsToRemove = []fdbv1beta2.ProcessGroupID{
					removed.ProcessGroupID,
				}
				Expect(k8sClient.Update(context.TODO(), cluster)).To(Succeed())

				result, err := reconcileCluster(cluster)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(BeZero())
				_, err = reloadCluster(cluster)
				Expect(err).NotTo(HaveOccurred())
			})

			It(
				"should exclude the missing log process by IP:port instead of the node IP and remove it",
				func() {
					Expect(fdbv1beta2.ContainsProcessGroupID(
						cluster.Status.ProcessGroups,
						removed.ProcessGroupID,
					)).To(BeFalse())

					excluded := map[string]fdbv1beta2.None{}
					for address := range adminClient.ReincludedAddresses {
						excluded[address] = fdbv1beta2.None{}
					}
					for address := range adminClient.ExcludedAddresses {
						excluded[address] = fdbv1beta2.None{}
					}

					// The cluster doesn't use TLS, so the log process listens on the non-TLS port of process 1.
					Expect(excluded).To(HaveKey(fmt.Sprintf(
						"%s:%d",
						removed.Addresses[0],
						removed.PortBlock.Start+1,
					)))
					Expect(excluded).NotTo(HaveKey(removed.Addresses[0]))
				},
			)
		})

		When("a host-networked pod is deleted", func() {
			var processGroup *fdbv1beta2.ProcessGroupStatus

			BeforeEach(func() {
				processGroup = getProcessGroup(cluster, fdbv1beta2.ProcessClassStorage).DeepCopy()
				pod := getClusterPods(cluster)[processGroup.ProcessGroupID]
				Expect(k8sClient.Delete(context.TODO(), pod)).To(Succeed())

				result, err := reconcileCluster(cluster)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(BeZero())
				_, err = reloadCluster(cluster)
				Expect(err).NotTo(HaveOccurred())
			})

			It("should recreate the pod with the same port block", func() {
				pod := getClusterPods(cluster)[processGroup.ProcessGroupID]
				Expect(pod).NotTo(BeNil())
				start, ok := getMainContainerEnv(pod, fdbv1beta2.EnvNamePortBlockStart)
				Expect(ok).To(BeTrue())
				Expect(start).To(Equal(strconv.Itoa(processGroup.PortBlock.Start)))
				servers, ok := getMainContainerEnv(pod, "STORAGE_SERVERS_PER_POD")
				Expect(ok).To(BeTrue())
				Expect(servers).To(Equal("2"))

				current := getProcessGroup(cluster, fdbv1beta2.ProcessClassStorage)
				Expect(current.ProcessGroupID).To(Equal(processGroup.ProcessGroupID))
				Expect(current.PortBlock).To(Equal(processGroup.PortBlock))
			})
		})

		When("the number of storage servers per pod changes", func() {
			var oldProcessGroup *fdbv1beta2.ProcessGroupStatus

			BeforeEach(func() {
				oldProcessGroup = getProcessGroup(
					cluster,
					fdbv1beta2.ProcessClassStorage,
				).DeepCopy()
				cluster.Spec.StorageServersPerPod = 1
				Expect(k8sClient.Update(context.TODO(), cluster)).To(Succeed())
			})

			It(
				"should recreate a deleted pod of an old process group with the servers of its block",
				func() {
					pod := getClusterPods(cluster)[oldProcessGroup.ProcessGroupID]
					Expect(k8sClient.Delete(context.TODO(), pod)).To(Succeed())

					// The reconciler normalizes the spec before running the sub-reconcilers.
					Expect(
						internal.NormalizeClusterSpec(cluster, internal.DeprecationOptions{}),
					).To(Succeed())
					Expect(addPods{}.reconcile(
						context.TODO(),
						clusterReconciler,
						cluster,
						nil,
						logr.Discard(),
					)).To(BeNil())

					pod = getClusterPods(cluster)[oldProcessGroup.ProcessGroupID]
					Expect(pod).NotTo(BeNil())
					servers, ok := getMainContainerEnv(pod, "STORAGE_SERVERS_PER_POD")
					Expect(ok).To(BeTrue())
					Expect(servers).To(Equal("2"))
					start, ok := getMainContainerEnv(pod, fdbv1beta2.EnvNamePortBlockStart)
					Expect(ok).To(BeTrue())
					Expect(start).To(Equal(strconv.Itoa(oldProcessGroup.PortBlock.Start)))
					var args []string
					for _, container := range pod.Spec.Containers {
						if container.Name == fdbv1beta2.MainContainerName {
							args = container.Args
						}
					}
					Expect(strings.Join(args, " ")).To(ContainSubstring(
						fmt.Sprintf(
							"--listen-address :%d",
							oldProcessGroup.PortBlock.MetricsPort(),
						),
					))
					expectNonOverlappingBlocks(cluster)
				},
			)

			It("should replace the storage process groups with blocks for one server", func() {
				result, err := reconcileCluster(cluster)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(BeZero())
				_, err = reloadCluster(cluster)
				Expect(err).NotTo(HaveOccurred())

				Expect(fdbv1beta2.ContainsProcessGroupID(
					cluster.Status.ProcessGroups,
					oldProcessGroup.ProcessGroupID,
				)).To(BeFalse())
				for _, processGroup := range cluster.Status.ProcessGroups {
					if processGroup.ProcessClass == fdbv1beta2.ProcessClassStorage {
						Expect(processGroup.PortBlock.ServersPerPod).To(Equal(1))
						Expect(processGroup.PortBlock.Size()).To(Equal(3))
					}
				}
				expectNonOverlappingBlocks(cluster)
			})
		})

		When("the port range has no room for another block", func() {
			var newProcessGroup *fdbv1beta2.ProcessGroupStatus
			var result *requeue

			BeforeEach(func() {
				lastPort := 0
				for _, processGroup := range cluster.Status.ProcessGroups {
					lastPort = max(lastPort, processGroup.PortBlock.End())
				}
				cluster.Spec.Routing.HostNetwork.PortRangeEnd = ptr.To(lastPort)

				newProcessGroup = fdbv1beta2.NewProcessGroupStatus(
					"storage-99",
					fdbv1beta2.ProcessClassStorage,
					nil,
				)
				cluster.Status.ProcessGroups = append(cluster.Status.ProcessGroups, newProcessGroup)

				Expect(
					internal.NormalizeClusterSpec(cluster, internal.DeprecationOptions{}),
				).To(Succeed())
				result = addPods{}.reconcile(
					context.TODO(),
					clusterReconciler,
					cluster,
					nil,
					logr.Discard(),
				)
			})

			It("should not create the pod and wait for a free block", func() {
				Expect(result).NotTo(BeNil())
				Expect(result.message).To(ContainSubstring("waiting for free port blocks"))
				Expect(newProcessGroup.PortBlock).To(BeNil())
				Expect(getClusterPods(cluster)).NotTo(HaveKey(newProcessGroup.ProcessGroupID))
			})
		})

		When("one of the process groups on the shared node IP is incompatible", func() {
			var incompatible *fdbv1beta2.ProcessGroupStatus
			var podCount int

			BeforeEach(func() {
				clusterReconciler.EnableRestartIncompatibleProcesses = true
				DeferCleanup(func() {
					clusterReconciler.EnableRestartIncompatibleProcesses = false
				})

				incompatible = getProcessGroup(cluster, fdbv1beta2.ProcessClassStorage)
				status, err := adminClient.GetStatus()
				Expect(err).NotTo(HaveOccurred())

				// The incompatible process doesn't report to the cluster, but all other processes on the same IP do.
				removedProcesses := 0
				for key, process := range status.Cluster.Processes {
					if process.Locality[fdbv1beta2.FDBLocalityInstanceIDKey] == string(
						incompatible.ProcessGroupID,
					) {
						delete(status.Cluster.Processes, key)
						removedProcesses++
					}
				}
				Expect(removedProcesses).To(Equal(2))
				// FDB reports the IP:port of the incompatible peer, here the non-TLS port of its first process.
				status.Cluster.IncompatibleConnections = []string{fmt.Sprintf(
					"%s:%d",
					incompatible.Addresses[0],
					incompatible.PortBlock.ProcessPort(1, false),
				)}
				status.Client.DatabaseStatus.Available = true
				status.Cluster.FaultTolerance = fdbv1beta2.FaultTolerance{
					MaxZoneFailuresWithoutLosingAvailability: 2,
					MaxZoneFailuresWithoutLosingData:         2,
				}
				adminClient.FrozenStatus = status
				podCount = len(getClusterPods(cluster))

				Expect(processIncompatibleProcesses(
					context.TODO(),
					clusterReconciler,
					logr.Discard(),
					cluster,
					nil,
				)).To(Succeed())
			})

			It("should only recreate the pod of the incompatible process group", func() {
				pods := getClusterPods(cluster)
				Expect(pods).To(HaveLen(podCount - 1))
				Expect(pods).NotTo(HaveKey(incompatible.ProcessGroupID))
			})
		})
	})

	When("turning host networking on and off for a running cluster", func() {
		BeforeEach(func() {
			cluster = newHostNetworkCluster(false)
			Expect(k8sClient.Create(context.TODO(), cluster)).To(Succeed())

			result, err := reconcileCluster(cluster)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeZero())
			_, err = reloadCluster(cluster)
			Expect(err).NotTo(HaveOccurred())

			for _, pod := range getClusterPods(cluster) {
				Expect(pod.Spec.HostNetwork).To(BeFalse())
			}
			Expect(cluster.HasPortBlocks()).To(BeFalse())
		})

		It("should move every pod to the host network and back", func() {
			cluster.Spec.Routing.HostNetwork.Enabled = ptr.To(true)
			Expect(k8sClient.Update(context.TODO(), cluster)).To(Succeed())
			result, err := reconcileCluster(cluster)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeZero())
			_, err = reloadCluster(cluster)
			Expect(err).NotTo(HaveOccurred())

			expectNonOverlappingBlocks(cluster)
			pods := getClusterPods(cluster)
			for _, processGroup := range cluster.Status.ProcessGroups {
				pod := pods[processGroup.ProcessGroupID]
				Expect(pod.Spec.HostNetwork).To(BeTrue(), "pod of %s", processGroup.ProcessGroupID)
				start, _ := getMainContainerEnv(pod, fdbv1beta2.EnvNamePortBlockStart)
				Expect(start).To(Equal(strconv.Itoa(processGroup.PortBlock.Start)))
			}

			cluster.Spec.Routing.HostNetwork.Enabled = ptr.To(false)
			Expect(k8sClient.Update(context.TODO(), cluster)).To(Succeed())
			result, err = reconcileCluster(cluster)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeZero())
			_, err = reloadCluster(cluster)
			Expect(err).NotTo(HaveOccurred())

			Expect(cluster.HasPortBlocks()).To(BeFalse())
			for _, pod := range getClusterPods(cluster) {
				Expect(pod.Spec.HostNetwork).To(BeFalse())
				_, ok := getMainContainerEnv(pod, fdbv1beta2.EnvNamePortBlockStart)
				Expect(ok).To(BeFalse())
			}

			configMap := &corev1.ConfigMap{}
			Expect(k8sClient.Get(context.TODO(), client.ObjectKey{
				Namespace: cluster.Namespace,
				Name:      cluster.Name + "-config",
			}, configMap)).To(Succeed())
			for key := range configMap.Data {
				Expect(key).NotTo(ContainSubstring("host-network"))
			}
		})
	})

	When("the operator doesn't skip the version check", func() {
		BeforeEach(func() {
			clusterReconciler.AllowedPodModifications = nil
			cluster = newHostNetworkCluster(true)
			Expect(k8sClient.Create(context.TODO(), cluster)).To(Succeed())
		})

		It("should not create any pods", func() {
			_, err := reconcileCluster(cluster)
			Expect(
				err,
			).To(MatchError(ContainSubstring("fdb-kubernetes-monitor supports the Sum argument")))
			pods := &corev1.PodList{}
			Expect(k8sClient.List(context.TODO(), pods, getListOptions(cluster)...)).To(Succeed())
			Expect(pods.Items).To(BeEmpty())
		})
	})
})
