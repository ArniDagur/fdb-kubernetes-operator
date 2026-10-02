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

package v1beta2

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/utils/ptr"
)

var _ = Describe("[api] the hostNetwork public IP source", func() {
	It("should run the pods on the host network, if the operator allows it", func() {
		cluster := &FoundationDBCluster{
			Spec: FoundationDBClusterSpec{
				Version: Versions.Default.String(),
				Routing: RoutingConfig{PublicIPSource: ptr.To(PublicIPSourceHostNetwork)},
			},
		}
		Expect(cluster.UseHostNetwork()).To(BeTrue())
		Expect(cluster.Validate(&AllowedPodModifications{})).To(Succeed())
		Expect(cluster.Validate(&AllowedPodModifications{AllowHostNetwork: ptr.To(false)})).
			To(MatchError(ContainSubstring("the public IP source hostNetwork is not allowed by the operator")))
	})
})
