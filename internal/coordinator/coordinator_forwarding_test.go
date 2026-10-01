/*
 * coordinator_forwarding_test.go
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

package coordinator

import (
	"context"

	fdbv1beta2 "github.com/FoundationDB/fdb-kubernetes-operator/v2/api/v1beta2"
	"github.com/FoundationDB/fdb-kubernetes-operator/v2/internal"
	"github.com/FoundationDB/fdb-kubernetes-operator/v2/pkg/fdbadminclient/mock"
	"github.com/go-logr/logr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Change coordinators while excluding process groups", func() {
	var cluster *fdbv1beta2.FoundationDBCluster
	var adminClient *mock.AdminClient
	var status *fdbv1beta2.FoundationDBStatus

	BeforeEach(func() {
		cluster = internal.CreateDefaultCluster()
		// A configured cluster already has coordinators; the mock derives the new connection string from it.
		cluster.Status.ConnectionString = "operator_test:asdfasf@127.0.0.1:4501"
		Expect(internal.SetupClusterForTest(cluster, k8sClient)).To(Succeed())
		Expect(k8sClient.Create(context.Background(), cluster)).To(Succeed())

		var err error
		adminClient, err = mock.NewMockAdminClientUncast(cluster, k8sClient)
		Expect(err).NotTo(HaveOccurred())
		status, err = adminClient.GetStatus()
		Expect(err).NotTo(HaveOccurred())
	})

	It("should never select an excluded process group and return the new coordinators", func() {
		excluded := map[fdbv1beta2.ProcessGroupID]fdbv1beta2.None{}
		for _, processGroup := range internal.PickProcessGroups(cluster, fdbv1beta2.ProcessClassStorage, 2) {
			excluded[processGroup.ProcessGroupID] = fdbv1beta2.None{}
		}

		newCoordinators, err := ChangeCoordinators(
			logr.Discard(),
			adminClient,
			cluster,
			status,
			excluded,
		)
		Expect(err).NotTo(HaveOccurred())
		Expect(newCoordinators).To(HaveLen(cluster.DesiredCoordinatorCount()))
		for _, processGroupID := range newCoordinators {
			Expect(excluded).NotTo(HaveKey(processGroupID))
		}

		// The new connection string names exactly the returned coordinators.
		processGroupByAddress := map[string]string{}
		for _, process := range status.Cluster.Processes {
			processGroupByAddress[process.Address.StringWithoutFlags()] = process.Locality[fdbv1beta2.FDBLocalityInstanceIDKey]
		}
		connectionString, err := fdbv1beta2.ParseConnectionString(cluster.Status.ConnectionString)
		Expect(err).NotTo(HaveOccurred())
		fromConnectionString := make(
			[]fdbv1beta2.ProcessGroupID,
			0,
			len(connectionString.Coordinators),
		)
		for _, coordinatorAddress := range connectionString.Coordinators {
			address, err := fdbv1beta2.ParseProcessAddress(coordinatorAddress)
			Expect(err).NotTo(HaveOccurred())
			Expect(processGroupByAddress).To(HaveKey(address.StringWithoutFlags()))
			fromConnectionString = append(
				fromConnectionString,
				fdbv1beta2.ProcessGroupID(processGroupByAddress[address.StringWithoutFlags()]),
			)
		}
		Expect(fromConnectionString).To(ConsistOf(newCoordinators))
	})

	It(
		"should fail with ErrCoordinatorSelection if the excluded process groups leave no valid set",
		func() {
			excluded := map[fdbv1beta2.ProcessGroupID]fdbv1beta2.None{}
			for _, processGroup := range cluster.Status.ProcessGroups {
				excluded[processGroup.ProcessGroupID] = fdbv1beta2.None{}
			}
			previousConnectionString := cluster.Status.ConnectionString

			newCoordinators, err := ChangeCoordinators(
				logr.Discard(),
				adminClient,
				cluster,
				status,
				excluded,
			)
			Expect(err).To(MatchError(ErrCoordinatorSelection))
			Expect(newCoordinators).To(BeEmpty())
			Expect(cluster.Status.ConnectionString).To(Equal(previousConnectionString))
		},
	)
})
