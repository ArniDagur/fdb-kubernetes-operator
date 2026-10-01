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

package internal

import (
	"time"

	fdbv1beta2 "github.com/FoundationDB/fdb-kubernetes-operator/v2/api/v1beta2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

var _ = Describe("coordinator forwarding", func() {
	When("checking whether the address of a process group will change", func() {
		hostNetworkPod := func() *corev1.Pod {
			return &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name: fdbv1beta2.MainContainerName,
				Env:  []corev1.EnvVar{{Name: fdbv1beta2.EnvNamePortBlockStart, Value: "20000"}},
			}}}}
		}
		podNetworkPod := func() *corev1.Pod {
			return &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name: fdbv1beta2.MainContainerName,
			}}}}
		}

		DescribeTable(
			"the address will change",
			func(modify func(*fdbv1beta2.FoundationDBCluster, *fdbv1beta2.ProcessGroupStatus) *corev1.Pod, expected bool) {
				cluster := &fdbv1beta2.FoundationDBCluster{}
				processGroup := &fdbv1beta2.ProcessGroupStatus{ProcessGroupID: "storage-1"}
				pod := modify(cluster, processGroup)
				Expect(
					ProcessGroupAddressWillChange(cluster, processGroup, pod),
				).To(Equal(expected))
			},
			Entry(
				"unchanged pod-network process group",
				func(_ *fdbv1beta2.FoundationDBCluster, _ *fdbv1beta2.ProcessGroupStatus) *corev1.Pod {
					return podNetworkPod()
				},
				false,
			),
			Entry(
				"process group marked for removal",
				func(_ *fdbv1beta2.FoundationDBCluster, processGroup *fdbv1beta2.ProcessGroupStatus) *corev1.Pod {
					processGroup.MarkForRemoval()
					return podNetworkPod()
				},
				true,
			),
			Entry(
				"process group without a pod",
				func(_ *fdbv1beta2.FoundationDBCluster, _ *fdbv1beta2.ProcessGroupStatus) *corev1.Pod {
					return nil
				},
				false,
			),
			Entry(
				"host networking enabled, pod still on the pod network",
				func(cluster *fdbv1beta2.FoundationDBCluster, _ *fdbv1beta2.ProcessGroupStatus) *corev1.Pod {
					cluster.Spec.Routing.HostNetwork = &fdbv1beta2.HostNetworkConfig{
						Enabled: ptr.To(true),
					}
					return podNetworkPod()
				},
				true,
			),
			Entry(
				"host networking enabled, pod already on the host network",
				func(cluster *fdbv1beta2.FoundationDBCluster, _ *fdbv1beta2.ProcessGroupStatus) *corev1.Pod {
					cluster.Spec.Routing.HostNetwork = &fdbv1beta2.HostNetworkConfig{
						Enabled: ptr.To(true),
					}
					return hostNetworkPod()
				},
				false,
			),
			Entry(
				"host networking disabled, pod still on the host network",
				func(_ *fdbv1beta2.FoundationDBCluster, _ *fdbv1beta2.ProcessGroupStatus) *corev1.Pod {
					return hostNetworkPod()
				},
				true,
			),
			Entry(
				"public IP source changed",
				func(cluster *fdbv1beta2.FoundationDBCluster, _ *fdbv1beta2.ProcessGroupStatus) *corev1.Pod {
					cluster.Spec.Routing.PublicIPSource = ptr.To(fdbv1beta2.PublicIPSourceService)
					return podNetworkPod()
				},
				true,
			),
			Entry(
				"pod to be recreated while the cluster file uses IP addresses",
				func(cluster *fdbv1beta2.FoundationDBCluster, processGroup *fdbv1beta2.ProcessGroupStatus) *corev1.Pod {
					cluster.Spec.Routing.UseDNSInClusterFile = ptr.To(false)
					processGroup.UpdateCondition(fdbv1beta2.IncorrectPodSpec, true)
					return podNetworkPod()
				},
				true,
			),
			Entry(
				"pod to be recreated while the cluster file uses DNS names",
				func(cluster *fdbv1beta2.FoundationDBCluster, processGroup *fdbv1beta2.ProcessGroupStatus) *corev1.Pod {
					cluster.Spec.Routing.UseDNSInClusterFile = ptr.To(true)
					processGroup.UpdateCondition(fdbv1beta2.IncorrectPodSpec, true)
					return podNetworkPod()
				},
				false,
			),
		)
	})

	When("marking and clearing forwarding coordinators", func() {
		var cluster *fdbv1beta2.FoundationDBCluster
		now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)

		BeforeEach(func() {
			cluster = &fdbv1beta2.FoundationDBCluster{
				Spec: fdbv1beta2.FoundationDBClusterSpec{
					AutomationOptions: fdbv1beta2.FoundationDBClusterAutomationOptions{
						CoordinatorForwardingGracePeriodSeconds: ptr.To(600),
					},
				},
				Status: fdbv1beta2.FoundationDBClusterStatus{
					ProcessGroups: []*fdbv1beta2.ProcessGroupStatus{
						{
							ProcessGroupID: "log-1",
						}, {ProcessGroupID: "log-2"}, {ProcessGroupID: "log-3"},
						{ProcessGroupID: "log-4"},
					},
				},
			}
		})

		It("should mark the previous coordinators that are not coordinators anymore", func() {
			marked := MarkForwardingCoordinators(
				cluster,
				map[string]fdbv1beta2.None{"log-1": {}, "log-2": {}, "log-3": {}},
				[]fdbv1beta2.ProcessGroupID{"log-2", "log-4"},
				now,
			)
			Expect(marked).To(BeTrue())
			since := map[fdbv1beta2.ProcessGroupID]*metav1.Time{}
			for _, processGroup := range cluster.Status.ProcessGroups {
				since[processGroup.ProcessGroupID] = processGroup.ForwardingCoordinatorSince
			}
			Expect(since).To(Equal(map[fdbv1beta2.ProcessGroupID]*metav1.Time{
				"log-1": {Time: now}, "log-2": nil, "log-3": {Time: now}, "log-4": nil,
			}))
		})

		It("should not mark anything when the grace period is disabled", func() {
			cluster.Spec.AutomationOptions.CoordinatorForwardingGracePeriodSeconds = nil
			Expect(MarkForwardingCoordinators(
				cluster,
				map[string]fdbv1beta2.None{"log-1": {}},
				nil,
				now,
			)).To(BeFalse())
			Expect(cluster.Status.ProcessGroups[0].ForwardingCoordinatorSince).To(BeNil())
		})

		It(
			"should keep the marker during the grace period and clear it afterwards or for a coordinator",
			func() {
				for _, processGroup := range cluster.Status.ProcessGroups {
					processGroup.ForwardingCoordinatorSince = &metav1.Time{Time: now}
				}
				// log-4 became a coordinator again.
				current := map[string]fdbv1beta2.None{"log-4": {}}

				UpdateForwardingCoordinators(
					cluster,
					cluster.Status.ProcessGroups,
					current,
					now.Add(599*time.Second),
				)
				Expect(cluster.Status.ProcessGroups[0].ForwardingCoordinatorSince).NotTo(BeNil())
				Expect(cluster.Status.ProcessGroups[3].ForwardingCoordinatorSince).To(BeNil())
				Expect(
					cluster.IsForwardingCoordinator(
						cluster.Status.ProcessGroups[0],
						now.Add(599*time.Second),
					),
				).To(BeTrue())

				UpdateForwardingCoordinators(
					cluster,
					cluster.Status.ProcessGroups,
					current,
					now.Add(600*time.Second),
				)
				Expect(cluster.Status.ProcessGroups[0].ForwardingCoordinatorSince).To(BeNil())
			},
		)
	})
})
