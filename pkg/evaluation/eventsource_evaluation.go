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

	"github.com/go-logr/logr"
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

	libsveltosv1beta1 "github.com/projectsveltos/libsveltos/api/v1beta1"
	logs "github.com/projectsveltos/libsveltos/lib/logsettings"
	"github.com/projectsveltos/libsveltos/lib/roles"
	"github.com/projectsveltos/sveltos-agent/pkg/utils"
)

type eventMatchStatus struct {
	Matching bool   `json:"matching"`
	Message  string `json:"message"`
}

type aggregatedStatus struct {
	Resources []*unstructured.Unstructured `json:"resources,omitempty"`
	Message   string                       `json:"message"`
}

// evaluateEventSources evaluates all healthchecks awaiting evaluation
func (m *manager) evaluateEventSources(ctx context.Context) {
	for {
		m.log.V(logs.LogDebug).Info("Evaluating EventSources")
		m.mu.Lock()
		// Copy queue content. That is only operation that
		// needs to be done in a mutex protect section
		jobQueueCopy := make([]string, len(m.eventSourceJobQueue))
		i := 0
		for k := range m.eventSourceJobQueue {
			jobQueueCopy[i] = k
			i++
		}
		// Reset current queue
		m.eventSourceJobQueue = make(map[string]bool)
		m.mu.Unlock()

		failedEvaluations := make([]string, 0)

		for i := range jobQueueCopy {
			m.log.V(logs.LogDebug).Info(fmt.Sprintf("Evaluating EventSource %s", jobQueueCopy[i]))
			err := m.evaluateEventSourceInstance(ctx, jobQueueCopy[i])
			if err != nil {
				m.log.V(logs.LogInfo).Error(err,
					fmt.Sprintf("failed to evaluate EventSource %s", jobQueueCopy[i]))
				failedEvaluations = append(failedEvaluations, jobQueueCopy[i])
			}
		}

		// Re-queue all EventSources whose evaluation failed
		for i := range failedEvaluations {
			m.log.V(logs.LogDebug).Info(fmt.Sprintf("requeuing EventSource %s for evaluation", failedEvaluations[i]))
			m.EvaluateEventSource(failedEvaluations[i])
		}

		// Sleep before next evaluation
		time.Sleep(m.interval)
	}
}

// evaluateEventSourceInstance evaluates event status.
// Creates (or updates if already exists) a EventReport.
func (m *manager) evaluateEventSourceInstance(ctx context.Context, eventName string) error {
	event := &libsveltosv1beta1.EventSource{}

	logger := m.log.WithValues("event", eventName)
	logger.V(logs.LogDebug).Info("evaluating")

	err := m.Client.Get(ctx, types.NamespacedName{Name: eventName}, event)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return m.cleanEventReport(ctx, eventName)
		}
		return err
	}

	if !event.DeletionTimestamp.IsZero() {
		return m.cleanEventReport(ctx, eventName)
	}

	var matchinResources []corev1.ObjectReference
	var collectedResources []unstructured.Unstructured
	matchinResources, collectedResources, err = m.getEventMatchingResources(ctx, event, logger)
	if err != nil {
		return err
	}

	if collectedResources == nil || matchinResources == nil {
		err = m.createEventReport(ctx, event, matchinResources, nil)
		if err != nil {
			logger.Error(err, "failed to create/update EventReport")
			return err
		}
	} else {
		var jsonResources []byte
		jsonResources, err = m.marshalSliceOfUnstructured(collectedResources)
		if err != nil {
			return err
		}

		err = m.createEventReport(ctx, event, matchinResources, jsonResources)
		if err != nil {
			logger.Error(err, "failed to create/update EventReport")
			return err
		}
	}

	if m.sendReport {
		err = m.sendEventReport(ctx, event)
		if err != nil {
			logger.Error(err, "failed to send EventReport")
			return err
		}
	}

	return nil
}

// createEventReport creates EventReport or updates it if already exists.
func (m *manager) createEventReport(ctx context.Context, event *libsveltosv1beta1.EventSource,
	matchinResources []corev1.ObjectReference, jsonResources []byte) error {

	logger := m.log.WithValues("event", event.Name)

	eventReport := &libsveltosv1beta1.EventReport{}
	err := m.Get(ctx,
		types.NamespacedName{Namespace: utils.ReportNamespace, Name: event.Name}, eventReport)
	if err == nil {
		return m.updateEventReport(ctx, event, matchinResources, jsonResources, eventReport)
	}

	if err != nil && !apierrors.IsNotFound(err) {
		m.log.Error(err, "failed to get EventReport")
		return err
	}

	m.log.V(logs.LogInfo).Info("creating EventReport")
	eventReport = m.getEventReport(event.Name, matchinResources, jsonResources)
	err = m.Create(ctx, eventReport)
	if err != nil {
		logger.Error(err, "failed to create eventReport")
		return err
	}

	return m.updateEventReportStatus(ctx, eventReport)
}

