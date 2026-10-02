/*
 * host_network_test.go
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

package internal

import (
	fdbv1beta2 "github.com/FoundationDB/fdb-kubernetes-operator/v2/api/v1beta2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
)

var _ = Describe("the hostNetwork public IP source", func() {
	It(
		"should run the pod on the host network with cluster DNS, unless the template sets a DNS policy",
		func() {
			cluster := CreateDefaultCluster()
			cluster.Spec.Routing.PublicIPSource = ptr.To(fdbv1beta2.PublicIPSourceHostNetwork)
			Expect(NormalizeClusterSpec(cluster, DeprecationOptions{})).To(Succeed())
			processGroup := GetProcessGroup(cluster, fdbv1beta2.ProcessClassStorage, 1)

			spec, err := GetPodSpec(cluster, processGroup)
			Expect(err).NotTo(HaveOccurred())
			Expect(spec.HostNetwork).To(BeTrue())
			Expect(spec.DNSPolicy).To(Equal(corev1.DNSClusterFirstWithHostNet))

			cluster.Spec.Processes = map[fdbv1beta2.ProcessClass]fdbv1beta2.ProcessSettings{
				fdbv1beta2.ProcessClassGeneral: {PodTemplate: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{DNSPolicy: corev1.DNSDefault},
				}},
			}
			Expect(NormalizeClusterSpec(cluster, DeprecationOptions{})).To(Succeed())
			spec, err = GetPodSpec(cluster, processGroup)
			Expect(err).NotTo(HaveOccurred())
			Expect(spec.DNSPolicy).To(Equal(corev1.DNSDefault))
		},
	)
})
