/*
 * status_checks_port_blocks_test.go
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

package fdbstatus

import (
	"net"
	"slices"

	fdbv1beta2 "github.com/FoundationDB/fdb-kubernetes-operator/v2/api/v1beta2"
	"github.com/go-logr/logr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("status_checks with process groups that share the IP of their node", func() {
	nodeIP := net.ParseIP("10.1.0.7")
	tlsFlags := map[string]bool{"tls": true}

	// storage-1 uses the block 4530-4534 and both of its processes are fully excluded. log-1 runs on the same node
	// with the block 4535-4537 and is not excluded.
	status := &fdbv1beta2.FoundationDBStatus{
		Client: fdbv1beta2.FoundationDBStatusLocalClientInfo{
			DatabaseStatus: fdbv1beta2.FoundationDBStatusClientDBStatus{Available: true},
		},
		Cluster: fdbv1beta2.FoundationDBStatusClusterInfo{
			Processes: map[fdbv1beta2.ProcessGroupID]fdbv1beta2.FoundationDBStatusProcessInfo{
				"storage-1-1": {
					Address:  fdbv1beta2.NewProcessAddress(nodeIP, "", 4530, tlsFlags),
					Excluded: true,
					Locality: map[string]string{fdbv1beta2.FDBLocalityInstanceIDKey: "storage-1"},
				},
				"storage-1-2": {
					Address:  fdbv1beta2.NewProcessAddress(nodeIP, "", 4532, tlsFlags),
					Excluded: true,
					Locality: map[string]string{fdbv1beta2.FDBLocalityInstanceIDKey: "storage-1"},
				},
				"log-1": {
					Address: fdbv1beta2.NewProcessAddress(nodeIP, "", 4535, tlsFlags),
					Roles: []fdbv1beta2.FoundationDBStatusProcessRoleInfo{
						{Role: string(fdbv1beta2.ProcessRoleLog)},
					},
					Locality: map[string]string{fdbv1beta2.FDBLocalityInstanceIDKey: "log-1"},
				},
			},
		},
	}

	storageAddresses := []fdbv1beta2.ProcessAddress{
		{IPAddress: nodeIP, Port: 4530},
		{IPAddress: nodeIP, Port: 4532},
	}
	logAddress := fdbv1beta2.ProcessAddress{IPAddress: nodeIP, Port: 4535}

	It("should classify each process by its IP:port, and a bare IP by every process on it", func() {
		missing := fdbv1beta2.ProcessAddress{IPAddress: nodeIP, Port: 4540}
		bareIP := fdbv1beta2.ProcessAddress{IPAddress: nodeIP}
		exclusions := getRemainingAndExcludedFromStatus(
			logr.Discard(),
			status,
			append(slices.Clone(storageAddresses), logAddress, missing, bareIP),
		)
		Expect(exclusions.fullyExcluded).To(ConsistOf(storageAddresses))
		Expect(exclusions.notExcluded).To(ConsistOf(logAddress, bareIP))
		Expect(exclusions.missingInStatus).To(ConsistOf(missing))
		Expect(exclusions.inProgress).To(BeEmpty())
	})
})