// updateEventReport updates EventReport
func (m *manager) updateEventReport(ctx context.Context, event *libsveltosv1beta1.EventSource,
	matchinResources []corev1.ObjectReference, jsonResources []byte,
	eventReport *libsveltosv1beta1.EventReport) error {

	logger := m.log.WithValues("event", event.Name)
	logger.V(logs.LogDebug).Info("updating eventReport")
	if eventReport.Labels == nil {
		eventReport.Labels = map[string]string{}
	}
	eventReport.Labels[libsveltosv1beta1.EventSourceNameLabel] = event.Name
	eventReport.Spec.MatchingResources = matchinResources
	eventReport.Spec.Resources = jsonResources

	err := m.Update(ctx, eventReport)
	if err != nil {
		logger.Error(err, "failed to update EventReport")
		return err
	}

	return m.updateEventReportStatus(ctx, eventReport)
}

// updateEventReportStatus updates EventReport Status by marking Phase as ReportWaitingForDelivery
func (m *manager) updateEventReportStatus(ctx context.Context,
	eventReport *libsveltosv1beta1.EventReport) error {

	logger := m.log.WithValues("eventReport", eventReport.Name)
	logger.V(logs.LogDebug).Info("updating eventReport status")

	phase := libsveltosv1beta1.ReportWaitingForDelivery
	eventReport.Status.Phase = &phase

	return m.Status().Update(ctx, eventReport)
}

func (m *manager) cleanEventReport(ctx context.Context, eventName string) error {
	eventReport := &libsveltosv1beta1.EventReport{}
	err := m.Get(ctx,
		types.NamespacedName{Namespace: utils.ReportNamespace, Name: eventName}, eventReport)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}

	if !eventReport.DeletionTimestamp.IsZero() {
		// if eventReport is already marked for deletion
		// do nothing
		return nil
	}

	// reset status
	phase := libsveltosv1beta1.ReportWaitingForDelivery
	eventReport.Status.Phase = &phase

	err = m.Status().Update(ctx, eventReport)
	if err != nil {
		m.log.V(logs.LogInfo).Info(fmt.Sprintf("failed to update status: %v", err))
		return err
	}

	// Now delete it. EventReport is created with a finalizer.
	// Reconciler will remove finalizer only when EventReport is delivered to management cluster.
	return m.Delete(ctx, eventReport)
}

// getEventMatchingResources returns resources matching EventSource.
func (m *manager) getEventMatchingResources(ctx context.Context, event *libsveltosv1beta1.EventSource,
	logger logr.Logger) ([]corev1.ObjectReference, []unstructured.Unstructured, error) {

	resources, err := m.fetchResourcesMatchingEventSource(ctx, event, logger)
	if err != nil {
		m.log.V(logs.LogInfo).Info(fmt.Sprintf("failed to fetch resources: %v", err))
		return nil, nil, err
	}

	if resources == nil {
		return nil, nil, nil
	}

	matchingResources := make([]corev1.ObjectReference, 0)
	collectedResources := make([]unstructured.Unstructured, 0)

	for i := range resources {
		matchingResources = append(matchingResources,
			corev1.ObjectReference{
				Namespace:  resources[i].GetNamespace(),
				Name:       resources[i].GetName(),
				Kind:       resources[i].GetKind(),
				APIVersion: resources[i].GetAPIVersion(),
			})
		if event.Spec.CollectResources {
			collectedResources = append(collectedResources, *resources[i])
		}
	}

	return matchingResources, collectedResources, nil
}

func (m *manager) isMatchForEventSource(resource *unstructured.Unstructured, script string,
	logger logr.Logger) (bool, error) {

	if script == "" {
		return true, nil
	}

	l := lua.NewState()
	defer l.Close()

	obj := mapToTable(resource.UnstructuredContent())

	if err := l.DoString(script); err != nil {
		m.log.V(logs.LogInfo).Info(fmt.Sprintf("doString failed: %v", err))
		return false, err
	}

	l.SetGlobal("obj", obj)

	if err := l.CallByParam(lua.P{
		Fn:      l.GetGlobal("evaluate"), // name of Lua function
		NRet:    1,                       // number of returned values
		Protect: true,                    // return err or panic
	}, obj); err != nil {
		logger.V(logs.LogInfo).Info(fmt.Sprintf("failed to evaluate health for resource: %v", err))
		return false, err
	}

	lv := l.Get(-1)
	tbl, ok := lv.(*lua.LTable)
	if !ok {
		logger.V(logs.LogInfo).Info(luaTableError)
		return false, fmt.Errorf("%s", luaTableError)
	}

	goResult := toGoValue(tbl)
	resultJson, err := json.Marshal(goResult)
	if err != nil {
		logger.V(logs.LogInfo).Info(fmt.Sprintf("failed to marshal result: %v", err))
		return false, err
	}

	var result eventMatchStatus
	err = json.Unmarshal(resultJson, &result)
	if err != nil {
		logger.V(logs.LogInfo).Info(fmt.Sprintf("failed to marshal result: %v", err))
		return false, err
	}

	if result.Message != "" {
		logger.V(logs.LogInfo).Info(fmt.Sprintf("message: %s", result.Message))
	}

	logger.V(logs.LogDebug).Info(fmt.Sprintf("is a match: %t", result.Matching))

	return result.Matching, nil
}

