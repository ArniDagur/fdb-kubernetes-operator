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

package v1beta2

import (
	"net"

	"github.com/apple/foundationdb/fdbkubernetesmonitor/api"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
)

var _ = Describe("[api] host networking", func() {
	When("using a port block", func() {
		// A block for two processes starting at 4530: 4530/4531 for process 1, 4532/4533 for process 2, and the
		// metrics port 4534.
		block := PortBlock{Start: 4530, ServersPerPod: 2}

		It("should have a TLS and a non-TLS port per process and the metrics port", func() {
			Expect(block.Size()).To(Equal(5))
			Expect(block.End()).To(Equal(4534))
			Expect(block.ProcessPort(1, true)).To(Equal(4530))
			Expect(block.ProcessPort(1, false)).To(Equal(4531))
			Expect(block.ProcessPort(2, true)).To(Equal(4532))
			Expect(block.ProcessPort(2, false)).To(Equal(4533))
			Expect(block.MetricsPort()).To(Equal(4534))
		})

		It("should keep the default layout for pods without a block", func() {
			Expect(GetProcessPortFromStart(DefaultProcessPortStart, 3, true)).
				To(Equal(GetProcessPort(3, true)))
			Expect(GetProcessPortFromStart(DefaultProcessPortStart, 3, false)).
				To(Equal(GetProcessPort(3, false)))
		})
	})

	When("getting the network addresses of a process group", func() {
		var cluster *FoundationDBCluster
		var processGroup *ProcessGroupStatus

		BeforeEach(func() {
			cluster = &FoundationDBCluster{
				Status: FoundationDBClusterStatus{
					RequiredAddresses: RequiredAddressSet{TLS: true},
				},
			}
			processGroup = &ProcessGroupStatus{
				ProcessGroupID: "storage-1",
				Addresses:      []string{"10.0.0.5"},
			}
		})

		It("should return the bare IPs without a port block", func() {
			Expect(cluster.GetProcessGroupNetworkAddresses(processGroup)).To(ConsistOf(
				ProcessAddress{IPAddress: net.ParseIP("10.0.0.5")},
			))
		})

		When("the process group has a port block", func() {
			BeforeEach(func() {
				processGroup.PortBlock = &PortBlock{Start: 4530, ServersPerPod: 2}
			})

			It("should return the IP:port of each process in the required address mode", func() {
				Expect(cluster.GetProcessGroupNetworkAddresses(processGroup)).To(ConsistOf(
					ProcessAddress{IPAddress: net.ParseIP("10.0.0.5"), Port: 4530},
					ProcessAddress{IPAddress: net.ParseIP("10.0.0.5"), Port: 4532},
				))
			})

			It("should return the primary address of a process", func() {
				Expect(cluster.GetProcessGroupFullAddress(processGroup, "10.0.0.5", 2).String()).
					To(Equal("10.0.0.5:4532:tls"))
			})

			When("the required addresses are not known yet", func() {
				BeforeEach(func() {
					cluster.Status.RequiredAddresses = RequiredAddressSet{}
				})

				It("should return both the TLS and the non-TLS addresses", func() {
					Expect(cluster.GetProcessGroupNetworkAddresses(processGroup)).To(ConsistOf(
						ProcessAddress{IPAddress: net.ParseIP("10.0.0.5"), Port: 4530},
						ProcessAddress{IPAddress: net.ParseIP("10.0.0.5"), Port: 4531},
						ProcessAddress{IPAddress: net.ParseIP("10.0.0.5"), Port: 4532},
						ProcessAddress{IPAddress: net.ParseIP("10.0.0.5"), Port: 4533},
					))
				})
			})
		})
	})

	When("checking the version support for host networking", func() {
		BeforeEach(func() {
			previous := hostNetworkingMinimumVersions
			hostNetworkingMinimumVersions = []Version{
				{api.Version{Major: 7, Minor: 3, Patch: 80}},
			}
			DeferCleanup(func() {
				hostNetworkingMinimumVersions = previous
			})
		})

		DescribeTable(
			"it should only support versions from the minimum of the same branch",
			func(version Version, expected bool) {
				Expect(version.SupportsHostNetworking()).To(Equal(expected))
			},
			Entry("the minimum version", Version{api.Version{Major: 7, Minor: 3, Patch: 80}}, true),
			Entry(
				"a later patch version",
				Version{api.Version{Major: 7, Minor: 3, Patch: 81}},
				true,
			),
			Entry(
				"an earlier patch version",
				Version{api.Version{Major: 7, Minor: 3, Patch: 79}},
				false,
			),
			Entry(
				"a branch without a minimum",
				Version{api.Version{Major: 7, Minor: 1, Patch: 99}},
				false,
			),
		)

		It("should not support any released version yet", func() {
			hostNetworkingMinimumVersions = nil
			Expect(Versions.Default.SupportsHostNetworking()).To(BeFalse())
		})
	})

	When("validating a cluster with host networking", func() {
		var cluster *FoundationDBCluster
		var mods *AllowedPodModifications

		BeforeEach(func() {
			cluster = &FoundationDBCluster{
				Spec: FoundationDBClusterSpec{
					Version: Versions.Default.String(),
					DatabaseConfiguration: DatabaseConfiguration{
						StorageEngine: StorageEngineSSD2,
					},
					Routing: RoutingConfig{
						HostNetwork: &HostNetworkConfig{
							Enabled:        ptr.To(true),
							PortRangeStart: ptr.To(20000),
							PortRangeEnd:   ptr.To(29999),
						},
					},
				},
			}
			mods = &AllowedPodModifications{SkipHostNetworkVersionCheck: ptr.To(true)}
		})

		It("should accept a supported configuration", func() {
			Expect(cluster.Validate(mods)).To(Succeed())
		})

		It("should reject FDB versions whose monitor lacks the Sum argument", func() {
			Expect(cluster.Validate(nil)).To(MatchError(ContainSubstring(
				"host networking requires an FDB version whose fdb-kubernetes-monitor supports the Sum argument, version 7.1.57 does not",
			)))
		})

		It("should reject a running version whose monitor lacks the Sum argument", func() {
			previous := hostNetworkingMinimumVersions
			hostNetworkingMinimumVersions = []Version{Versions.Default}
			DeferCleanup(func() {
				hostNetworkingMinimumVersions = previous
			})
			cluster.Status.RunningVersion = "7.1.56"

			err := cluster.Validate(nil)
			Expect(err).To(MatchError(ContainSubstring("the running version 7.1.56 does not")))
			Expect(err).NotTo(MatchError(ContainSubstring("version 7.1.57 does not")))
		})

		DescribeTable("it should reject unsupported combinations",
			func(modify func(*FoundationDBCluster, *AllowedPodModifications), expected string) {
				modify(cluster, mods)
				Expect(cluster.Validate(mods)).To(MatchError(ContainSubstring(expected)))
			},
			Entry(
				"a missing port range",
				func(cluster *FoundationDBCluster, _ *AllowedPodModifications) {
					cluster.Spec.Routing.HostNetwork.PortRangeEnd = nil
				},
				"host networking requires spec.routing.hostNetwork.portRangeStart and spec.routing.hostNetwork.portRangeEnd",
			),
			Entry(
				"a port range that ends before it starts",
				func(cluster *FoundationDBCluster, _ *AllowedPodModifications) {
					cluster.Spec.Routing.HostNetwork.PortRangeEnd = ptr.To(19999)
				},
				"spec.routing.hostNetwork.portRangeEnd (19999) must be greater than spec.routing.hostNetwork.portRangeStart (20000)",
			),
			Entry("a port range too small for the desired process groups",
				func(cluster *FoundationDBCluster, _ *AllowedPodModifications) {
					// A single process group already needs 3 ports.
					cluster.Spec.Routing.HostNetwork.PortRangeEnd = ptr.To(20001)
				},
				"the host network port range 20000-20001 has 2 ports",
			),
			Entry("the split image",
				func(cluster *FoundationDBCluster, _ *AllowedPodModifications) {
					cluster.Spec.ImageType = ptr.To(ImageTypeSplit)
				},
				"host networking requires the unified image",
			),
			Entry("disabled locality based exclusions",
				func(cluster *FoundationDBCluster, _ *AllowedPodModifications) {
					cluster.Spec.AutomationOptions.UseLocalitiesForExclusion = ptr.To(false)
				},
				"host networking requires locality based exclusions",
			),
			Entry("the service public IP source",
				func(cluster *FoundationDBCluster, _ *AllowedPodModifications) {
					cluster.Spec.Routing.PublicIPSource = ptr.To(PublicIPSourceService)
				},
				"host networking requires the public IP source pod",
			),
			Entry("an operator that forbids host networking",
				func(_ *FoundationDBCluster, mods *AllowedPodModifications) {
					mods.AllowHostNetwork = ptr.To(false)
				},
				"host networking is not allowed by the operator",
			),
			Entry("the global synchronization mode",
				func(cluster *FoundationDBCluster, _ *AllowedPodModifications) {
					cluster.Spec.AutomationOptions.SynchronizationMode = ptr.To(
						string(SynchronizationModeGlobal),
					)
				},
				"host networking is only supported with the local synchronization mode",
			),
		)

		It(
			"should reject a pod template that sets the port block variable, even without host networking",
			func() {
				cluster.Spec.Routing.HostNetwork = nil
				cluster.Spec.Processes = map[ProcessClass]ProcessSettings{
					ProcessClassGeneral: {
						PodTemplate: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{
									Name: MainContainerName,
									Env: []corev1.EnvVar{
										{Name: EnvNamePortBlockStart, Value: "4500"},
									},
								}},
							},
						},
					},
				}

				Expect(cluster.Validate(nil)).To(MatchError(ContainSubstring(
					"the environment variable FDB_PORT_BLOCK_START is managed by the operator",
				)))
			},
		)

		It("should not validate host networking settings when it is disabled", func() {
			cluster.Spec.Routing.HostNetwork.Enabled = ptr.To(false)
			cluster.Spec.Routing.HostNetwork.PortRangeEnd = nil
			Expect(cluster.Validate(nil)).To(Succeed())
		})
	})
})
