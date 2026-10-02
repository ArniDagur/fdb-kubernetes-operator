/*
 * port_blocks_test.go
 *
 * This source file is part of the FoundationDB open source project
 *
 * Copyright 2018-2026 Apple Inc. and the FoundationDB project authors
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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
)

var _ = Describe("[api] port blocks", func() {
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
	})

	ip := net.ParseIP("10.0.0.5")
	tls := RequiredAddressSet{TLS: true}

	DescribeTable("getting the network addresses of a process group",
		func(block *PortBlock, required RequiredAddressSet, expected []ProcessAddress) {
			cluster := &FoundationDBCluster{
				Status: FoundationDBClusterStatus{RequiredAddresses: required},
			}
			processGroup := &ProcessGroupStatus{
				ProcessGroupID: "storage-1",
				Addresses:      []string{"10.0.0.5"},
				PortBlock:      block,
			}
			Expect(cluster.GetProcessGroupNetworkAddresses(processGroup)).To(ConsistOf(expected))
		},
		Entry("without a block, the bare IP", nil, tls, []ProcessAddress{{IPAddress: ip}}),
		Entry("with a block, the IP:port of each process in the required address mode",
			&PortBlock{Start: 4530, ServersPerPod: 2}, tls,
			[]ProcessAddress{{IPAddress: ip, Port: 4530}, {IPAddress: ip, Port: 4532}}),
		Entry("with a block while the required addresses are not known, both address modes",
			&PortBlock{Start: 4530, ServersPerPod: 2}, RequiredAddressSet{},
			[]ProcessAddress{
				{IPAddress: ip, Port: 4530},
				{IPAddress: ip, Port: 4531},
				{IPAddress: ip, Port: 4532},
				{IPAddress: ip, Port: 4533},
			}),
	)

	It("should return the primary address of a process in its block", func() {
		cluster := &FoundationDBCluster{Status: FoundationDBClusterStatus{RequiredAddresses: tls}}
		processGroup := &ProcessGroupStatus{PortBlock: &PortBlock{Start: 4530, ServersPerPod: 2}}
		Expect(cluster.GetProcessGroupFullAddress(processGroup, "10.0.0.5", 2).String()).
			To(Equal("10.0.0.5:4532:tls"))
		cluster.Status.RequiredAddresses = RequiredAddressSet{NonTLS: true}
		Expect(cluster.GetProcessGroupFullAddress(processGroup, "10.0.0.5", 2).String()).
			To(Equal("10.0.0.5:4533"))
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
						PublicIPSource: ptr.To(PublicIPSourceHostNetwork),
						PortBlocks: &PortBlocksConfig{
							PortRangeStart: 20000,
							PortRangeEnd:   29999,
						},
					},
				},
			}
			mods = &AllowedPodModifications{}
		})

		It("should accept each setting without the other", func() {
			Expect(cluster.Validate(mods)).To(Succeed())
			cluster.Spec.Routing.PublicIPSource = ptr.To(PublicIPSourcePod)
			Expect(cluster.Validate(mods)).To(Succeed())
			cluster.Spec.Routing.PublicIPSource = ptr.To(PublicIPSourceHostNetwork)
			cluster.Spec.Routing.PortBlocks = nil
			Expect(cluster.Validate(mods)).To(Succeed())
			Expect(cluster.UsePortBlocks()).To(BeFalse())
		})

		DescribeTable("it should reject unsupported combinations",
			func(modify func(*FoundationDBCluster, *AllowedPodModifications), expected string) {
				modify(cluster, mods)
				Expect(cluster.Validate(mods)).To(MatchError(ContainSubstring(expected)))
			},
			Entry(
				"a port range that ends before it starts",
				func(cluster *FoundationDBCluster, _ *AllowedPodModifications) {
					cluster.Spec.Routing.PortBlocks.PortRangeEnd = 19999
				},
				"spec.routing.portBlocks.portRangeEnd (19999) must be greater than spec.routing.portBlocks.portRangeStart (20000)",
			),
			Entry("a port range too small for the desired process groups",
				func(cluster *FoundationDBCluster, _ *AllowedPodModifications) {
					// A single process group already needs 3 ports.
					cluster.Spec.Routing.PortBlocks.PortRangeEnd = 20001
				},
				"the port block range 20000-20001 has 2 ports",
			),
			Entry("the split image",
				func(cluster *FoundationDBCluster, _ *AllowedPodModifications) {
					cluster.Spec.ImageType = ptr.To(ImageTypeSplit)
				},
				"spec.routing.portBlocks requires the unified image",
			),
			Entry("disabled locality based exclusions",
				func(cluster *FoundationDBCluster, _ *AllowedPodModifications) {
					cluster.Spec.AutomationOptions.UseLocalitiesForExclusion = ptr.To(false)
				},
				"spec.routing.portBlocks requires locality based exclusions",
			),
			Entry("the global synchronization mode",
				func(cluster *FoundationDBCluster, _ *AllowedPodModifications) {
					cluster.Spec.AutomationOptions.SynchronizationMode = ptr.To(
						string(SynchronizationModeGlobal),
					)
				},
				"spec.routing.portBlocks is only supported with the local synchronization mode",
			),
		)

		It(
			"should reject a pod template that sets the port block variable, even without host networking",
			func() {
				cluster.Spec.Routing.PublicIPSource = nil
				cluster.Spec.Routing.PortBlocks = nil
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

		It("should not validate the port block settings when both are turned off", func() {
			cluster.Spec.Routing.PublicIPSource = nil
			cluster.Spec.Routing.PortBlocks = nil
			cluster.Spec.ImageType = ptr.To(ImageTypeSplit)
			Expect(cluster.Validate(nil)).To(Succeed())
		})
	})
})