// fetchResourcesMatchingEventSource fetchs all resources matching an event
func (m *manager) fetchResourcesMatchingEventSource(ctx context.Context,
	event *libsveltosv1beta1.EventSource, logger logr.Logger) ([]*unstructured.Unstructured, error) {

	saNamespace, saName := m.getServiceAccountInfo(event)
	list := []*unstructured.Unstructured{}
	for i := range event.Spec.ResourceSelectors {
		rs := &event.Spec.ResourceSelectors[i]
		tmpList, err := m.fetchResourcesMatchingResourceSelector(ctx, rs, saNamespace, saName, logger)
		if err != nil {
			return nil, err
		}

		list = append(list, tmpList...)
	}

	if event.Spec.AggregatedSelection != "" {
		return m.aggregatedSelection(event.Spec.AggregatedSelection, list, logger)
	}

	return list, nil
}

func (m *manager) aggregatedSelection(luaScript string, resources []*unstructured.Unstructured,
	logger logr.Logger) ([]*unstructured.Unstructured, error) {

	if luaScript == "" {
		return resources, nil
	}

	// Create a new Lua state
	l := lua.NewState()
	defer l.Close()

	// Load the Lua script
	if err := l.DoString(luaScript); err != nil {
		logger.V(logs.LogInfo).Info(fmt.Sprintf("doString failed: %v", err))
		return nil, err
	}

	// Create an argument table
	argTable := l.NewTable()
	for _, resource := range resources {
		obj := mapToTable(resource.UnstructuredContent())
		argTable.Append(obj)
	}

	l.SetGlobal("resources", argTable)

	if err := l.CallByParam(lua.P{
		Fn:      l.GetGlobal("evaluate"), // name of Lua function
		NRet:    1,                       // number of returned values
		Protect: true,                    // return err or panic
	}, argTable); err != nil {
		logger.V(logs.LogInfo).Info(fmt.Sprintf("failed to call evaluate function: %s", err.Error()))
		return nil, err
	}

	lv := l.Get(-1)
	tbl, ok := lv.(*lua.LTable)
	if !ok {
		logger.V(logs.LogInfo).Info(luaTableError)
		return nil, fmt.Errorf("%s", luaTableError)
	}

	goResult := toGoValue(tbl)
	resultJson, err := json.Marshal(goResult)
	if err != nil {
		logger.V(logs.LogInfo).Info(fmt.Sprintf("failed to marshal result: %v", err))
		return nil, err
	}

	var result aggregatedStatus
	err = json.Unmarshal(resultJson, &result)
	if err != nil {
		logger.V(logs.LogInfo).Info(fmt.Sprintf("failed to marshal result: %v", err))
		return nil, err
	}

	if result.Message != "" {
		logger.V(logs.LogInfo).Info(fmt.Sprintf("message: %s", result.Message))
	}

	for i := range result.Resources {
		l := logger.WithValues("resource", fmt.Sprintf("%s:%s/%s",
			result.Resources[i].GetKind(), result.Resources[i].GetNamespace(), result.Resources[i].GetName()))
		l.Info("found a match")
	}

	return result.Resources, nil
}

