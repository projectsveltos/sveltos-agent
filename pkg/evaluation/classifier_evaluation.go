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
	"os"
	"sync"
	"time"

	"github.com/Masterminds/semver/v3"
	"github.com/go-logr/logr"
	lua "github.com/yuin/gopher-lua"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	libsveltosv1beta1 "github.com/projectsveltos/libsveltos/api/v1beta1"
	"github.com/projectsveltos/libsveltos/lib/k8s_utils"
	logs "github.com/projectsveltos/libsveltos/lib/logsettings"
	"github.com/projectsveltos/sveltos-agent/pkg/utils"
)

type aggregatedClassification struct {
	Matching bool   `json:"matching,omitempty"`
	Message  string `json:"message"`
}

// evaluateClassifiers evaluates all classifiers awaiting evaluation
func (m *manager) evaluateClassifiers(ctx context.Context, wg *sync.WaitGroup) {
	var once sync.Once

	for {
		// Sleep before next evaluation
		time.Sleep(m.interval)

		m.log.V(logs.LogDebug).Info("Evaluating Classifiers")
		m.mu.Lock()
		// Copy queue content. That is only operation that
		// needs to be done in a mutex protect section
		jobQueueCopy := make([]string, len(m.classifierJobQueue))
		i := 0
		for k := range m.classifierJobQueue {
			jobQueueCopy[i] = k
			i++
		}
		// Reset current queue
		m.classifierJobQueue = make(map[string]bool)
		m.mu.Unlock()

		failedEvaluations := make([]string, 0)

		for i := range jobQueueCopy {
			m.log.V(logs.LogDebug).Info(fmt.Sprintf("Evaluating Classifier %s", jobQueueCopy[i]))
			err := m.evaluateClassifierInstance(ctx, jobQueueCopy[i])
			if err != nil {
				m.log.V(logs.LogInfo).Error(err,
					fmt.Sprintf("failed to evaluate classifier %s", jobQueueCopy[i]))
				failedEvaluations = append(failedEvaluations, jobQueueCopy[i])
			}
		}

		// Re-queue all Classifiers whose evaluation failed
		for i := range failedEvaluations {
			m.log.V(logs.LogDebug).Info(fmt.Sprintf("requeuing Classifier %s for evaluation", failedEvaluations[i]))
			m.EvaluateClassifier(failedEvaluations[i])
		}

		once.Do(func() {
			wg.Done()
		})
	}
}

// evaluateClassifierInstance evaluates whether current state of the cluster
// matches specified Classifier instance.
// Creates (or updates if already exists) a ClassifierReport.
func (m *manager) evaluateClassifierInstance(ctx context.Context, classifierName string) error {
	classifier := &libsveltosv1beta1.Classifier{}

	logger := m.log.WithValues("classifier", classifierName)
	logger.V(logs.LogDebug).Info("evaluating")

	err := m.Client.Get(ctx, types.NamespacedName{Name: classifierName}, classifier)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return m.cleanClassifierReport(ctx, classifierName)
		}
		return err
	}

	if !classifier.DeletionTimestamp.IsZero() {
		return m.cleanClassifierReport(ctx, classifierName)
	}

	match, err := m.isVersionAMatch(ctx, classifier)
	if err != nil {
		logger.Error(err, "failed to validate if Kubernetes version is a match")
		return err
	}

	if match {
		match, err = m.areResourcesAMatch(ctx, classifier)
		if err != nil {
			logger.Error(err, "failed to validate if current cluster resources are a match")
			return err
		}
	}

	logger.V(logs.LogDebug).Info(fmt.Sprintf("isMatch %t", match))

	err = m.createClassifierReport(ctx, classifier, match)
	if err != nil {
		logger.Error(err, "failed to create/update ClassifierReport")
		return err
	}

	if m.sendReport {
		err = m.sendClassifierReport(ctx, classifier)
		if err != nil {
			logger.Error(err, "failed to send ClassifierReport")
			return err
		}
	}

	return nil
}

