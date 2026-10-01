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
	"github.com/FoundationDB/fdb-kubernetes-operator/v2/internal"
	"github.com/FoundationDB/fdb-kubernetes-operator/v2/pkg/fdbadminclient/mock"
	"github.com/FoundationDB/fdb-kubernetes-operator/v2/pkg/fdbstatus"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
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

// getProcessGroupByID returns the process group with the given ID, or nil.
func getProcessGroupByID(
	cluster *fdbv1beta2.FoundationDBCluster,
	processGroupID fdbv1beta2.ProcessGroupID,
) *fdbv1beta2.ProcessGroupStatus {
	for _, processGroup := range cluster.Status.ProcessGroups {
		if processGroup.ProcessGroupID == processGroupID {
			return processGroup
		}
	}

	return nil
}

// reconcileAndReload reconciles the cluster and loads the result.
func reconcileAndReload(cluster *fdbv1beta2.FoundationDBCluster) time.Duration {
	result, err := reconcileCluster(cluster)
	Expect(err).NotTo(HaveOccurred())
	_, err = reloadCluster(cluster)
	Expect(err).NotTo(HaveOccurred())
	return result.RequeueAfter
}

// setTestChangeEnv changes the pod template, so that every pod has to be recreated.
func setTestChangeEnv(cluster *fdbv1beta2.FoundationDBCluster) {
	cluster.Spec.Processes = map[fdbv1beta2.ProcessClass]fdbv1beta2.ProcessSettings{
		fdbv1beta2.ProcessClassGeneral: {PodTemplate: &corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Name: fdbv1beta2.MainContainerName,
					Env:  []corev1.EnvVar{{Name: "TEST_CHANGE", Value: "1"}},
				}},
			},
		}},
	}
}

