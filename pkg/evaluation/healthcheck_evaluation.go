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
	"sync"
	"time"

	lua "github.com/yuin/gopher-lua"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	libsveltosv1beta1 "github.com/projectsveltos/libsveltos/api/v1beta1"
	logs "github.com/projectsveltos/libsveltos/lib/logsettings"
	sveltoslua "github.com/projectsveltos/libsveltos/lib/lua"
	"github.com/projectsveltos/sveltos-agent/pkg/utils"
)

type ResourceResult struct {
	// Resource identify a Kubernetes resource
	Resource *unstructured.Unstructured `json:"resource"`

	// Status is the current status of the resource
	Status libsveltosv1beta1.HealthStatus `json:"status"`

	// Message is an optional field.
	// +optional
	Message string `json:"message,omitempty"`
}

// HealthSource.EvaluateHealth will return a struct of this type
type healthStatus struct {
	Resources []ResourceResult `json:"resources,omitempty"`
}

// evaluateHealthChecks evaluates all healthchecks awaiting evaluation
func (m *manager) evaluateHealthChecks(ctx context.Context, wg *sync.WaitGroup) {
	var once sync.Once

	for {
		// Sleep before next evaluation
		time.Sleep(m.interval)

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

		once.Do(func() {
			wg.Done()
		})
	}
}