// isVersionAMatch returns true if current cluster kubernetes version
// is currently a match for Classif
func (m *manager) isVersionAMatch(ctx context.Context,
	classifier *libsveltosv1beta1.Classifier) (bool, error) {

	currentVersion, err := k8s_utils.GetKubernetesVersion(ctx, m.config, m.log)
	if err != nil {
		m.log.Error(err, "failed to get cluster kubernetes version")
		return false, err
	}

	m.log.V(logs.LogDebug).Info(fmt.Sprintf("cluster version %s", currentVersion))

	currentSemVersion, err := semver.NewVersion(currentVersion)
	if err != nil {
		m.log.Error(err, "failed to get semver for current version %s", currentVersion)
		return false, err
	}

	// Build version that can be compared
	var comparableSemVersion *semver.Version
	comparableSemVersion, err = semver.NewVersion(fmt.Sprintf("%d.%d.%d",
		currentSemVersion.Major(), currentSemVersion.Minor(), currentSemVersion.Patch()))
	if err != nil {
		m.log.Error(err, "failed to create comparable semver version")
		return false, err
	}

	for i := range classifier.Spec.KubernetesVersionConstraints {
		kubernetesVersionConstraint := &classifier.Spec.KubernetesVersionConstraints[i]

		var c *semver.Constraints
		switch kubernetesVersionConstraint.Comparison {
		case string(libsveltosv1beta1.ComparisonEqual):
			c, err = semver.NewConstraint(fmt.Sprintf("= %s", kubernetesVersionConstraint.Version))
		case string(libsveltosv1beta1.ComparisonNotEqual):
			c, err = semver.NewConstraint(fmt.Sprintf("!= %s", kubernetesVersionConstraint.Version))
		case string(libsveltosv1beta1.ComparisonGreaterThan):
			c, err = semver.NewConstraint(fmt.Sprintf("> %s", kubernetesVersionConstraint.Version))
		case string(libsveltosv1beta1.ComparisonGreaterThanOrEqualTo):
			c, err = semver.NewConstraint(fmt.Sprintf(">= %s", kubernetesVersionConstraint.Version))
		case string(libsveltosv1beta1.ComparisonLessThan):
			c, err = semver.NewConstraint(fmt.Sprintf("< %s", kubernetesVersionConstraint.Version))
		case string(libsveltosv1beta1.ComparisonLessThanOrEqualTo):
			c, err = semver.NewConstraint(fmt.Sprintf("<= %s", kubernetesVersionConstraint.Version))
		}
		if err != nil {
			m.log.Error(err, "failed to build constraints")
		}

		if !c.Check(comparableSemVersion) {
			return false, nil
		}
	}
	// Check if the version meets the constraints. The a variable will be true.
	return true, nil
}

// areResourcesAMatch consider all ResourceSelectors and AggregatedClassification.
// If AggregatedClassification is not provided, a cluster is considered a match if and
// only if each ResourceSelector finds at least one matching resource.
// Otherwise, the function first collects the resources that match each ResourceSelector
// and then passes all of these resources to the AggregatedClassification function for
// further evaluation.
func (m *manager) areResourcesAMatch(ctx context.Context,
	classifier *libsveltosv1beta1.Classifier) (bool, error) {

	if classifier.Spec.DeployedResourceConstraint == nil {
		return true, nil
	}

	logger := m.log.WithValues("classifier", classifier.Name)
	var resources []*unstructured.Unstructured
	// Consider all ResourceSelectors. If AggregatedClassification is not defined,
	// a Cluster is a match only if each ResourceSelector returns at least a match.
	// If AggregatedClassification is specified, collect all resources via ResourceSelectors
	// then pass those to AggregatedClassification and let that method decide if cluster
	// is a match
	for i := range classifier.Spec.DeployedResourceConstraint.ResourceSelectors {
		rs := &classifier.Spec.DeployedResourceConstraint.ResourceSelectors[i]
		tmpResult, err := m.getResourcesForResourceSelector(ctx, rs, logger)
		if err != nil {
			return false, err
		}
		if classifier.Spec.DeployedResourceConstraint.AggregatedClassification == "" {
			// If AggregatedClassification is not specified, a Cluster is a match if
			// each ResourceSelector returns at least a match.
			if len(tmpResult) == 0 {
				logger.V(logs.LogDebug).Info(fmt.Sprintf("not a match for %s:%s:%s",
					rs.Group, rs.Version, rs.Kind))
				return false, nil
			}
		}
		logger.V(logs.LogDebug).Info(fmt.Sprintf("GVK: %s.%s:%s found %d resources",
			rs.Group, rs.Version, rs.Kind, len(tmpResult)))
		resources = append(resources, tmpResult...)
	}

	if classifier.Spec.DeployedResourceConstraint.AggregatedClassification != "" {
		return m.aggregatedClassification(classifier.Spec.DeployedResourceConstraint.AggregatedClassification,
			resources, logger)
	}

	return true, nil
}