var _ = Describe("coordinator forwarding", func() {
	var cluster *fdbv1beta2.FoundationDBCluster
	var adminClient *mock.AdminClient
	var originalCoordinators map[string]fdbv1beta2.None
	var originalPodUIDs map[fdbv1beta2.ProcessGroupID]types.UID

	BeforeEach(func() {
		cluster = internal.CreateDefaultCluster()
		cluster.Spec.ImageType = ptr.To(fdbv1beta2.ImageTypeUnified)
		cluster.Spec.AutomationOptions.CoordinatorForwardingGracePeriodSeconds = ptr.To(3600)
		// With IP addresses in the cluster file, every recreated pod comes back with a new address.
		cluster.Spec.Routing.UseDNSInClusterFile = ptr.To(false)
		Expect(k8sClient.Create(context.TODO(), cluster)).To(Succeed())
		Expect(reconcileAndReload(cluster)).To(BeZero())

		var err error
		adminClient, err = mock.NewMockAdminClientUncast(cluster, k8sClient)
		Expect(err).NotTo(HaveOccurred())

		originalCoordinators = currentCoordinators(adminClient)
		Expect(originalCoordinators).To(HaveLen(cluster.DesiredCoordinatorCount()))
		originalPodUIDs = map[fdbv1beta2.ProcessGroupID]types.UID{}
		for processGroupID, pod := range getClusterPods(cluster) {
			originalPodUIDs[processGroupID] = pod.UID
		}
	})

	// expectPreviousCoordinatorsKept checks that every previous coordinator still exists with its original pod and is
	// marked as forwarding coordinator.
	expectPreviousCoordinatorsKept := func() {
		pods := getClusterPods(cluster)
		for processGroupID := range originalCoordinators {
			processGroup := getProcessGroupByID(cluster, fdbv1beta2.ProcessGroupID(processGroupID))
			Expect(
				processGroup,
			).NotTo(BeNil(), "previous coordinator %s should still exist", processGroupID)
			Expect(processGroup.ForwardingCoordinatorSince).NotTo(BeNil())

			pod := pods[fdbv1beta2.ProcessGroupID(processGroupID)]
			Expect(pod).NotTo(BeNil())
			Expect(pod.UID).To(Equal(originalPodUIDs[fdbv1beta2.ProcessGroupID(processGroupID)]),
				"the pod of %s should not be recreated", processGroupID)
		}
	}

	When("the public IP source changes", func() {
		BeforeEach(func() {
			cluster.Spec.Routing.PublicIPSource = ptr.To(fdbv1beta2.PublicIPSourceService)
			Expect(k8sClient.Update(context.TODO(), cluster)).To(Succeed())
			// The previous coordinators are still waiting for their grace period.
			Expect(reconcileAndReload(cluster)).To(BeNumerically(">", 0))
		})

		It(
			"should move the coordinators to process groups that already use the new IP source",
			func() {
				coordinators := currentCoordinators(adminClient)
				Expect(coordinators).To(HaveLen(cluster.DesiredCoordinatorCount()))
				pods := getClusterPods(cluster)
				for processGroupID := range coordinators {
					Expect(originalCoordinators).NotTo(HaveKey(processGroupID))
					pod := pods[fdbv1beta2.ProcessGroupID(processGroupID)]
					Expect(pod).NotTo(BeNil())
					Expect(
						pod.Annotations[fdbv1beta2.PublicIPSourceAnnotation],
					).To(Equal("service"))
				}
			},
		)

		It("should keep the previous coordinators running during the grace period", func() {
			expectPreviousCoordinatorsKept()
		})

		It("should remove the previous coordinators after the grace period", func() {
			endForwardingGracePeriods(cluster)
			Expect(reconcileAndReload(cluster)).To(BeZero())
			for processGroupID := range originalCoordinators {
				Expect(
					getProcessGroupByID(cluster, fdbv1beta2.ProcessGroupID(processGroupID)),
				).To(BeNil())
			}
			for _, pod := range getClusterPods(cluster) {
				Expect(pod.Annotations[fdbv1beta2.PublicIPSourceAnnotation]).To(Equal("service"))
			}
		})
	})

	When("the public IP source changes with one replacement at a time", func() {
		BeforeEach(func() {
			cluster.Spec.AutomationOptions.Replacements.MaxConcurrentReplacements = ptr.To(1)
			cluster.Spec.Routing.PublicIPSource = ptr.To(fdbv1beta2.PublicIPSourceService)
			Expect(k8sClient.Update(context.TODO(), cluster)).To(Succeed())
			Expect(reconcileAndReload(cluster)).To(BeNumerically(">", 0))
		})

		It(
			"should only move the coordinators to process groups that already use the new IP source",
			func() {
				// Process groups that are not replaced yet still use the old IP source and must not become coordinators.
				pods := getClusterPods(cluster)
				for processGroupID := range currentCoordinators(adminClient) {
					pod := pods[fdbv1beta2.ProcessGroupID(processGroupID)]
					Expect(pod).NotTo(BeNil())
					Expect(
						pod.Annotations[fdbv1beta2.PublicIPSourceAnnotation],
					).To(Equal("service"),
						"coordinator %s should already use the new IP source", processGroupID)
				}
			},
		)

		It(
			"should move the coordinators only once, so only the original coordinators are held back",
			func() {
				forwarding := map[string]fdbv1beta2.None{}
				for _, processGroup := range cluster.Status.ProcessGroups {
					if processGroup.ForwardingCoordinatorSince != nil {
						forwarding[string(processGroup.ProcessGroupID)] = fdbv1beta2.None{}
					}
				}
				Expect(forwarding).To(Equal(originalCoordinators))
			},
		)
	})

	When("pods are recreated in place while the cluster file uses IP addresses", func() {
		BeforeEach(func() {
			cluster.Spec.AutomationOptions.PodUpdateStrategy = fdbv1beta2.PodUpdateStrategyDelete
			setTestChangeEnv(cluster)
			Expect(k8sClient.Update(context.TODO(), cluster)).To(Succeed())
			Expect(reconcileAndReload(cluster)).To(BeNumerically(">", 0))
		})

		It(
			"should move the coordinators to process groups whose pods were already recreated",
			func() {
				coordinators := currentCoordinators(adminClient)
				Expect(coordinators).To(HaveLen(cluster.DesiredCoordinatorCount()))
				pods := getClusterPods(cluster)
				for processGroupID := range coordinators {
					Expect(originalCoordinators).NotTo(HaveKey(processGroupID))
					pod := pods[fdbv1beta2.ProcessGroupID(processGroupID)]
					Expect(
						pod.UID,
					).NotTo(Equal(originalPodUIDs[fdbv1beta2.ProcessGroupID(processGroupID)]),
						"new coordinator %s should already run the new pod spec", processGroupID)
				}
			},
		)

		It(
			"should recreate the pods of the previous coordinators only after the grace period",
			func() {
				expectPreviousCoordinatorsKept()

				endForwardingGracePeriods(cluster)
				Expect(reconcileAndReload(cluster)).To(BeZero())
				pods := getClusterPods(cluster)
				for processGroupID := range originalCoordinators {
					Expect(pods[fdbv1beta2.ProcessGroupID(processGroupID)].UID).
						NotTo(Equal(originalPodUIDs[fdbv1beta2.ProcessGroupID(processGroupID)]))
				}
				for _, processGroup := range cluster.Status.ProcessGroups {
					Expect(processGroup.ForwardingCoordinatorSince).To(BeNil())
				}
			},
		)
	})

	When("pods are recreated in place while the cluster file uses DNS names", func() {
		var dnsCluster *fdbv1beta2.FoundationDBCluster

		BeforeEach(func() {
			// A separate cluster that uses DNS names in its cluster file from the start.
			dnsCluster = internal.CreateDefaultCluster()
			dnsCluster.Name = "operator-test-dns"
			dnsCluster.Spec.ImageType = ptr.To(fdbv1beta2.ImageTypeUnified)
			dnsCluster.Spec.AutomationOptions.CoordinatorForwardingGracePeriodSeconds = ptr.To(3600)
			dnsCluster.Spec.Routing.UseDNSInClusterFile = ptr.To(true)
			Expect(k8sClient.Create(context.TODO(), dnsCluster)).To(Succeed())
			Expect(reconcileAndReload(dnsCluster)).To(BeZero())
		})

		It("should not move the coordinators, as their address doesn't change", func() {
			dnsAdminClient, err := mock.NewMockAdminClientUncast(dnsCluster, k8sClient)
			Expect(err).NotTo(HaveOccurred())
			coordinators := currentCoordinators(dnsAdminClient)
			connectionString := dnsCluster.Status.ConnectionString
			podUIDs := map[fdbv1beta2.ProcessGroupID]types.UID{}
			for processGroupID, pod := range getClusterPods(dnsCluster) {
				podUIDs[processGroupID] = pod.UID
			}

			dnsCluster.Spec.AutomationOptions.PodUpdateStrategy = fdbv1beta2.PodUpdateStrategyDelete
			setTestChangeEnv(dnsCluster)
			Expect(k8sClient.Update(context.TODO(), dnsCluster)).To(Succeed())
			Expect(reconcileAndReload(dnsCluster)).To(BeZero())

			// Every pod was recreated, but the coordinators stayed the same.
			for processGroupID, pod := range getClusterPods(dnsCluster) {
				Expect(pod.UID).NotTo(Equal(podUIDs[processGroupID]))
			}
			Expect(currentCoordinators(dnsAdminClient)).To(Equal(coordinators))
			Expect(dnsCluster.Status.ConnectionString).To(Equal(connectionString))
			for _, processGroup := range dnsCluster.Status.ProcessGroups {
				Expect(processGroup.ForwardingCoordinatorSince).To(BeNil())
			}
		})
	})

	When("a coordinator process group is removed", func() {
		var coordinatorID fdbv1beta2.ProcessGroupID

		BeforeEach(func() {
			for processGroupID := range originalCoordinators {
				coordinatorID = fdbv1beta2.ProcessGroupID(processGroupID)
				break
			}
			cluster.Spec.ProcessGroupsToRemove = []fdbv1beta2.ProcessGroupID{coordinatorID}
			Expect(k8sClient.Update(context.TODO(), cluster)).To(Succeed())
		})

		When("its process is running", func() {
			BeforeEach(func() {
				Expect(reconcileAndReload(cluster)).To(BeNumerically(">", 0))
			})

			It(
				"should move the coordinators and keep the process group until the grace period is over",
				func() {
					Expect(currentCoordinators(adminClient)).NotTo(HaveKey(string(coordinatorID)))
					Expect(getClusterPods(cluster)).To(HaveKey(coordinatorID))
					Expect(getProcessGroupByID(cluster, coordinatorID)).NotTo(BeNil())

					endForwardingGracePeriods(cluster)
					Expect(reconcileAndReload(cluster)).To(BeZero())
					Expect(getProcessGroupByID(cluster, coordinatorID)).To(BeNil())
					Expect(getClusterPods(cluster)).NotTo(HaveKey(coordinatorID))
				},
			)
		})

		When("its process stops during the grace period", func() {
			It("should remove it without waiting for the rest of the grace period", func() {
				Expect(reconcileAndReload(cluster)).To(BeNumerically(">", 0))
				Expect(getProcessGroupByID(cluster, coordinatorID)).NotTo(BeNil())

				// A process that doesn't run can't forward clients, so there is no reason to keep it.
				adminClient.MockMissingProcessGroup(coordinatorID, true)
				Expect(reconcileAndReload(cluster)).To(BeZero())
				Expect(getProcessGroupByID(cluster, coordinatorID)).To(BeNil())
			})
		})

		When("its process is not running", func() {
			It("should remove it without waiting, as it can't forward clients", func() {
				adminClient.MockMissingProcessGroup(coordinatorID, true)
				Expect(reconcileAndReload(cluster)).To(BeZero())
				Expect(getProcessGroupByID(cluster, coordinatorID)).To(BeNil())
			})
		})

		When("the grace period is disabled", func() {
			It("should remove it right away", func() {
				cluster.Spec.AutomationOptions.CoordinatorForwardingGracePeriodSeconds = nil
				Expect(k8sClient.Update(context.TODO(), cluster)).To(Succeed())
				Expect(reconcileAndReload(cluster)).To(BeZero())
				Expect(getProcessGroupByID(cluster, coordinatorID)).To(BeNil())
				for _, processGroup := range cluster.Status.ProcessGroups {
					Expect(processGroup.ForwardingCoordinatorSince).To(BeNil())
				}
			})
		})
	})
})
