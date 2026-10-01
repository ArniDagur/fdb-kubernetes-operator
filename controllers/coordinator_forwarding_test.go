/*
 * coordinator_forwarding_test.go
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
	"time"

	fdbv1beta2 "github.com/FoundationDB/fdb-kubernetes-operator/v2/api/v1beta2"
	"github.com/FoundationDB/fdb-kubernetes-operator/v2/pkg/fdbadminclient/mock"
	"github.com/FoundationDB/fdb-kubernetes-operator/v2/pkg/fdbstatus"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
)

// currentCoordinators returns the process group IDs of the processes that report the coordinator role.
func currentCoordinators(adminClient *mock.AdminClient) map[string]fdbv1beta2.None {
	status, err := adminClient.GetStatus()
	Expect(err).NotTo(HaveOccurred())
	return fdbstatus.GetCoordinatorsFromStatus(status)
}

// endForwardingGracePeriods moves the start of every forwarding grace period into the past.
func endForwardingGracePeriods(cluster *fdbv1beta2.FoundationDBCluster) {
	for _, processGroup := range cluster.Status.ProcessGroups {
		if processGroup.ForwardingCoordinatorSince != nil {
			processGroup.ForwardingCoordinatorSince = &metav1.Time{
				Time: time.Now().Add(-24 * time.Hour),
			}
		}
	}
	Expect(k8sClient.Status().Update(context.TODO(), cluster)).To(Succeed())
}

var _ = Describe("coordinator forwarding", func() {
	var cluster *fdbv1beta2.FoundationDBCluster
	var adminClient *mock.AdminClient

	BeforeEach(func() {
		// No released FDB version ships the Sum argument of the fdb-kubernetes-monitor yet.
		clusterReconciler.AllowedPodModifications = &fdbv1beta2.AllowedPodModifications{
			SkipHostNetworkVersionCheck: ptr.To(true),
		}
		cluster = newHostNetworkCluster(false)
		cluster.Spec.AutomationOptions.CoordinatorForwardingGracePeriodSeconds = ptr.To(3600)
		Expect(k8sClient.Create(context.TODO(), cluster)).To(Succeed())

		result, err := reconcileCluster(cluster)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(BeZero())
		_, err = reloadCluster(cluster)
		Expect(err).NotTo(HaveOccurred())

		adminClient, err = mock.NewMockAdminClientUncast(cluster, k8sClient)
		Expect(err).NotTo(HaveOccurred())
	})

	DescribeTableSubtree("host networking is enabled for a running cluster",
		func(strategy fdbv1beta2.PodUpdateStrategy) {
			var originalCoordinators map[string]fdbv1beta2.None
			var originalPodUIDs map[fdbv1beta2.ProcessGroupID]types.UID

			BeforeEach(func() {
				originalCoordinators = currentCoordinators(adminClient)
				Expect(originalCoordinators).To(HaveLen(cluster.DesiredCoordinatorCount()))
				originalPodUIDs = map[fdbv1beta2.ProcessGroupID]types.UID{}
				for processGroupID, pod := range getClusterPods(cluster) {
					originalPodUIDs[processGroupID] = pod.UID
				}

				cluster.Spec.AutomationOptions.PodUpdateStrategy = strategy
				cluster.Spec.Routing.HostNetwork.Enabled = ptr.To(true)
				Expect(k8sClient.Update(context.TODO(), cluster)).To(Succeed())
				result, err := reconcileCluster(cluster)
				Expect(err).NotTo(HaveOccurred())
				// The previous coordinators are still waiting for their grace period.
				Expect(result.RequeueAfter).To(BeNumerically(">", 0))
				_, err = reloadCluster(cluster)
				Expect(err).NotTo(HaveOccurred())
			})

			It("should move the coordinators to host-networked process groups first", func() {
				coordinators := currentCoordinators(adminClient)
				Expect(coordinators).To(HaveLen(cluster.DesiredCoordinatorCount()))
				pods := getClusterPods(cluster)
				for processGroupID := range coordinators {
					Expect(originalCoordinators).NotTo(HaveKey(processGroupID))
					pod := pods[fdbv1beta2.ProcessGroupID(processGroupID)]
					Expect(pod).NotTo(BeNil())
					Expect(
						pod.Spec.HostNetwork,
					).To(BeTrue(), "coordinator %s should be host-networked", processGroupID)
				}
			})

			It(
				"should keep the previous coordinators running at their old address during the grace period",
				func() {
					pods := getClusterPods(cluster)
					for processGroupID := range originalCoordinators {
						var processGroup *fdbv1beta2.ProcessGroupStatus
						for _, current := range cluster.Status.ProcessGroups {
							if current.ProcessGroupID == fdbv1beta2.ProcessGroupID(processGroupID) {
								processGroup = current
							}
						}
						Expect(
							processGroup,
						).NotTo(BeNil(), "previous coordinator %s should still exist", processGroupID)
						Expect(processGroup.ForwardingCoordinatorSince).NotTo(BeNil())

						pod := pods[fdbv1beta2.ProcessGroupID(processGroupID)]
						Expect(pod).NotTo(BeNil())
						Expect(
							pod.UID,
						).To(Equal(originalPodUIDs[fdbv1beta2.ProcessGroupID(processGroupID)]),
							"the pod of %s should not be recreated", processGroupID)
						Expect(pod.Spec.HostNetwork).To(BeFalse())
					}
				},
			)

			It("should move every other process group to the host network", func() {
				for processGroupID, pod := range getClusterPods(cluster) {
					if _, wasCoordinator := originalCoordinators[string(processGroupID)]; wasCoordinator {
						continue
					}
					Expect(pod.Spec.HostNetwork).To(BeTrue(), "pod of %s", processGroupID)
				}
			})

			When("the grace period is over", func() {
				BeforeEach(func() {
					endForwardingGracePeriods(cluster)
					result, err := reconcileCluster(cluster)
					Expect(err).NotTo(HaveOccurred())
					Expect(result.RequeueAfter).To(BeZero())
					_, err = reloadCluster(cluster)
					Expect(err).NotTo(HaveOccurred())
				})

				It("should finish the migration and clear the markers", func() {
					pods := getClusterPods(cluster)
					Expect(pods).To(HaveLen(len(cluster.Status.ProcessGroups)))
					for _, processGroup := range cluster.Status.ProcessGroups {
						Expect(processGroup.ForwardingCoordinatorSince).To(BeNil())
						Expect(pods[processGroup.ProcessGroupID].Spec.HostNetwork).To(BeTrue())
					}
					expectNonOverlappingBlocks(cluster)
				})
			})
		},
		// Replaces the log and stateless process groups, the coordinators move when they are excluded.
		Entry("with the default update strategy", fdbv1beta2.PodUpdateStrategy("")),
		// Recreates every pod in place, the coordinators move before their pods are recreated.
		Entry("with the Delete update strategy", fdbv1beta2.PodUpdateStrategyDelete),
	)

	When("a coordinator process group is removed", func() {
		var coordinatorID fdbv1beta2.ProcessGroupID

		BeforeEach(func() {
			for processGroupID := range currentCoordinators(adminClient) {
				coordinatorID = fdbv1beta2.ProcessGroupID(processGroupID)
				break
			}
			cluster.Spec.ProcessGroupsToRemove = []fdbv1beta2.ProcessGroupID{coordinatorID}
			Expect(k8sClient.Update(context.TODO(), cluster)).To(Succeed())
		})

		When("its process is running", func() {
			BeforeEach(func() {
				result, err := reconcileCluster(cluster)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(BeNumerically(">", 0))
				_, err = reloadCluster(cluster)
				Expect(err).NotTo(HaveOccurred())
			})

			It(
				"should move the coordinators and keep the process group until the grace period is over",
				func() {
					Expect(currentCoordinators(adminClient)).NotTo(HaveKey(string(coordinatorID)))
					Expect(getClusterPods(cluster)).To(HaveKey(coordinatorID))
					Expect(
						fdbv1beta2.ContainsProcessGroupID(
							cluster.Status.ProcessGroups,
							coordinatorID,
						),
					).To(BeTrue())

					endForwardingGracePeriods(cluster)
					result, err := reconcileCluster(cluster)
					Expect(err).NotTo(HaveOccurred())
					Expect(result.RequeueAfter).To(BeZero())
					_, err = reloadCluster(cluster)
					Expect(err).NotTo(HaveOccurred())
					Expect(
						fdbv1beta2.ContainsProcessGroupID(
							cluster.Status.ProcessGroups,
							coordinatorID,
						),
					).To(BeFalse())
					Expect(getClusterPods(cluster)).NotTo(HaveKey(coordinatorID))
				},
			)
		})

		When("its process stops during the grace period", func() {
			It("should remove it without waiting for the rest of the grace period", func() {
				result, err := reconcileCluster(cluster)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(BeNumerically(">", 0))
				_, err = reloadCluster(cluster)
				Expect(err).NotTo(HaveOccurred())
				Expect(
					fdbv1beta2.ContainsProcessGroupID(cluster.Status.ProcessGroups, coordinatorID),
				).To(BeTrue())

				// A process that doesn't run can't forward clients, so there is no reason to keep it.
				adminClient.MockMissingProcessGroup(coordinatorID, true)
				result, err = reconcileCluster(cluster)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(BeZero())
				_, err = reloadCluster(cluster)
				Expect(err).NotTo(HaveOccurred())
				Expect(
					fdbv1beta2.ContainsProcessGroupID(cluster.Status.ProcessGroups, coordinatorID),
				).To(BeFalse())
			})
		})

		When("its process is not running", func() {
			BeforeEach(func() {
				adminClient.MockMissingProcessGroup(coordinatorID, true)
				result, err := reconcileCluster(cluster)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(BeZero())
				_, err = reloadCluster(cluster)
				Expect(err).NotTo(HaveOccurred())
			})

			It("should remove it without waiting, as it can't forward clients", func() {
				Expect(
					fdbv1beta2.ContainsProcessGroupID(cluster.Status.ProcessGroups, coordinatorID),
				).To(BeFalse())
			})
		})
	})

	When("the grace period is disabled", func() {
		It("should remove a coordinator process group right away", func() {
			cluster.Spec.AutomationOptions.CoordinatorForwardingGracePeriodSeconds = nil
			var coordinatorID fdbv1beta2.ProcessGroupID
			for processGroupID := range currentCoordinators(adminClient) {
				coordinatorID = fdbv1beta2.ProcessGroupID(processGroupID)
				break
			}
			cluster.Spec.ProcessGroupsToRemove = []fdbv1beta2.ProcessGroupID{coordinatorID}
			Expect(k8sClient.Update(context.TODO(), cluster)).To(Succeed())

			result, err := reconcileCluster(cluster)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeZero())
			_, err = reloadCluster(cluster)
			Expect(err).NotTo(HaveOccurred())
			Expect(
				fdbv1beta2.ContainsProcessGroupID(cluster.Status.ProcessGroups, coordinatorID),
			).To(BeFalse())
			for _, processGroup := range cluster.Status.ProcessGroups {
				Expect(processGroup.ForwardingCoordinatorSince).To(BeNil())
			}
		})
	})
})
