/*
 * coordinator_forwarding.go
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
	"errors"
	"time"

	fdbv1beta2 "github.com/FoundationDB/fdb-kubernetes-operator/v2/api/v1beta2"
	"github.com/FoundationDB/fdb-kubernetes-operator/v2/internal"
	"github.com/FoundationDB/fdb-kubernetes-operator/v2/internal/coordinator"
	"github.com/FoundationDB/fdb-kubernetes-operator/v2/pkg/fdbadminclient"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
)

// getProcessGroupsWithPendingAddressChange returns the process groups whose current address will not survive the
// pending changes, see internal.ProcessGroupAddressWillChange.
func getProcessGroupsWithPendingAddressChange(
	ctx context.Context,
	r *FoundationDBClusterReconciler,
	cluster *fdbv1beta2.FoundationDBCluster,
) (map[fdbv1beta2.ProcessGroupID]fdbv1beta2.None, error) {
	pending := make(map[fdbv1beta2.ProcessGroupID]fdbv1beta2.None)
	for _, processGroup := range cluster.Status.ProcessGroups {
		var pod *corev1.Pod
		currentPod, err := r.PodLifecycleManager.GetPod(
			ctx,
			r,
			cluster,
			processGroup.GetPodName(cluster),
		)
		if err == nil {
			pod = currentPod
		} else if !k8serrors.IsNotFound(err) {
			return nil, err
		}

		if internal.ProcessGroupAddressWillChange(cluster, processGroup, pod) {
			pending[processGroup.ProcessGroupID] = fdbv1beta2.None{}
		}
	}

	return pending, nil
}

// changeCoordinatorsAwayFrom changes the coordinators without selecting any of the process groups with a pending
// address change. If that's not possible and fallback is true, any process group may be selected, because the
// current coordinators must be replaced. The previous coordinators are marked as forwarding coordinators.
func changeCoordinatorsAwayFrom(
	logger logr.Logger,
	adminClient fdbadminclient.AdminClient,
	cluster *fdbv1beta2.FoundationDBCluster,
	status *fdbv1beta2.FoundationDBStatus,
	previousCoordinators map[string]fdbv1beta2.None,
	pendingAddressChange map[fdbv1beta2.ProcessGroupID]fdbv1beta2.None,
	fallback bool,
) error {
	newCoordinators, err := coordinator.ChangeCoordinatorsExcluding(
		logger,
		adminClient,
		cluster,
		status,
		pendingAddressChange,
	)
	if err != nil && fallback && len(pendingAddressChange) > 0 &&
		errors.Is(err, coordinator.ErrCoordinatorSelection) {
		logger.Info(
			"Not enough process groups at their final address for new coordinators, selecting from all process groups",
			"error",
			err.Error(),
		)
		newCoordinators, err = coordinator.ChangeCoordinatorsExcluding(
			logger,
			adminClient,
			cluster,
			status,
			nil,
		)
	}
	if err != nil {
		return err
	}

	internal.MarkForwardingCoordinators(cluster, previousCoordinators, newCoordinators, time.Now())

	return nil
}

// coordinatorsWithPendingAddressChange returns the current coordinators whose address will change.
func coordinatorsWithPendingAddressChange(
	currentCoordinators map[string]fdbv1beta2.None,
	pendingAddressChange map[fdbv1beta2.ProcessGroupID]fdbv1beta2.None,
) []fdbv1beta2.ProcessGroupID {
	var result []fdbv1beta2.ProcessGroupID
	for processGroupID := range currentCoordinators {
		if _, ok := pendingAddressChange[fdbv1beta2.ProcessGroupID(processGroupID)]; ok {
			result = append(result, fdbv1beta2.ProcessGroupID(processGroupID))
		}
	}

	return result
}

// forwardingCoordinatorRequeue returns the requeue for process groups that keep their address until the forwarding
// grace period is over. until is the earliest time any of them may change its address.
func forwardingCoordinatorRequeue(message string, until time.Time) *requeue {
	delay := time.Until(until)
	if delay < time.Second {
		delay = time.Second
	}

	return &requeue{message: message, delay: delay, delayedRequeue: true}
}
