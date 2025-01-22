/*
Copyright 2023. projectsveltos.io. All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package evaluation

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	libsveltosv1beta1 "github.com/projectsveltos/libsveltos/api/v1beta1"
	logs "github.com/projectsveltos/libsveltos/lib/logsettings"
	libsveltosset "github.com/projectsveltos/libsveltos/lib/set"
	"github.com/projectsveltos/sveltos-agent/pkg/utils"
)

// When ClusterProfile reloader knob is set to true, corresponding Reloader
// instance is deployed in the managed cluster after add-ons are deployed.
// If a ClusterProfile with reloader knob set to true is matching a managed cluster
// after deploying Helm charts, Reloader is created with all Deployments/StatefulSet/DaemonSet
// deployed because of such helm charts.
// If same ClusterProfile with reloader knob set to true is also deploying resources contained
// in ConfigMap/Secrets, another Reloader is created with all Deployments/StatefulSet/DaemonSet
// deployed because of referenced ConfigMaps/Secrets.

var (
	// key: Reloader: value: set of all Deployment/StatefulSet/DaemonSet referenced by reloader
	reloaderMap map[corev1.ObjectReference]*libsveltosset.Set

	// key: Deployment/StatefulSet/DaemonSet: value: set of all Reloaders referencing it
	resourceMap map[corev1.ObjectReference]*libsveltosset.Set

	// key: ConfigMap/Secret: value: set of all Deployment/StatefulSet/DaemonSet mounting it
	volumeMap map[corev1.ObjectReference]*libsveltosset.Set
)

const (
	partNumber = 2
)

func initializeReloaderMaps() {
	reloaderMap = make(map[corev1.ObjectReference]*libsveltosset.Set)
	resourceMap = make(map[corev1.ObjectReference]*libsveltosset.Set)
	volumeMap = make(map[corev1.ObjectReference]*libsveltosset.Set)
}

// evaluateReloaders evaluates all reloaders awaiting evaluation
func (m *manager) evaluateReloaders(ctx context.Context, wg *sync.WaitGroup) {
	var once sync.Once

	for {
		select {
		case <-ctx.Done():
			m.log.V(logs.LogInfo).Info("Context canceled. Exiting evaluation.")
			return // Exit the goroutine
		default:
			// Sleep before next evaluation
			time.Sleep(m.interval)

			m.log.V(logs.LogDebug).Info("Evaluating Reloaders")
			m.mu.Lock()
			// Copy queue content. That is only operation that
			// needs to be done in a mutex protect section
			jobQueueCopy := make([]string, len(m.reloaderJobQueue))
			i := 0
			for k := range m.reloaderJobQueue {
				jobQueueCopy[i] = k
				i++
			}
			// Reset current queue
			m.reloaderJobQueue = make(map[string]bool)
			m.mu.Unlock()

			failedEvaluations := make([]string, 0)

			// format is
			// - kind:name for Reloader
			// - kind:namespace/name for ConfigMap/Secret

			for i := range jobQueueCopy {
				var err error
				parts := strings.Split(jobQueueCopy[i], ":")
				if len(parts) != partNumber {
					m.log.V(logs.LogInfo).Info(fmt.Sprintf("incorrect format %s", jobQueueCopy[i]))
					continue
				}

				if strings.Contains(parts[1], "/") {
					m.log.V(logs.LogDebug).Info(fmt.Sprintf("Evaluating %s %s", parts[0], parts[1]))
					err = m.evaluateMountedResource(ctx, parts[0], parts[1])
				} else {
					m.log.V(logs.LogDebug).Info(fmt.Sprintf("Evaluating Reloader %s", parts[1]))
					err = m.evaluateReloaderInstance(ctx, parts[1])
				}

				if err != nil {
					m.log.V(logs.LogInfo).Error(err,
						fmt.Sprintf("failed to evaluate %s", jobQueueCopy[i]))
					failedEvaluations = append(failedEvaluations, jobQueueCopy[i])
				}
			}

			// Re-queue all Reloaders whose evaluation failed
			for i := range failedEvaluations {
				m.log.V(logs.LogDebug).Info(fmt.Sprintf("requeuing Reloader %s for evaluation", failedEvaluations[i]))
				m.EvaluateReloader(failedEvaluations[i])
			}

			once.Do(func() {
				wg.Done()
			})
		}
	}
}

// evaluateMountedResource finds all Deployments/StatefulSets/DaemonSets currently mounting
// resource and updates corresponding ReloaderReport.
// Kind is either ConfigMap/Secret. resourceInfo is ConfigMap/Secret namespace/name
func (m *manager) evaluateMountedResource(ctx context.Context, kind, resourceInfo string) error {
	// resourceInfo is in the form namespace/name
	parts := strings.Split(resourceInfo, "/")
	if len(parts) != partNumber {
		errMsg := fmt.Sprintf("incorrect name format %s", resourceInfo)
		m.log.V(logs.LogInfo).Info(errMsg)
		return fmt.Errorf("%s", errMsg)
	}

	mountedResource := &corev1.ObjectReference{
		Kind:       kind,
		APIVersion: corev1.SchemeGroupVersion.String(), // both Secret and ConfigMap are corev1
		Namespace:  parts[0],
		Name:       parts[1],
	}

	// Finds all Deployments/StatefulSets/DaemonSets currently mounting this resource.
	// Updates ReloaderReport Spec and Status accordingly.
	err := m.updateReloaderReport(ctx, mountedResource)
	if err != nil {
		return err
	}

	if m.sendReport {
		err = m.sendReloaderReportToMgtmCluster(ctx, mountedResource)
		if err != nil {
			m.log.Error(err, "failed to send EventReport")
			return err
		}
	}

	return nil
}

// updateReloaderReport updates ReloaderReport Spec and Status.
// Spec will contain list of Deployments/StatefulSets/DaemonSets currently mounting resource
// represented by ref.
func (m *manager) updateReloaderReport(ctx context.Context, mountedResource *corev1.ObjectReference) error {
	// Get list of all Deployments/StatefulSet/DaemonSet currently mounting the ConfigMap/Secret
	// represented by ref
	v, ok := volumeMap[*mountedResource]
	if !ok {
		return nil
	}

	reloaderReport, err := m.getReloaderReport(ctx, mountedResource)
	if err != nil {
		m.log.V(logs.LogInfo).Info(fmt.Sprintf("failed to get reloaderReport %v", err))
		return err
	}

	if reloaderReport.Spec.ResourcesToReload == nil {
		reloaderReport.Spec.ResourcesToReload = make([]libsveltosv1beta1.ReloaderInfo, 0)
	}

	resources := v.Items()
	for i := range resources {
		reloaderReport.Spec.ResourcesToReload = append(reloaderReport.Spec.ResourcesToReload,
			libsveltosv1beta1.ReloaderInfo{
				Kind:      resources[i].Kind,
				Namespace: resources[i].Namespace,
				Name:      resources[i].Name,
			})
	}

	err = m.Client.Update(ctx, reloaderReport)
	if err != nil {
		m.log.V(logs.LogInfo).Info(fmt.Sprintf("failed to update reloaderReport %v", err))
		return err
	}

	return m.updateReloaderReportStatus(ctx, reloaderReport)
}

// updateHealthCheckReportStatus updates HealthCheckReport Status by marking Phase as ReportWaitingForDelivery
func (m *manager) updateReloaderReportStatus(ctx context.Context,
	reloaderReport *libsveltosv1beta1.ReloaderReport) error {

	logger := m.log.WithValues("reloaderReport", reloaderReport.Name)
	logger.V(logs.LogDebug).Info("updating reloaderReport status")

	phase := libsveltosv1beta1.ReportWaitingForDelivery
	reloaderReport.Status.Phase = &phase

	return m.Status().Update(ctx, reloaderReport)
}

// getReloaderReport gets ReloaderReport. It creates one if does not exist already
func (m *manager) getReloaderReport(ctx context.Context, mountedResource *corev1.ObjectReference,
) (*libsveltosv1beta1.ReloaderReport, error) {

	reloaderReport := &libsveltosv1beta1.ReloaderReport{}

	// There is one ReloaderReport per mounted resource
	name := libsveltosv1beta1.GetReloaderReportName(mountedResource.Kind, mountedResource.Namespace,
		mountedResource.Name, m.clusterName, &m.clusterType)

	err := m.Client.Get(ctx,
		types.NamespacedName{Namespace: utils.ReportNamespace, Name: name}, reloaderReport)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return m.createReloaderReport(ctx, mountedResource)
		}
		return nil, err
	}

	return reloaderReport, nil
}

// createReloaderReport creates ReloaderReport
func (m *manager) createReloaderReport(ctx context.Context, mountedResource *corev1.ObjectReference,
) (*libsveltosv1beta1.ReloaderReport, error) {

	name := libsveltosv1beta1.GetReloaderReportName(mountedResource.Kind, mountedResource.Namespace,
		mountedResource.Name, m.clusterName, &m.clusterType)
	reloaderReport := &libsveltosv1beta1.ReloaderReport{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: utils.ReportNamespace,
			Labels: libsveltosv1beta1.GetReloaderReportLabels(
				m.clusterName, &m.clusterType),
			Annotations: libsveltosv1beta1.GetReloaderReportAnnotations(
				mountedResource.Kind, mountedResource.Namespace, mountedResource.Name,
			),
		},
		Spec: libsveltosv1beta1.ReloaderReportSpec{
			ResourcesToReload: make([]libsveltosv1beta1.ReloaderInfo, 0),
		},
	}

	err := m.Client.Create(ctx, reloaderReport)
	return reloaderReport, err
}

// sendReloaderReportToMgtmCluster sends default ReloaderReport to management cluster. It also deletes ReloaderReport
// in the managed cluster after updates is sent.
func (m *manager) sendReloaderReportToMgtmCluster(ctx context.Context, mountedResource *corev1.ObjectReference) error {
	logger := m.log.WithValues("reloaderReport", mountedResource.Name)

	name := libsveltosv1beta1.GetReloaderReportName(mountedResource.Kind, mountedResource.Namespace,
		mountedResource.Name, m.clusterName, &m.clusterType)

	// Get reloaderReport in the managed cluster
	reloaderReport := &libsveltosv1beta1.ReloaderReport{}
	err := m.Get(ctx,
		types.NamespacedName{Namespace: utils.ReportNamespace, Name: name}, reloaderReport)
	if err != nil {
		logger.V(logs.LogInfo).Info(fmt.Sprintf("failed to get reloaderReport: %v", err))
		return err
	}

	logger.V(logs.LogDebug).Info("send reloaderReport to management cluster")
	err = m.createOrUpdateReloaderReportInMgtmtCluster(ctx, reloaderReport)
	if err != nil {
		logger.V(logs.LogInfo).Info(
			fmt.Sprintf("failed to create/update reloaderReport in the mgtm cluster: %v", err))
		return err
	}

	logger.V(logs.LogDebug).Info("delete reloaderReport in managed cluster")
	return m.deleteReloaderReport(ctx, reloaderReport)
}

func (m *manager) deleteReloaderReport(ctx context.Context, reloaderReport *libsveltosv1beta1.ReloaderReport) error {
	return m.Client.Delete(ctx, reloaderReport)
}

// createOrUpdateReloaderReport creates or updates ReloaderReport in the management cluster
func (m *manager) createOrUpdateReloaderReportInMgtmtCluster(ctx context.Context,
	reloaderReport *libsveltosv1beta1.ReloaderReport) error {

	agentClient, err := m.getManamegentClusterClient(ctx, m.log)
	if err != nil {
		return err
	}
	currentReloaderReport := &libsveltosv1beta1.ReloaderReport{}

	namespace := m.clusterNamespace

	err = agentClient.Get(ctx,
		types.NamespacedName{Namespace: namespace, Name: reloaderReport.Name}, currentReloaderReport)
	if err != nil {
		if apierrors.IsNotFound(err) {
			currentReloaderReport.Namespace = namespace
			currentReloaderReport.Name = reloaderReport.Name
			currentReloaderReport.Spec = reloaderReport.Spec
			currentReloaderReport.Spec.ClusterNamespace = m.clusterNamespace
			currentReloaderReport.Spec.ClusterName = m.clusterName
			currentReloaderReport.Spec.ClusterType = m.clusterType
			currentReloaderReport.Labels = reloaderReport.Labels
			currentReloaderReport.Annotations = reloaderReport.Annotations
			return agentClient.Create(ctx, currentReloaderReport)
		}
		return err
	}

	currentReloaderReport.Namespace = namespace
	currentReloaderReport.Name = reloaderReport.Name
	currentReloaderReport.Spec.ClusterType = m.clusterType
	currentReloaderReport.Spec.ResourcesToReload = reloaderReport.Spec.ResourcesToReload
	currentReloaderReport.Labels = reloaderReport.Labels
	currentReloaderReport.Annotations = reloaderReport.Annotations

	return agentClient.Update(ctx, currentReloaderReport)
}

// evaluateReloaderInstance evaluates reloader instance.
// - Updates reloaderMap => Deployment/StatefulSet/DaemonSet each reloader is currently referencing
// - Updates resourceMap => for each Deployment/StatefulSet/DaemonSet list of mounted ConfigMaps/Secrets
// - Updates volumeMap => for each mounted ConfigMap/Secret list of Deployments/StatefulSets/DaemonSets
// currently mounting it
func (m *manager) evaluateReloaderInstance(ctx context.Context, reloaderName string) error {
	reloader := &libsveltosv1beta1.Reloader{}

	logger := m.log.WithValues("reloader", reloaderName)
	logger.V(logs.LogDebug).Info("evaluating")

	err := m.Client.Get(ctx, types.NamespacedName{Name: reloaderName}, reloader)
	if err != nil {
		if apierrors.IsNotFound(err) {
			m.cleanReloaderMap(reloaderName)
			m.cleanResourceMap(reloaderName)
			return nil
		}
		return err
	}

	if !reloader.DeletionTimestamp.IsZero() {
		m.cleanReloaderMap(reloaderName)
		m.cleanResourceMap(reloaderName)
		return nil
	}

	reloaderRef := getReloaderObjectReference(reloaderName)

	currentResources := &libsveltosset.Set{}
	// Walk over all Deployment/StatefulSet/DaemonSet referenced by
	// this Reloader instance
	for i := range reloader.Spec.ReloaderInfo {
		resource := getObjectRef(&reloader.Spec.ReloaderInfo[i])
		currentResources.Insert(&resource)

		// Add Reloader as a consumer for Deployment/StatefulSet/DaemonSet
		// More than one ClusterProfile migth be deploying such Deployment/
		// StatefulSet/DaemonSet in this managed cluster. And more than one
		// ClusterProfile might have reloader knob set.
		s := resourceMap[resource]
		if s == nil {
			s = &libsveltosset.Set{}
			resourceMap[resource] = s
		}
		resourceMap[resource].Insert(reloaderRef)

		err := m.updateVolumeMap(ctx, &reloader.Spec.ReloaderInfo[i])
		if err != nil {
			return err
		}

		// Get list of Deployment/StatefulSet/DaemonSet previously
		// referenced by this Reloader instance.
		var toBeRemoved []corev1.ObjectReference
		if v, ok := reloaderMap[*reloaderRef]; ok {
			// Compare list of Deployment/StatefulSet/DaemonSet currently referenced
			// and previously referenced.
			toBeRemoved = v.Difference(currentResources)
		}

		for i := range toBeRemoved {
			reloaderSet := resourceMap[toBeRemoved[i]]
			reloaderSet.Erase(reloaderRef)
			if reloaderSet.Len() == 0 {
				// Since no Reloader is referencing Deployment/StatefulSet/DaemonSet
				// anymore, clean volumeMap as well.
				// volumeMap contains all ConfigMap/Secret mounted by watched
				// Deployment/StatefulSet/DaemonSet
				delete(resourceMap, toBeRemoved[i])
				m.cleanVolumeMap(&toBeRemoved[i])
			}
		}
	}
	reloaderMap[*reloaderRef] = currentResources

	return nil
}

func getReloaderObjectReference(reloaderName string) *corev1.ObjectReference {
	return &corev1.ObjectReference{
		Kind:       libsveltosv1beta1.ReloaderKind,
		APIVersion: libsveltosv1beta1.GroupVersion.String(),
		Name:       reloaderName,
	}
}

func getObjectRef(reloaderInfo *libsveltosv1beta1.ReloaderInfo) corev1.ObjectReference {
	return corev1.ObjectReference{
		Kind:       reloaderInfo.Kind,
		Namespace:  reloaderInfo.Namespace,
		Name:       reloaderInfo.Name,
		APIVersion: appsv1.SchemeGroupVersion.String(), // Deployments/StatefulSet/DaemonSet are all apps/v1
	}
}

func (m *manager) updateVolumeMap(ctx context.Context,
	reloaderInfo *libsveltosv1beta1.ReloaderInfo) error {

	volumes, err := m.getResourceVolumeMounts(ctx, reloaderInfo)
	if err != nil {
		return err
	}

	ref := getObjectRef(reloaderInfo)

	for i := range volumes {
		v := &volumes[i]
		s := volumeMap[*v]
		if s == nil {
			s = &libsveltosset.Set{}
			volumeMap[*v] = s
		}
		volumeMap[*v].Insert(&ref)
	}

	return nil
}

func (m *manager) getResourceVolumeMounts(ctx context.Context,
	reloaderInfo *libsveltosv1beta1.ReloaderInfo,
) ([]corev1.ObjectReference, error) {

	switch reloaderInfo.Kind {
	case "Deployment":
		return m.getDeploymentVolumeMounts(ctx, reloaderInfo)
	case "StatefulSet":
		return m.getStatufulSetVolumeMounts(ctx, reloaderInfo)
	case "DaemonSet":
		return m.getDaemonSetVolumeMounts(ctx, reloaderInfo)
	}

	return nil, fmt.Errorf("unknown reloaderInfo")
}

func (m *manager) getVolumes(resource client.Object, mountedVolumes []corev1.Volume) []corev1.ObjectReference {
	volumes := make([]corev1.ObjectReference, 0)
	for i := range mountedVolumes {
		volume := mountedVolumes[i]
		if volume.ConfigMap != nil {
			configMap := corev1.ObjectReference{
				Kind:       "ConfigMap",
				Name:       volume.ConfigMap.Name,
				Namespace:  resource.GetNamespace(),
				APIVersion: corev1.SchemeGroupVersion.String(),
			}
			volumes = append(volumes, configMap)
		} else if volume.Secret != nil {
			secret := corev1.ObjectReference{
				Kind:       "Secret",
				Name:       volume.Secret.SecretName,
				Namespace:  resource.GetNamespace(),
				APIVersion: corev1.SchemeGroupVersion.String(),
			}
			volumes = append(volumes, secret)
		}
	}

	return volumes
}

// getStatufulSetVolumeMounts returns list of ConfigMaps/Secrets mounted by
// a StatefulSet
func (m *manager) getStatufulSetVolumeMounts(ctx context.Context,
	reloaderInfo *libsveltosv1beta1.ReloaderInfo,
) ([]corev1.ObjectReference, error) {

	statufulSet := &appsv1.StatefulSet{}
	err := m.Client.Get(ctx,
		types.NamespacedName{Namespace: reloaderInfo.Namespace, Name: reloaderInfo.Name}, statufulSet)
	if err != nil {
		return nil, err
	}

	return m.getVolumes(statufulSet, statufulSet.Spec.Template.Spec.Volumes), nil
}

// getDeploymentVolumeMounts returns list of ConfigMaps/Secrets mounted by
// a Deployment
func (m *manager) getDeploymentVolumeMounts(ctx context.Context,
	reloaderInfo *libsveltosv1beta1.ReloaderInfo,
) ([]corev1.ObjectReference, error) {

	depl := &appsv1.Deployment{}
	err := m.Client.Get(ctx,
		types.NamespacedName{Namespace: reloaderInfo.Namespace, Name: reloaderInfo.Name}, depl)
	if err != nil {
		return nil, err
	}

	return m.getVolumes(depl, depl.Spec.Template.Spec.Volumes), nil
}

// getDaemonSetVolumeMounts returns list of ConfigMaps/Secrets mounted by
// a DaemonSet
func (m *manager) getDaemonSetVolumeMounts(ctx context.Context,
	reloaderInfo *libsveltosv1beta1.ReloaderInfo,
) ([]corev1.ObjectReference, error) {

	daemonSet := &appsv1.DaemonSet{}
	err := m.Client.Get(ctx,
		types.NamespacedName{Namespace: reloaderInfo.Namespace, Name: reloaderInfo.Name}, daemonSet)
	if err != nil {
		return nil, err
	}

	return m.getVolumes(daemonSet, daemonSet.Spec.Template.Spec.Volumes), nil
}

// cleanReloaderMap stops tracking Reloader.
// reloaderMap keeps track of all Reloader instance and for each
// reloader instance list of Deployment/StatefulSet/DaemonSet referenced
// by the reloader
func (m *manager) cleanReloaderMap(reloaderName string) {
	objRef := getReloaderObjectReference(reloaderName)
	delete(reloaderMap, *objRef)
}

// cleanResourceMap cleans resourceMap by removing reloader.
// Reloader contains Deployment/StatefulSet/DaemonSet instances
// that needs to be reloaded if a mounted ConfigMap/Secret changes.
// resourceMap contains for each Deployment/StatefulSet/DaemonSet list
// of reloader instances referencing it.
func (m *manager) cleanResourceMap(reloaderName string) {
	objRef := getReloaderObjectReference(reloaderName)

	// r is a ObjectReference for a Deployment/StatefulSet/DaemonSet
	for r := range resourceMap {
		reloaderSet := resourceMap[r]
		reloaderSet.Erase(objRef)
		if reloaderSet.Len() == 0 {
			// Since no Reloader is referencing Deployment/StatefulSet/DaemonSet
			// anymore, clean volumeMap as well.
			// volumeMap contains all ConfigMap/Secret mounted by watched
			// Deployment/StatefulSet/DaemonSet
			delete(resourceMap, r)
			m.cleanVolumeMap(&r)
		}
	}
}

// cleanVolumeMap cleans volumeMap by removing resource.
// Resource represents a Deployment/StatefulSet/DaemonSet.
// volumeMap contains all ConfigMap/Secret instances mounted by
// Deployments/StatefulSets/DaemonSets referenced by a Reloader.
func (m *manager) cleanVolumeMap(resource *corev1.ObjectReference) {
	for r := range volumeMap {
		resourceSet := volumeMap[r]
		resourceSet.Erase(resource)
		if resourceSet.Len() == 0 {
			delete(volumeMap, r)
		}
	}
}
