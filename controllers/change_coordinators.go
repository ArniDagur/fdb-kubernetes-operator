/*
 * change_coordinators.go
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

package controllers

import (
	"context"
	"errors"
	"fmt"

	"github.com/FoundationDB/fdb-kubernetes-operator/v2/internal/coordinator"
	"github.com/FoundationDB/fdb-kubernetes-operator/v2/internal/locality"
	"github.com/FoundationDB/fdb-kubernetes-operator/v2/pkg/fdbstatus"
	"github.com/go-logr/logr"

	corev1 "k8s.io/api/core/v1"

	fdbv1beta2 "github.com/FoundationDB/fdb-kubernetes-operator/v2/api/v1beta2"
)

// changeCoordinators provides a reconciliation step for choosing new
// coordinators.
type changeCoordinators struct{}

// reconcile runs the reconciler's work.
func (c changeCoordinators) reconcile(
	ctx context.Context,
	r *FoundationDBClusterReconciler,
	cluster *fdbv1beta2.FoundationDBCluster,
	status *fdbv1beta2.FoundationDBStatus,
	logger logr.Logger,
) *requeue {
	if !cluster.Status.Configured {
		return nil
	}

	adminClient, err := r.getAdminClient(logger, cluster)
	if err != nil {
		return &requeue{curError: err, delayedRequeue: true}
	}
	defer func() {
		_ = adminClient.Close()
	}()

	// If the status is not cached, we have to fetch it.
	if status == nil {
		status, err = adminClient.GetStatus()
		if err != nil {
			return &requeue{curError: err}
		}
	}

	coordinatorStatus := make(map[string]bool, len(status.Client.Coordinators.Coordinators))
	for _, coord := range status.Client.Coordinators.Coordinators {
		coordinatorStatus[coord.Address.String()] = false
	}

	hasValidCoordinators, allAddressesValid, err := locality.CheckCoordinatorValidity(
		logger,
		cluster,
		status,
		coordinatorStatus,
	)
	if err != nil {
		return &requeue{curError: err, delayedRequeue: true}
	}

	currentCoordinators := fdbstatus.GetCoordinatorsFromStatus(status)
	var pendingAddressChange map[fdbv1beta2.ProcessGroupID]fdbv1beta2.None
	if cluster.GetCoordinatorForwardingGracePeriod() > 0 {
		pendingAddressChange, err = getProcessGroupsWithPendingAddressChange(ctx, r, cluster)
		if err != nil {
			return &requeue{curError: err, delayedRequeue: true}
		}
	}

	// Valid coordinators are moved before their address changes, so that their processes can forward clients with an
	// outdated cluster file to the new coordinators.
	moving := coordinatorsWithPendingAddressChange(currentCoordinators, pendingAddressChange)
	if hasValidCoordinators && len(moving) == 0 {
		return nil
	}

	if !allAddressesValid {
		logger.Info("Deferring coordinator change")
		r.Recorder.Event(
			cluster,
			corev1.EventTypeNormal,
			"DeferringCoordinatorChange",
			"Deferring coordinator change until all processes have consistent address TLS settings",
		)
		return nil
	}

	// Perform safety checks before changing coordinators. The minimum uptime should reduce the coordinator changes
	// if a process is down for a short amount of time, e.g. after a cluster wide bounce.
	err = fdbstatus.CanSafelyChangeCoordinators(
		logger,
		cluster,
		status,
		r.MinimumUptimeForCoordinatorChangeWithMissingProcess,
		r.MinimumUptimeForCoordinatorChangeWithUndesiredProcess,
	)
	if err != nil {
		logger.Info("Deferring coordinator change due to safety check", "error", err.Error())
		return &requeue{curError: err, delayedRequeue: true}
	}

	err = r.takeLock(logger, cluster, "changing coordinators")
	if err != nil {
		return &requeue{curError: err, delayedRequeue: true}
	}

	if hasValidCoordinators {
		logger.Info("Moving coordinators before their address changes", "processGroups", moving)
		r.Recorder.Event(
			cluster,
			corev1.EventTypeNormal,
			"MovingCoordinators",
			fmt.Sprintf("Moving coordinators before the address of %v changes", moving),
		)
	} else {
		logger.Info("Changing coordinators")
		r.Recorder.Event(
			cluster,
			corev1.EventTypeNormal,
			"ChangingCoordinators",
			"Choosing new coordinators",
		)
	}

	// Coordinators that are still valid are only moved to process groups that already have their final address.
	// Invalid coordinators are replaced even if only process groups with a pending address change can take over.
	err = changeCoordinatorsAwayFrom(
		logger,
		adminClient,
		cluster,
		status,
		currentCoordinators,
		pendingAddressChange,
		!hasValidCoordinators,
	)
	if err != nil {
		if hasValidCoordinators && errors.Is(err, coordinator.ErrCoordinatorSelection) {
			logger.Info(
				"Waiting for more process groups at their final address to move the coordinators",
				"error",
				err.Error(),
			)
			return &requeue{
				message:        "waiting for more process groups at their final address to move the coordinators",
				delayedRequeue: true,
			}
		}

		return &requeue{curError: err, delayedRequeue: true}
	}

	// Reset the SecondsSinceLastRecovered sine the operator just changed the coordinators, which will cause a recovery.
	status.Cluster.RecoveryState.SecondsSinceLastRecovered = 0.0

	err = r.updateOrApply(ctx, cluster)
	if err != nil {
		return &requeue{curError: err, delayedRequeue: true}
	}

	return nil
}
