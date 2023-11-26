/*
Copyright 2022. projectsveltos.io. All rights reserved.

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
	"encoding/json"
	"fmt"
	"time"

	lua "github.com/yuin/gopher-lua"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"

	libsveltosv1alpha1 "github.com/projectsveltos/libsveltos/api/v1alpha1"
	logs "github.com/projectsveltos/libsveltos/lib/logsettings"
	"github.com/projectsveltos/libsveltos/lib/roles"
	"github.com/projectsveltos/sveltos-agent/pkg/utils"
)

type healthCheckStatus struct {
	Status  libsveltosv1alpha1.HealthStatus `json:"status"`
	Message string                          `json:"message"`
	Ignore  bool                            `json:"ignore"`
}

// evaluateHealthChecks evaluates all healthchecks awaiting evaluation
func (m *manager) evaluateHealthChecks(ctx context.Context) {
	for {
		m.log.V(logs.LogDebug).Info("Evaluating HealthChecks")
		m.mu.Lock()
		// Copy queue content. That is only operation that
		// needs to be done in a mutex protect section
		jobQueueCopy := make([]string, len(m.healthCheckJobQueue))
		i := 0
		for k := range m.healthCheckJobQueue {
			jobQueueCopy[i] = k
			i++
		}
		// Reset current queue
		m.healthCheckJobQueue = make(map[string]bool)
		m.mu.Unlock()

		failedEvaluations := make([]string, 0)

		for i := range jobQueueCopy {
			m.log.V(logs.LogDebug).Info(fmt.Sprintf("Evaluating HealthCheck %s", jobQueueCopy[i]))
			err := m.evaluateHealthCheckInstance(ctx, jobQueueCopy[i])
			if err != nil {
				m.log.V(logs.LogInfo).Error(err,
					fmt.Sprintf("failed to evaluate HealthCheck %s", jobQueueCopy[i]))
				failedEvaluations = append(failedEvaluations, jobQueueCopy[i])
			}
		}

		// Re-queue all HealthChecks whose evaluation failed
		for i := range failedEvaluations {
			m.log.V(logs.LogDebug).Info(fmt.Sprintf("requeuing HealthCheck %s for evaluation", failedEvaluations[i]))
			m.EvaluateHealthCheck(failedEvaluations[i])
		}

		// Sleep before next evaluation
		time.Sleep(m.interval)
	}
}

// evaluateHealthCheckInstance evaluates healthCheck status.
// Creates (or updates if already exists) a HealthCheckReport.
func (m *manager) evaluateHealthCheckInstance(ctx context.Context, healthCheckName string) error {
	healthCheck := &libsveltosv1alpha1.HealthCheck{}

	logger := m.log.WithValues("healthCheck", healthCheckName)
	logger.V(logs.LogDebug).Info("evaluating")

	err := m.Client.Get(ctx, types.NamespacedName{Name: healthCheckName}, healthCheck)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return m.cleanHealthCheckReport(ctx, healthCheckName)
		}
		return err
	}

	if !healthCheck.DeletionTimestamp.IsZero() {
		return m.cleanHealthCheckReport(ctx, healthCheckName)
	}

	var status []libsveltosv1alpha1.ResourceStatus
	status, err = m.getHealthStatus(ctx, healthCheck)
	if err != nil {
		return err
	}

	err = m.createHealthCheckReport(ctx, healthCheck, status)
	if err != nil {
		logger.Error(err, "failed to create/update HealthCheckReport")
		return err
	}

	if m.sendReport {
		err = m.sendHealthCheckReport(ctx, healthCheck)
		if err != nil {
			logger.Error(err, "failed to send HealthCheckReport")
			return err
		}
	}

	return nil
}

// createHealthCheckReport creates HealthCheckReport or updates it if already exists.
func (m *manager) createHealthCheckReport(ctx context.Context, healthCheck *libsveltosv1alpha1.HealthCheck,
	statuses []libsveltosv1alpha1.ResourceStatus) error {

	logger := m.log.WithValues("healthCheck", healthCheck.Name)

	healthCheckReport := &libsveltosv1alpha1.HealthCheckReport{}
	err := m.Get(ctx,
		types.NamespacedName{Namespace: utils.ReportNamespace, Name: healthCheck.Name}, healthCheckReport)
	if err == nil {
		return m.updateHealthCheckReport(ctx, healthCheck, statuses, healthCheckReport)
	}

	if err != nil && !apierrors.IsNotFound(err) {
		m.log.Error(err, "failed to get HealthCheck")
		return err
	}

	m.log.V(logs.LogInfo).Info("creating HealthCheck")
	healthCheckReport = m.getHealthCheckReportReport(healthCheck.Name, statuses)
	err = m.Create(ctx, healthCheckReport)
	if err != nil {
		logger.Error(err, "failed to create healthCheckReport")
		return err
	}

	return m.updateHealthCheckReportStatus(ctx, healthCheckReport)
}

// updateHealthCheckReport updates HealthCheckReport
func (m *manager) updateHealthCheckReport(ctx context.Context, healthCheck *libsveltosv1alpha1.HealthCheck,
	status []libsveltosv1alpha1.ResourceStatus, healthCheckReport *libsveltosv1alpha1.HealthCheckReport) error {

	logger := m.log.WithValues("healthCheck", healthCheck.Name)
	logger.V(logs.LogDebug).Info("updating healthCheckReport")
	if healthCheckReport.Labels == nil {
		healthCheckReport.Labels = map[string]string{}
	}
	healthCheckReport.Labels[libsveltosv1alpha1.HealthCheckNameLabel] = healthCheck.Name
	healthCheckReport.Spec.ResourceStatuses = status

	err := m.Update(ctx, healthCheckReport)
	if err != nil {
		logger.Error(err, "failed to update HealthCheckReport")
		return err
	}

	return m.updateHealthCheckReportStatus(ctx, healthCheckReport)
}

// updateHealthCheckReportStatus updates HealthCheckReport Status by marking Phase as ReportWaitingForDelivery
func (m *manager) updateHealthCheckReportStatus(ctx context.Context,
	healthCheckReport *libsveltosv1alpha1.HealthCheckReport) error {

	logger := m.log.WithValues("healthCheckReport", healthCheckReport.Name)
	logger.V(logs.LogDebug).Info("updating healthCheckReport status")

	phase := libsveltosv1alpha1.ReportWaitingForDelivery
	healthCheckReport.Status.Phase = &phase

	return m.Status().Update(ctx, healthCheckReport)
}

func (m *manager) cleanHealthCheckReport(ctx context.Context, healthCheckName string) error {
	healthCheckReport := &libsveltosv1alpha1.HealthCheckReport{}
	err := m.Get(ctx,
		types.NamespacedName{Namespace: utils.ReportNamespace, Name: healthCheckName}, healthCheckReport)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}

	if !healthCheckReport.DeletionTimestamp.IsZero() {
		// if healthCheckReport is already marked for deletion
		// do nothing
		return nil
	}

	// reset status
	phase := libsveltosv1alpha1.ReportWaitingForDelivery
	healthCheckReport.Status.Phase = &phase

	err = m.Status().Update(ctx, healthCheckReport)
	if err != nil {
		m.log.V(logs.LogInfo).Info(fmt.Sprintf("failed to update status: %v", err))
		return err
	}

	// Now delete it. HealthCheckReport is created with a finalizer.
	// HealthCheck Reconciler will remove finalizer only when HealthCheckReport
	// is delivered to management cluster.
	return m.Delete(ctx, healthCheckReport)
}

// getHealthStatus returns current health status.
// All resources matching this healthCheck instance are fetched.
// For each resource, health status is checked by running the lua code present in the referenced ConfigMap (if any).
func (m *manager) getHealthStatus(ctx context.Context, healthCheck *libsveltosv1alpha1.HealthCheck,
) ([]libsveltosv1alpha1.ResourceStatus, error) {

	resources, err := m.fetchHealthCheckResources(ctx, healthCheck)
	if err != nil {
		m.log.V(logs.LogInfo).Info(fmt.Sprintf("failed to fetch resources: %v", err))
		return nil, err
	}

	if resources == nil {
		return nil, nil
	}

	resourceStatuses := make([]libsveltosv1alpha1.ResourceStatus, 0)

	for i := range resources.Items {
		resource := &resources.Items[i]
		if !resource.GetDeletionTimestamp().IsZero() {
			continue
		}
		s, err := m.getResourceHealthStatus(resource, healthCheck.Spec.Script)
		if err != nil {
			return nil, err
		}
		if s == nil {
			// Ignore
			continue
		}
		if healthCheck.Spec.CollectResources {
			tmpJson, err := resource.MarshalJSON()
			if err != nil {
				return nil, err
			}
			s.Resource = tmpJson
		}

		resourceStatuses = append(resourceStatuses, *s)
	}

	return resourceStatuses, nil
}

func (m *manager) getResourceHealthStatus(resource *unstructured.Unstructured, script string,
) (*libsveltosv1alpha1.ResourceStatus, error) {

	if script == "" {
		return &libsveltosv1alpha1.ResourceStatus{
			HealthStatus: libsveltosv1alpha1.HealthStatusHealthy,
			ObjectRef: corev1.ObjectReference{
				Namespace:  resource.GetNamespace(),
				Name:       resource.GetName(),
				Kind:       resource.GetKind(),
				APIVersion: resource.GetAPIVersion(),
			},
		}, nil
	}

	l := lua.NewState()
	defer l.Close()

	obj := mapToTable(resource.UnstructuredContent())

	if err := l.DoString(script); err != nil {
		m.log.V(logs.LogInfo).Info(fmt.Sprintf("doString failed: %v", err))
		return nil, err
	}

	l.SetGlobal("obj", obj)

	if err := l.CallByParam(lua.P{
		Fn:      l.GetGlobal("evaluate"), // name of Lua function
		NRet:    1,                       // number of returned values
		Protect: true,                    // return err or panic
	}, obj); err != nil {
		m.log.V(logs.LogInfo).Info(fmt.Sprintf("failed to evaluate health for resource: %v", err))
		return nil, err
	}

	lv := l.Get(-1)
	tbl, ok := lv.(*lua.LTable)
	if !ok {
		m.log.V(logs.LogInfo).Info(luaTableError)
		return nil, fmt.Errorf("%s", luaTableError)
	}

	goResult := toGoValue(tbl)
	resultJson, err := json.Marshal(goResult)
	if err != nil {
		m.log.V(logs.LogInfo).Info(fmt.Sprintf("failed to marshal result: %v", err))
		return nil, err
	}

	var result healthCheckStatus
	err = json.Unmarshal(resultJson, &result)
	if err != nil {
		m.log.V(logs.LogInfo).Info(fmt.Sprintf("failed to marshal result: %v", err))
		return nil, err
	}

	if result.Ignore {
		return nil, nil
	}

	return &libsveltosv1alpha1.ResourceStatus{
		HealthStatus: result.Status,
		Message:      result.Message,
		ObjectRef: corev1.ObjectReference{
			Namespace:  resource.GetNamespace(),
			Name:       resource.GetName(),
			Kind:       resource.GetKind(),
			APIVersion: resource.GetAPIVersion(),
		},
	}, nil
}

// fetchHealthCheckResources fetchs all resources matching an healthCheck
func (m *manager) fetchHealthCheckResources(ctx context.Context, healthCheck *libsveltosv1alpha1.HealthCheck,
) (*unstructured.UnstructuredList, error) {

	gvk := schema.GroupVersionKind{
		Group:   healthCheck.Spec.Group,
		Version: healthCheck.Spec.Version,
		Kind:    healthCheck.Spec.Kind,
	}

	dc := discovery.NewDiscoveryClientForConfigOrDie(m.config)
	groupResources, err := restmapper.GetAPIGroupResources(dc)
	if err != nil {
		return nil, err
	}
	mapper := restmapper.NewDiscoveryRESTMapper(groupResources)

	mapping, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		if meta.IsNoMatchError(err) {
			return nil, nil
		}
		return nil, err
	}

	resourceId := schema.GroupVersionResource{
		Group:    gvk.Group,
		Version:  gvk.Version,
		Resource: mapping.Resource.Resource,
	}

	options := metav1.ListOptions{}

	if len(healthCheck.Spec.LabelFilters) > 0 {
		labelFilter := ""
		for i := range healthCheck.Spec.LabelFilters {
			if labelFilter != "" {
				labelFilter += ","
			}
			f := healthCheck.Spec.LabelFilters[i]
			if f.Operation == libsveltosv1alpha1.OperationEqual {
				labelFilter += fmt.Sprintf("%s=%s", f.Key, f.Value)
			} else {
				labelFilter += fmt.Sprintf("%s!=%s", f.Key, f.Value)
			}
		}

		options.LabelSelector = labelFilter
	}

	if healthCheck.Spec.Namespace != "" {
		options.FieldSelector += fmt.Sprintf("metadata.namespace=%s", healthCheck.Spec.Namespace)
	}

	saNamespace, saName := m.getServiceAccountInfo(healthCheck)
	currentConfig := rest.CopyConfig(m.config)
	if saName != "" {
		saNameInManagedCluster := roles.GetServiceAccountNameInManagedCluster(saNamespace, saName)
		m.log.V(logs.LogInfo).Info(fmt.Sprintf("Impersonating serviceAccount (%s/%s) projectsveltos:%s",
			saNamespace, saName, saNameInManagedCluster))
		// ServiceAccount for a tenant admin is created in the projectsveltos namespace
		currentConfig.Impersonate = rest.ImpersonationConfig{
			UserName: fmt.Sprintf("system:serviceaccount:projectsveltos:%s", saNameInManagedCluster),
		}
	}
	d := dynamic.NewForConfigOrDie(currentConfig)
	var list *unstructured.UnstructuredList
	list, err = d.Resource(resourceId).List(ctx, options)
	if err != nil {
		return nil, err
	}

	return list, nil
}

// getHealthCheckReport returns HealthCheckReport instance that needs to be created
func (m *manager) getHealthCheckReportReport(healthCheckName string,
	statuses []libsveltosv1alpha1.ResourceStatus) *libsveltosv1alpha1.HealthCheckReport {

	return &libsveltosv1alpha1.HealthCheckReport{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: utils.ReportNamespace,
			Name:      healthCheckName,
			Labels: map[string]string{
				libsveltosv1alpha1.HealthCheckNameLabel: healthCheckName,
			},
		},
		Spec: libsveltosv1alpha1.HealthCheckReportSpec{
			HealthCheckName:  healthCheckName,
			ResourceStatuses: statuses,
		},
	}
}

// sendHealthCheckReport sends HealthCheckReport to management cluster. It also updates HealthCheckReport Status
// to processed in the managed cluster.
func (m *manager) sendHealthCheckReport(ctx context.Context, healthCheck *libsveltosv1alpha1.HealthCheck) error {
	logger := m.log.WithValues("healthCheck", healthCheck.Name)

	healthCheckReport := &libsveltosv1alpha1.HealthCheckReport{}
	err := m.Get(ctx,
		types.NamespacedName{Namespace: utils.ReportNamespace, Name: healthCheck.Name}, healthCheckReport)
	if err != nil {
		logger.V(logs.LogInfo).Info(fmt.Sprintf("failed to get healthCheck: %v", err))
		return err
	}

	agentClient, err := m.getManamegentClusterClient(ctx, logger)
	if err != nil {
		logger.V(logs.LogInfo).Info(fmt.Sprintf("failed to get management cluster client: %v", err))
		return err
	}

	logger.V(logs.LogDebug).Info("send healthCheckReport to management cluster")

	healthCheckReportName := libsveltosv1alpha1.GetHealthCheckReportName(healthCheck.Name,
		m.clusterName, &m.clusterType)
	healthCheckReportNamespace := m.clusterNamespace

	currentHealthCheckReport := &libsveltosv1alpha1.HealthCheckReport{}

	err = agentClient.Get(ctx,
		types.NamespacedName{Namespace: healthCheckReportNamespace, Name: healthCheckReportName},
		currentHealthCheckReport)
	if err != nil {
		if apierrors.IsNotFound(err) {
			currentHealthCheckReport.Namespace = healthCheckReportNamespace
			currentHealthCheckReport.Name = healthCheckReportName
			currentHealthCheckReport.Spec = healthCheckReport.Spec
			currentHealthCheckReport.Spec.ClusterNamespace = m.clusterNamespace
			currentHealthCheckReport.Spec.ClusterName = m.clusterName
			currentHealthCheckReport.Spec.ClusterType = m.clusterType
			currentHealthCheckReport.Labels = libsveltosv1alpha1.GetHealthCheckReportLabels(
				healthCheck.Name, m.clusterName, &m.clusterType,
			)
			return agentClient.Create(ctx, currentHealthCheckReport)
		}
		return err
	}

	currentHealthCheckReport.Namespace = healthCheckReportNamespace
	currentHealthCheckReport.Name = healthCheckReportName
	currentHealthCheckReport.Spec.ClusterType = m.clusterType
	currentHealthCheckReport.Spec.ResourceStatuses = healthCheckReport.Spec.ResourceStatuses
	currentHealthCheckReport.Labels = libsveltosv1alpha1.GetHealthCheckReportLabels(
		healthCheck.Name, m.clusterName, &m.clusterType,
	)

	err = agentClient.Update(ctx, currentHealthCheckReport)
	if err != nil {
		return err
	}

	phase := libsveltosv1alpha1.ReportProcessed
	healthCheckReport.Status.Phase = &phase
	return m.Status().Update(ctx, healthCheckReport)
}