// evaluateHealthCheckInstance evaluates healthCheck status.
// Creates (or updates if already exists) a HealthCheckReport.
func (m *manager) evaluateHealthCheckInstance(ctx context.Context, healthCheckName string) error {
	healthCheck := &libsveltosv1beta1.HealthCheck{}

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

	var status []libsveltosv1beta1.ResourceStatus
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
func (m *manager) createHealthCheckReport(ctx context.Context, healthCheck *libsveltosv1beta1.HealthCheck,
	statuses []libsveltosv1beta1.ResourceStatus) error {

	logger := m.log.WithValues("healthCheck", healthCheck.Name)

	healthCheckReport := &libsveltosv1beta1.HealthCheckReport{}
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
func (m *manager) updateHealthCheckReport(ctx context.Context, healthCheck *libsveltosv1beta1.HealthCheck,
	status []libsveltosv1beta1.ResourceStatus, healthCheckReport *libsveltosv1beta1.HealthCheckReport) error {

	logger := m.log.WithValues("healthCheck", healthCheck.Name)
	logger.V(logs.LogDebug).Info("updating healthCheckReport")
	if healthCheckReport.Labels == nil {
		healthCheckReport.Labels = map[string]string{}
	}
	healthCheckReport.Labels[libsveltosv1beta1.HealthCheckNameLabel] = healthCheck.Name
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
	healthCheckReport *libsveltosv1beta1.HealthCheckReport) error {

	logger := m.log.WithValues("healthCheckReport", healthCheckReport.Name)
	logger.V(logs.LogDebug).Info("updating healthCheckReport status")

	phase := libsveltosv1beta1.ReportWaitingForDelivery
	healthCheckReport.Status.Phase = &phase

	return m.Status().Update(ctx, healthCheckReport)
}

func (m *manager) cleanHealthCheckReport(ctx context.Context, healthCheckName string) error {
	healthCheckReport := &libsveltosv1beta1.HealthCheckReport{}
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
	phase := libsveltosv1beta1.ReportWaitingForDelivery
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
// fetch all matching resources based on ResourceSelectors.
// For every resource matching ResourceSelectors, health is evaluated using EvaluateHealth
func (m *manager) getHealthStatus(ctx context.Context, healthCheck *libsveltosv1beta1.HealthCheck,
) ([]libsveltosv1beta1.ResourceStatus, error) {

	// use ResourceSelectors to fetch all resources to be considered.
	resources, err := m.fetchHealthCheckResources(ctx, healthCheck)
	if err != nil {
		m.log.V(logs.LogInfo).Info(fmt.Sprintf("failed to fetch resources: %v", err))
		return nil, err
	}

	if resources == nil {
		return nil, nil
	}

	// Pass all selected resources using ResourceSelectors to EvaluateHealth
	healthStatus, err := m.getResourceHealthStatuses(resources, healthCheck.Spec.EvaluateHealth)
	if err != nil {
		return nil, err
	}

	if healthStatus == nil {
		return nil, nil
	}

	resourceStatuses := make([]libsveltosv1beta1.ResourceStatus, len(healthStatus.Resources))
	for i := range healthStatus.Resources {
		if healthCheck.Spec.CollectResources {
			tmpJson, err := healthStatus.Resources[i].Resource.MarshalJSON()
			if err != nil {
				return nil, err
			}
			resourceStatuses[i].Resource = tmpJson
		}
		resourceStatuses[i].ObjectRef.Kind = healthStatus.Resources[i].Resource.GetKind()
		resourceStatuses[i].ObjectRef.Namespace = healthStatus.Resources[i].Resource.GetNamespace()
		resourceStatuses[i].ObjectRef.Name = healthStatus.Resources[i].Resource.GetName()
		resourceStatuses[i].ObjectRef.APIVersion = healthStatus.Resources[i].Resource.GetAPIVersion()
		resourceStatuses[i].HealthStatus = healthStatus.Resources[i].Status
		resourceStatuses[i].Message = healthStatus.Resources[i].Message
	}

	return resourceStatuses, nil
}

// All resources selected using ResourceSelectors will be passed to EvaluateHealth
func (m *manager) getResourceHealthStatuses(resources []*unstructured.Unstructured, evaluateHealth string) (*healthStatus, error) {
	l := lua.NewState()
	defer l.Close()

	// Create an argument table
	argTable := l.NewTable()
	for i := range resources {
		obj := sveltoslua.MapToTable(resources[i].UnstructuredContent())
		argTable.Append(obj)
	}

	if err := l.DoString(evaluateHealth); err != nil {
		m.log.V(logs.LogInfo).Info(fmt.Sprintf("doString failed: %v", err))
		return nil, err
	}

	l.SetGlobal("resources", argTable)

	if err := l.CallByParam(lua.P{
		Fn:      l.GetGlobal("evaluate"), // name of Lua function
		NRet:    1,                       // number of returned values
		Protect: true,                    // return err or panic
	}, argTable); err != nil {
		m.log.V(logs.LogInfo).Info(fmt.Sprintf("failed to evaluate health for resource: %v", err))
		return nil, err
	}

	lv := l.Get(-1)
	tbl, ok := lv.(*lua.LTable)
	if !ok {
		m.log.V(logs.LogInfo).Info(sveltoslua.LuaTableError)
		return nil, fmt.Errorf("%s", sveltoslua.LuaTableError)
	}

	goResult := sveltoslua.ToGoValue(tbl)
	resultJson, err := json.Marshal(goResult)
	if err != nil {
		m.log.V(logs.LogInfo).Info(fmt.Sprintf("failed to marshal result: %v", err))
		return nil, err
	}

	var result healthStatus
	err = json.Unmarshal(resultJson, &result)
	if err != nil {
		m.log.V(logs.LogInfo).Info(fmt.Sprintf("failed to marshal result: %v", err))
		return nil, err
	}

	return &result, nil
}

// fetchHealthCheckResources fetchs all resources matching an healthCheck
func (m *manager) fetchHealthCheckResources(ctx context.Context, healthCheck *libsveltosv1beta1.HealthCheck,
) ([]*unstructured.Unstructured, error) {

	list := []*unstructured.Unstructured{}

	saNamespace, saName := m.getServiceAccountInfo(healthCheck)
	for i := range healthCheck.Spec.ResourceSelectors {
		rs := &healthCheck.Spec.ResourceSelectors[i]
		tmpList, err := m.fetchResourcesMatchingResourceSelector(ctx, rs, saNamespace, saName, m.log)
		if err != nil {
			return nil, err
		}
		list = append(list, tmpList...)
	}

	return list, nil
}

// getHealthCheckReport returns HealthCheckReport instance that needs to be created
func (m *manager) getHealthCheckReportReport(healthCheckName string,
	statuses []libsveltosv1beta1.ResourceStatus) *libsveltosv1beta1.HealthCheckReport {

	return &libsveltosv1beta1.HealthCheckReport{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: utils.ReportNamespace,
			Name:      healthCheckName,
			Labels: map[string]string{
				libsveltosv1beta1.HealthCheckNameLabel: healthCheckName,
			},
		},
		Spec: libsveltosv1beta1.HealthCheckReportSpec{
			HealthCheckName:  healthCheckName,
			ResourceStatuses: statuses,
		},
	}
}

// sendHealthCheckReport sends HealthCheckReport to management cluster. It also updates HealthCheckReport Status
// to processed in the managed cluster.
func (m *manager) sendHealthCheckReport(ctx context.Context, healthCheck *libsveltosv1beta1.HealthCheck) error {
	logger := m.log.WithValues("healthCheck", healthCheck.Name)

	healthCheckReport := &libsveltosv1beta1.HealthCheckReport{}
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

	healthCheckReportName := libsveltosv1beta1.GetHealthCheckReportName(healthCheck.Name,
		m.clusterName, &m.clusterType)
	healthCheckReportNamespace := m.clusterNamespace

	currentHealthCheckReport := &libsveltosv1beta1.HealthCheckReport{}

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
			currentHealthCheckReport.Labels = libsveltosv1beta1.GetHealthCheckReportLabels(
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
	currentHealthCheckReport.Labels = libsveltosv1beta1.GetHealthCheckReportLabels(
		healthCheck.Name, m.clusterName, &m.clusterType,
	)

	err = agentClient.Update(ctx, currentHealthCheckReport)
	if err != nil {
		return err
	}

	phase := libsveltosv1beta1.ReportProcessed
	healthCheckReport.Status.Phase = &phase
	return m.Status().Update(ctx, healthCheckReport)
}
