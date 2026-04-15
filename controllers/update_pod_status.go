/*
 * update_pod_status.go
 *
 * This source file is part of the FoundationDB open source project
 *
 * Copyright 2019-2021 Apple Inc. and the FoundationDB project authors
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
	"time"

	fdbv1beta2 "github.com/FoundationDB/fdb-kubernetes-operator/v2/api/v1beta2"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
)

// updatePodStatus provides a reconciliation step for setting process group
// conditions based on Kubernetes pod state. Unlike updateStatus, this reconciler
// does not depend on FDB availability, so it runs even when the database is
// unreachable. This is critical for breaking deadlocks where the database is
// down because too many pods are in a terminal failed state (e.g. evicted).
type updatePodStatus struct{}

// reconcile runs the reconciler's work.
func (u updatePodStatus) reconcile(
	ctx context.Context,
	r *FoundationDBClusterReconciler,
	cluster *fdbv1beta2.FoundationDBCluster,
	_ *fdbv1beta2.FoundationDBStatus,
	logger logr.Logger,
) *requeue {
	for _, processGroup := range cluster.Status.ProcessGroups {
		pod, err := r.PodLifecycleManager.GetPod(
			ctx,
			r,
			cluster,
			processGroup.GetPodName(cluster),
		)
		if err != nil {
			if k8serrors.IsNotFound(err) {
				if processGroup.IsMarkedForRemoval() && processGroup.IsExcluded() {
					processGroup.UpdateCondition(fdbv1beta2.ResourcesTerminating, true)
				} else {
					processGroup.UpdateCondition(fdbv1beta2.MissingPod, true)
				}

				continue
			}

			logger.Info("Could not fetch Pod information",
				"processGroupID", processGroup.ProcessGroupID)
			continue
		}

		processGroup.UpdateCondition(fdbv1beta2.MissingPod, false)

		// Handle pods that are stuck in terminating.
		if !pod.ObjectMeta.DeletionTimestamp.IsZero() {
			if processGroup.IsMarkedForRemoval() && processGroup.IsExcluded() {
				processGroup.UpdateCondition(fdbv1beta2.ResourcesTerminating, true)
				continue
			}

			if pod.ObjectMeta.DeletionTimestamp.Add(cluster.GetFailedPodDuration()).
				Before(time.Now()) {
				processGroup.UpdateCondition(fdbv1beta2.PodFailing, true)
				continue
			}

			continue
		}

		if pod.Status.Phase == corev1.PodPending {
			processGroup.UpdateCondition(fdbv1beta2.PodPending, true)
			continue
		}

		failing := false
		for _, container := range pod.Status.ContainerStatuses {
			if !container.Ready {
				failing = true
				break
			}
		}

		// Delete pods that are in a terminal failed state (e.g. Evicted or
		// NodeAffinity) so they can be recreated by the addPods reconciler.
		if pod.Status.Phase == corev1.PodFailed {
			failing = true

			if (pod.Status.Reason == "NodeAffinity" || pod.Status.Reason == "Evicted") &&
				pod.CreationTimestamp.Add(5*time.Minute).Before(time.Now()) {
				logger.Info("Delete Pod that is in a terminal failed state",
					"processGroupID", processGroup.ProcessGroupID,
					"reason", pod.Status.Reason)

				err = r.PodLifecycleManager.DeletePod(logr.NewContext(ctx, logger), r, pod)
				if err != nil {
					return &requeue{curError: err}
				}
			}
		}

		processGroup.UpdateCondition(fdbv1beta2.PodFailing, failing)
		processGroup.UpdateCondition(fdbv1beta2.PodPending, false)
	}

	return nil
}