func (m *manager) fetchClassifierDeployedResources(ctx context.Context, rs *libsveltosv1beta1.ResourceSelector,
) (*unstructured.UnstructuredList, error) {

	gvk := schema.GroupVersionKind{
		Group:   rs.Group,
		Version: rs.Version,
		Kind:    rs.Kind,
	}

	dc := discovery.NewDiscoveryClientForConfigOrDie(m.config)
	groupResources, err := restmapper.GetAPIGroupResources(dc)
	if err != nil {
		return nil, err
	}
	mapper := restmapper.NewDiscoveryRESTMapper(groupResources)

	d := dynamic.NewForConfigOrDie(m.config)

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

	if len(rs.LabelFilters) > 0 {
		labelFilter := ""
		for i := range rs.LabelFilters {
			if labelFilter != "" {
				labelFilter += ","
			}
			f := rs.LabelFilters[i]
			if f.Operation == libsveltosv1beta1.OperationEqual {
				labelFilter += fmt.Sprintf("%s=%s", f.Key, f.Value)
			} else {
				labelFilter += fmt.Sprintf("%s!=%s", f.Key, f.Value)
			}
		}

		options.LabelSelector = labelFilter
	}

	if rs.Namespace != "" {
		if options.FieldSelector != "" {
			options.FieldSelector += ","
		}
		options.FieldSelector += fmt.Sprintf("metadata.namespace=%s", rs.Namespace)
	}

	if rs.Name != "" {
		if options.FieldSelector != "" {
			options.FieldSelector += ","
		}
		options.FieldSelector += fmt.Sprintf("metadata.name=%s", rs.Name)
	}

	list, err := d.Resource(resourceId).List(ctx, options)
	if err != nil {
		return nil, err
	}

	return list, nil
}

func (m *manager) getResourcesForResourceSelector(ctx context.Context, rs *libsveltosv1beta1.ResourceSelector,
	logger logr.Logger) ([]*unstructured.Unstructured, error) {

	list, err := m.fetchClassifierDeployedResources(ctx, rs)
	if err != nil {
		return nil, err
	}

	if list == nil {
		return nil, nil
	}

	result := make([]*unstructured.Unstructured, 0)
	for i := range list.Items {
		l := logger.WithValues("resource", fmt.Sprintf("%s/%s", list.Items[i].GetNamespace(), list.Items[i].GetName()))
		var match bool
		match, err = m.isMatchForResourceSelectorScript(&list.Items[i], rs.Evaluate, l)
		if err != nil {
			return nil, err
		}
		if match {
			result = append(result, &list.Items[i])
		}
	}

	return result, nil
}