// fetchResourcesMatchingResourceSelector fetchs all resources matching an event
func (m *manager) fetchResourcesMatchingResourceSelector(ctx context.Context,
	resourceSelector *libsveltosv1beta1.ResourceSelector, saNamespace, saName string,
	logger logr.Logger) ([]*unstructured.Unstructured, error) {

	gvk := schema.GroupVersionKind{
		Group:   resourceSelector.Group,
		Version: resourceSelector.Version,
		Kind:    resourceSelector.Kind,
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

	if len(resourceSelector.LabelFilters) > 0 {
		labelFilter := ""
		for i := range resourceSelector.LabelFilters {
			if labelFilter != "" {
				labelFilter += ","
			}
			f := resourceSelector.LabelFilters[i]
			if f.Operation == libsveltosv1beta1.OperationEqual {
				labelFilter += fmt.Sprintf("%s=%s", f.Key, f.Value)
			} else {
				labelFilter += fmt.Sprintf("%s!=%s", f.Key, f.Value)
			}
		}

		options.LabelSelector = labelFilter
	}

	if resourceSelector.Namespace != "" {
		options.FieldSelector += fmt.Sprintf("metadata.namespace=%s", resourceSelector.Namespace)
	}

	if resourceSelector.Name != "" {
		if options.FieldSelector != "" {
			options.FieldSelector += ","
		}
		options.FieldSelector += fmt.Sprintf("metadata.name=%s", resourceSelector.Name)
	}

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

	m.log.V(logs.LogDebug).Info(fmt.Sprintf("found %d resources", len(list.Items)))

	resources := []*unstructured.Unstructured{}
	for i := range list.Items {
		resource := &list.Items[i]
		if !resource.GetDeletionTimestamp().IsZero() {
			continue
		}
		isMatch, err := m.isMatchForEventSource(resource, resourceSelector.Evaluate, logger)
		if err != nil {
			return nil, err
		}

		if isMatch {
			resources = append(resources, resource)
		}
	}

	return resources, nil
}

// getEventReport returns EventReport instance that needs to be created
func (m *manager) getEventReport(eventName string, matchingResources []corev1.ObjectReference,
	jsonResources []byte) *libsveltosv1beta1.EventReport {

	return &libsveltosv1beta1.EventReport{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: utils.ReportNamespace,
			Name:      eventName,
			Labels: map[string]string{
				libsveltosv1beta1.EventSourceNameLabel: eventName,
			},
		},
		Spec: libsveltosv1beta1.EventReportSpec{
			EventSourceName:   eventName,
			MatchingResources: matchingResources,
			Resources:         jsonResources,
		},
	}
}

func (m *manager) marshalSliceOfUnstructured(collectedResources []unstructured.Unstructured) ([]byte, error) {
	result := ""
	for i := range collectedResources {
		r := &collectedResources[i]
		tmpJson, err := r.MarshalJSON()
		if err != nil {
			return nil, err
		}

		result += string(tmpJson)
		result += "---"
	}

	return []byte(result), nil
}

// sendEventReport sends EventReport to management cluster. It also updates EventReport Status
// to processed in the managed cluster.
func (m *manager) sendEventReport(ctx context.Context, event *libsveltosv1beta1.EventSource) error {
	logger := m.log.WithValues("event", event.Name)

	eventReport := &libsveltosv1beta1.EventReport{}
	err := m.Get(ctx,
		types.NamespacedName{Namespace: utils.ReportNamespace, Name: event.Name}, eventReport)
	if err != nil {
		logger.V(logs.LogInfo).Info(fmt.Sprintf("failed to get event: %v", err))
		return err
	}

	agentClient, err := m.getManamegentClusterClient(ctx, logger)
	if err != nil {
		logger.V(logs.LogInfo).Info(fmt.Sprintf("failed to get management cluster client: %v", err))
		return err
	}

	logger.V(logs.LogDebug).Info("send eventReport to management cluster")

	eventReportName := libsveltosv1beta1.GetEventReportName(event.Name,
		m.clusterName, &m.clusterType)
	eventReportNamespace := m.clusterNamespace

	currentEventReport := &libsveltosv1beta1.EventReport{}

	err = agentClient.Get(ctx,
		types.NamespacedName{Namespace: eventReportNamespace, Name: eventReportName},
		currentEventReport)
	if err != nil {
		if apierrors.IsNotFound(err) {
			currentEventReport.Namespace = eventReportNamespace
			currentEventReport.Name = eventReportName
			currentEventReport.Spec = eventReport.Spec
			currentEventReport.Spec.ClusterNamespace = m.clusterNamespace
			currentEventReport.Spec.ClusterName = m.clusterName
			currentEventReport.Spec.ClusterType = m.clusterType
			currentEventReport.Labels = libsveltosv1beta1.GetEventReportLabels(
				event.Name, m.clusterName, &m.clusterType,
			)
			return agentClient.Create(ctx, currentEventReport)
		}
		return err
	}

	currentEventReport.Namespace = eventReportNamespace
	currentEventReport.Name = eventReportName
	currentEventReport.Spec.ClusterType = m.clusterType
	currentEventReport.Spec.MatchingResources = eventReport.Spec.MatchingResources
	currentEventReport.Labels = libsveltosv1beta1.GetEventReportLabels(
		event.Name, m.clusterName, &m.clusterType,
	)

	err = agentClient.Update(ctx, currentEventReport)
	if err != nil {
		return err
	}

	phase := libsveltosv1beta1.ReportProcessed
	eventReport.Status.Phase = &phase
	return m.Status().Update(ctx, eventReport)
}
