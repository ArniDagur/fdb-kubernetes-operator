/*
 * coordinator_forwarding.go
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
	"time"

	fdbv1beta2 "github.com/FoundationDB/fdb-kubernetes-operator/v2/api/v1beta2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ProcessGroupAddressWillChange returns true if the address that the processes of the process group's current Pod
// listen on will not survive the pending changes: the process group will be removed, its Pod will gain or lose a
// port block (so its ports change), its public IP source changes, or its Pod will be recreated while the cluster file
// uses IP addresses (a recreated Pod gets a new IP).
func ProcessGroupAddressWillChange(
	cluster *fdbv1beta2.FoundationDBCluster,
	processGroup *fdbv1beta2.ProcessGroupStatus,
	pod *corev1.Pod,
) bool {
	if processGroup.IsMarkedForRemoval() {
		return true
	}

	if pod == nil {
		return false
	}

	// The port changes too, so this applies even with DNS names in the cluster file.
	if HasPortBlock(pod) != cluster.UsePortBlocks() {
		return true
	}

	publicIPSource, err := GetPublicIPSource(pod)
	if err == nil && publicIPSource != cluster.GetPublicIPSource() {
		return true
	}

	return !cluster.UseDNSInClusterFile() &&
		processGroup.GetConditionTime(fdbv1beta2.IncorrectPodSpec) != nil
}

// MarkForwardingCoordinators records that the previous coordinators, which are not part of the new coordinators,
// stopped being coordinators now. Only process groups whose processes were running as coordinators are marked, as only
// running processes can forward clients to the new coordinators.
func MarkForwardingCoordinators(
	cluster *fdbv1beta2.FoundationDBCluster,
	previousCoordinators map[string]fdbv1beta2.None,
	newCoordinators []fdbv1beta2.ProcessGroupID,
	now time.Time,
) {
	current := make(map[fdbv1beta2.ProcessGroupID]fdbv1beta2.None, len(newCoordinators))
	for _, processGroupID := range newCoordinators {
		current[processGroupID] = fdbv1beta2.None{}
	}

	for _, processGroup := range cluster.Status.ProcessGroups {
		if _, wasCoordinator := previousCoordinators[string(processGroup.ProcessGroupID)]; !wasCoordinator {
			continue
		}

		if _, isCoordinator := current[processGroup.ProcessGroupID]; isCoordinator {
			continue
		}

		processGroup.ForwardingCoordinatorSince = &metav1.Time{Time: now}
	}
}

// UpdateForwardingCoordinators clears the forwarding marker of process groups that are coordinators again, or whose
// grace period is over. currentCoordinators contains the process group IDs of the current coordinators.
func UpdateForwardingCoordinators(
	cluster *fdbv1beta2.FoundationDBCluster,
	processGroups []*fdbv1beta2.ProcessGroupStatus,
	currentCoordinators map[string]fdbv1beta2.None,
	now time.Time,
) {
	for _, processGroup := range processGroups {
		_, isCoordinator := currentCoordinators[string(processGroup.ProcessGroupID)]
		_, forwarding := cluster.GetForwardingCoordinatorUntil(processGroup, now)
		if isCoordinator || !forwarding {
			processGroup.ForwardingCoordinatorSince = nil
		}
	}
}