func (m *manager) aggregatedClassification(luaScript string, resources []*unstructured.Unstructured,
	logger logr.Logger) (bool, error) {

	if luaScript == "" {
		return true, nil
	}

	// Create a new Lua state
	l := lua.NewState()
	defer l.Close()

	// Load the Lua script
	if err := l.DoString(luaScript); err != nil {
		logger.V(logs.LogInfo).Info(fmt.Sprintf("doString failed: %v", err))
		return false, err
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

	var result aggregatedClassification
	err = json.Unmarshal(resultJson, &result)
	if err != nil {
		m.log.V(logs.LogInfo).Info(fmt.Sprintf("failed to marshal result: %v", err))
		return false, err
	}

	if result.Message != "" {
		logger.V(logs.LogInfo).Info(fmt.Sprintf("message: %s", result.Message))
	}

	logger.V(logs.LogInfo).Info(fmt.Sprintf("matching: %t", result.Matching))

	return result.Matching, nil
}

func (m *manager) isMatchForResourceSelectorScript(resource *unstructured.Unstructured, script string,
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

// getClassifierReport returns ClassifierReport instance that needs to be created
func (m *manager) getClassifierReport(classifierName string, isMatch bool) *libsveltosv1beta1.ClassifierReport {
	return &libsveltosv1beta1.ClassifierReport{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: utils.ReportNamespace,
			Name:      classifierName,
			Labels: map[string]string{
				libsveltosv1beta1.ClassifierlNameLabel: classifierName,
			},
		},
		Spec: libsveltosv1beta1.ClassifierReportSpec{
			ClassifierName: classifierName,
			Match:          isMatch,
		},
	}
}

// getManamegentClusterClient gets the Secret containing the Kubeconfig to access
// management cluster and return magamenet cluster client.
func (m *manager) getManamegentClusterClient(ctx context.Context, logger logr.Logger,
) (client.Client, error) {

	kubeconfigContent, err := m.getKubeconfig(ctx)
	if err != nil {
		logger.V(logs.LogInfo).Info(fmt.Sprintf("failed to get management cluster kubeconfig: %v", err))
		return nil, err
	}

	kubeconfigFile, err := os.CreateTemp("", "kubeconfig")
	if err != nil {
		logger.V(logs.LogInfo).Info(fmt.Sprintf("failed to get store kubeconfig: %v", err))
		return nil, err
	}
	defer os.Remove(kubeconfigFile.Name())

	_, err = kubeconfigFile.Write(kubeconfigContent)
	if err != nil {
		logger.V(logs.LogInfo).Info(fmt.Sprintf("failed to get store kubeconfig: %v", err))
		return nil, err
	}

	restConfig, err := clientcmd.BuildConfigFromFlags("", kubeconfigFile.Name())
	if err != nil {
		logger.V(logs.LogInfo).Info(fmt.Sprintf("failed to get management cluster config: %v", err))
		return nil, err
	}

	s := runtime.NewScheme()
	err = libsveltosv1beta1.AddToScheme(s)
	if err != nil {
		return nil, err
	}

	agentClient, err := client.New(restConfig, client.Options{Scheme: s})
	if err != nil {
		logger.V(logs.LogInfo).Info(fmt.Sprintf("failed to get management cluster client: %v", err))
		return nil, err
	}

	return agentClient, nil
}

// sendClassifierReport sends classifierReport to management cluster
func (m *manager) sendClassifierReport(ctx context.Context, classifier *libsveltosv1beta1.Classifier) error {
	logger := m.log.WithValues("classifier", classifier.Name)

	classifierReport := &libsveltosv1beta1.ClassifierReport{}
	err := m.Get(ctx,
		types.NamespacedName{Namespace: utils.ReportNamespace, Name: classifier.Name}, classifierReport)
	if err != nil {
		logger.V(logs.LogInfo).Info(fmt.Sprintf("failed to get classifier: %v", err))
		return err
	}

	agentClient, err := m.getManamegentClusterClient(ctx, logger)
	if err != nil {
		logger.V(logs.LogInfo).Info(fmt.Sprintf("failed to get management cluster client: %v", err))
		return err
	}

	logger.V(logs.LogDebug).Info("send classifierReport to management cluster")

	classifierReportName := libsveltosv1beta1.GetClassifierReportName(classifier.Name,
		m.clusterName, &m.clusterType)
	classifierReportNamespace := m.clusterNamespace

	currentClassifierReport := &libsveltosv1beta1.ClassifierReport{}

	err = agentClient.Get(ctx,
		types.NamespacedName{Namespace: classifierReportNamespace, Name: classifierReportName},
		currentClassifierReport)
	if err != nil {
		if apierrors.IsNotFound(err) {
			currentClassifierReport.Namespace = classifierReportNamespace
			currentClassifierReport.Name = classifierReportName
			currentClassifierReport.Spec = classifierReport.Spec
			currentClassifierReport.Spec.ClusterNamespace = m.clusterNamespace
			currentClassifierReport.Spec.ClusterName = m.clusterName
			currentClassifierReport.Spec.ClusterType = m.clusterType
			currentClassifierReport.Labels = libsveltosv1beta1.GetClassifierReportLabels(
				classifier.Name, m.clusterName, &m.clusterType,
			)
			return agentClient.Create(ctx, currentClassifierReport)
		}
		return err
	}

	currentClassifierReport.Namespace = classifierReportNamespace
	currentClassifierReport.Name = classifierReportName
	currentClassifierReport.Spec.ClusterType = m.clusterType
	currentClassifierReport.Spec.Match = classifierReport.Spec.Match
	currentClassifierReport.Labels = libsveltosv1beta1.GetClassifierReportLabels(
		classifier.Name, m.clusterName, &m.clusterType,
	)

	return agentClient.Update(ctx, currentClassifierReport)
}

// createClassifierReport creates ClassifierReport or updates it if already exists.
func (m *manager) createClassifierReport(ctx context.Context, classifier *libsveltosv1beta1.Classifier,
	isMatch bool) error {

	logger := m.log.WithValues("classifier", classifier.Name)

	classifierReport := &libsveltosv1beta1.ClassifierReport{}
	err := m.Get(ctx,
		types.NamespacedName{Namespace: utils.ReportNamespace, Name: classifier.Name}, classifierReport)
	if err == nil {
		return m.updateClassifierReport(ctx, classifier, isMatch, classifierReport)
	}

	if err != nil && !apierrors.IsNotFound(err) {
		logger.Error(err, "failed to get ClassifierReport")
		return err
	}

	logger.V(logs.LogInfo).Info("creating ClassifierReport")
	classifierReport = m.getClassifierReport(classifier.Name, isMatch)
	err = m.Create(ctx, classifierReport)
	if err != nil {
		logger.Error(err, "failed to create ClassifierReport")
		return err
	}

	return m.updateClassifierReportStatus(ctx, classifierReport)
}

// updateClassifierReport updates ClassifierReport
func (m *manager) updateClassifierReport(ctx context.Context, classifier *libsveltosv1beta1.Classifier,
	isMatch bool, classifierReport *libsveltosv1beta1.ClassifierReport) error {

	logger := m.log.WithValues("classifier", classifier.Name)
	logger.V(logs.LogDebug).Info("updating ClassifierReport")
	if classifierReport.Labels == nil {
		classifierReport.Labels = map[string]string{}
	}
	classifierReport.Labels[libsveltosv1beta1.ClassifierlNameLabel] = classifier.Name
	classifierReport.Spec.Match = isMatch

	err := m.Update(ctx, classifierReport)
	if err != nil {
		logger.Error(err, "failed to update ClassifierReport")
		return err
	}

	return m.updateClassifierReportStatus(ctx, classifierReport)
}

// updateClassifierReportStatus updates ClassifierReport Status by marking Phase as ReportWaitingForDelivery
func (m *manager) updateClassifierReportStatus(ctx context.Context,
	classifierReport *libsveltosv1beta1.ClassifierReport) error {

	logger := m.log.WithValues("classifierReport", classifierReport.Name)
	logger.V(logs.LogDebug).Info("updating ClassifierReport status")

	phase := libsveltosv1beta1.ReportWaitingForDelivery
	classifierReport.Status.Phase = &phase

	return m.Status().Update(ctx, classifierReport)
}

func (m *manager) cleanClassifierReport(ctx context.Context, classifierName string) error {
	// Find classifierReport and delete it. In the management cluster classifierReport
	// is removed when Classifier is removed
	classifierReport := &libsveltosv1beta1.ClassifierReport{}
	err := m.Get(ctx,
		types.NamespacedName{Namespace: utils.ReportNamespace, Name: classifierName}, classifierReport)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}

	return m.Delete(ctx, classifierReport)
}
