/*
 * add_pods.go
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
	"fmt"

	fdbv1beta2 "github.com/FoundationDB/fdb-kubernetes-operator/v2/api/v1beta2"
	"github.com/FoundationDB/fdb-kubernetes-operator/v2/internal"
	"github.com/FoundationDB/fdb-kubernetes-operator/v2/internal/errors"
	"github.com/FoundationDB/fdb-kubernetes-operator/v2/pkg/fdbstatus"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
)

// addPods provides a reconciliation step for adding new pods to a cluster.
type addPods struct{}

// reconcile runs the reconciler's work.
func (a addPods) reconcile(
	ctx context.Context,
	r *FoundationDBClusterReconciler,
	cluster *fdbv1beta2.FoundationDBCluster,
	status *fdbv1beta2.FoundationDBStatus,
	logger logr.Logger,
) *requeue {
	// Pods that use the host network need a port block before they can be created. The blocks are assigned and
	// persisted in a first pass, so that a pod is never created from a spec without its ports.
	if cluster.UseHostNetwork() {
		err := assignPortBlocks(ctx, r, cluster, status, logger)
		if err != nil {
			return &requeue{curError: err}
		}
	}

	configMap, err := internal.GetConfigMap(cluster)
	if err != nil {
		return &requeue{curError: err}
	}
	existingConfigMap := &corev1.ConfigMap{}
	err = r.Get(
		ctx,
		types.NamespacedName{Namespace: configMap.Namespace, Name: configMap.Name},
		existingConfigMap,
	)
	if err != nil && k8serrors.IsNotFound(err) {
		logger.Info("Creating config map", "name", configMap.Name)
		err = r.Create(ctx, configMap)
		if err != nil {
			return &requeue{curError: err}
		}
	} else if err != nil {
		return &requeue{curError: err}
	}

	var missingPortBlocks []fdbv1beta2.ProcessGroupID
	for _, processGroup := range cluster.Status.ProcessGroups {
		_, err := r.PodLifecycleManager.GetPod(ctx, r, cluster, processGroup.GetPodName(cluster))
		// If no error is returned the Pod exists
		if err == nil {
			continue
		}

		// Ignore the is not found error, as we are checking here if we should create Pods.
		if !k8serrors.IsNotFound(err) {
			return &requeue{curError: err}
		}

		// If this process group is marked for removal, we normally don't want to spin it back up
		// again. However, in a downscaling scenario, it could be that this is a storage node that
		// is still draining its data onto another one. Therefore, we only want to leave it off
		// (by continuing) if the cluster says that this process group is fully drained and safe
		// to delete, which is the case if a previous run of the `removeProcessGroups` subreconciler
		// has marked it as excluded in the cluster status (it does so only after executing the
		// `exclude` FDB command and being told that the nodes in question are fully excluded).
		if processGroup.IsMarkedForRemoval() && processGroup.IsExcluded() {
			continue
		}

		if cluster.UseHostNetwork() && processGroup.PortBlock == nil {
			missingPortBlocks = append(missingPortBlocks, processGroup.ProcessGroupID)
			continue
		}

		pod, err := internal.GetPod(cluster, processGroup)
		if err != nil {
			r.Recorder.Event(
				cluster,
				corev1.EventTypeWarning,
				"GetPod",
				fmt.Sprintf(
					"failed to get the PodSpec for %s with error: %s",
					processGroup.ProcessGroupID,
					err,
				),
			)
			return &requeue{curError: err}
		}

		serverPerPod, err := internal.GetServersPerPodForPod(pod, processGroup.ProcessClass)
		if err != nil {
			return &requeue{curError: err}
		}

		configMapHash, err := internal.GetDynamicConfHash(
			configMap,
			processGroup.ProcessClass,
			internal.GetImageType(pod),
			serverPerPod,
			internal.HasPortBlock(pod),
		)
		if err != nil {
			return &requeue{curError: err}
		}

		pod.ObjectMeta.Annotations[fdbv1beta2.LastConfigMapKey] = configMapHash

		if cluster.GetPublicIPSource() == fdbv1beta2.PublicIPSourceService {
			service := &corev1.Service{}
			err = r.Get(
				ctx,
				types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name},
				service,
			)
			if err != nil {
				return &requeue{curError: err}
			}
			ip := service.Spec.ClusterIP
			if ip == "" {
				logger.Info(
					"Service does not have an IP address",
					"processGroupID",
					processGroup.ProcessGroupID,
				)
				return &requeue{
					message: fmt.Sprintf("Service %s does not have an IP address", service.Name),
				}
			}
			pod.Annotations[fdbv1beta2.PublicIPAnnotation] = ip
		}

		err = r.PodLifecycleManager.CreatePod(logr.NewContext(ctx, logger), r, pod)
		if err != nil {
			if errors.IsQuotaExceeded(err) {
				return &requeue{curError: err, delayedRequeue: true}
			}

			return &requeue{curError: err, delayedRequeue: true}
		}
	}

	if len(missingPortBlocks) > 0 {
		r.Recorder.Event(
			cluster,
			corev1.EventTypeWarning,
			"PortRangeExhausted",
			fmt.Sprintf(
				"no free port block in the host network port range for the process groups %v",
				missingPortBlocks,
			),
		)

		return &requeue{
			message: fmt.Sprintf(
				"waiting for free port blocks for the process groups %v",
				missingPortBlocks,
			),
			delayedRequeue: true,
		}
	}

	return nil
}

// assignPortBlocks gives every process group whose pod must be created a port block, if it doesn't have one yet, and
// persists the new blocks before any pod is created. A block is sized for the desired servers per pod of the process
// class and never changes afterwards.
func assignPortBlocks(
	ctx context.Context,
	r *FoundationDBClusterReconciler,
	cluster *fdbv1beta2.FoundationDBCluster,
	status *fdbv1beta2.FoundationDBStatus,
	logger logr.Logger,
) error {
	needsBlock := make([]*fdbv1beta2.ProcessGroupStatus, 0, len(cluster.Status.ProcessGroups))
	for _, processGroup := range cluster.Status.ProcessGroups {
		if processGroup.PortBlock != nil {
			continue
		}

		if processGroup.IsMarkedForRemoval() && processGroup.IsExcluded() {
			continue
		}

		_, err := r.PodLifecycleManager.GetPod(ctx, r, cluster, processGroup.GetPodName(cluster))
		if err == nil {
			continue
		}

		if !k8serrors.IsNotFound(err) {
			return err
		}

		needsBlock = append(needsBlock, processGroup)
	}

	if len(needsBlock) == 0 {
		return nil
	}

	// Blocks of existing pods are taken as well, in case the status is behind the pods.
	pods, err := r.PodLifecycleManager.GetPods(
		ctx,
		r,
		cluster,
		internal.GetPodListOptions(cluster, "", "")...)
	if err != nil {
		return err
	}

	podBlocks := make([]*fdbv1beta2.PortBlock, 0, len(pods))
	for _, pod := range pods {
		processClass := fdbv1beta2.ProcessClass(pod.Labels[cluster.GetProcessClassLabel()])
		block, err := internal.GetPortBlock(pod, processClass)
		if err != nil {
			return err
		}

		podBlocks = append(podBlocks, block)
	}

	var exclusions []fdbv1beta2.ProcessAddress
	if status != nil {
		exclusions, err = fdbstatus.GetExclusions(status)
		if err != nil {
			return err
		}
	}

	taken := internal.GetTakenPortIntervals(cluster.Status.ProcessGroups, podBlocks, exclusions)
	rangeStart, rangeEnd := cluster.GetHostNetworkPortRange()
	assigned := false
	for _, processGroup := range needsBlock {
		serversPerPod := cluster.GetDesiredServersPerPod(processGroup.ProcessClass)
		size := fdbv1beta2.PortBlockSize(serversPerPod)
		start, ok := internal.AllocatePortBlock(rangeStart, rangeEnd, size, taken)
		if !ok {
			logger.Info(
				"no free port block in the host network port range",
				"processGroupID", processGroup.ProcessGroupID,
				"blockSize", size,
			)
			continue
		}

		processGroup.PortBlock = &fdbv1beta2.PortBlock{Start: start, ServersPerPod: serversPerPod}
		taken = append(taken, internal.PortInterval{
			Start: start,
			End:   processGroup.PortBlock.End(),
		})
		assigned = true
		logger.Info(
			"assigned port block",
			"processGroupID", processGroup.ProcessGroupID,
			"start", start,
			"serversPerPod", serversPerPod,
		)
	}

	if !assigned {
		return nil
	}

	// Updating the status replaces the cluster with the stored object, whose spec isn't normalized. The pods are
	// created from the normalized spec, so it's restored after the update.
	normalizedSpec := cluster.Spec.DeepCopy()
	err = r.updateOrApply(ctx, cluster)
	cluster.Spec = *normalizedSpec

	return err
}
