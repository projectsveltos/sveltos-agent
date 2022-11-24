/*
Copyright 2022.

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

package classification

import (
	"context"
	"fmt"
	"os"
	"time"

	"emperror.dev/errors"
	"github.com/Masterminds/semver"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/projectsveltos/classifier-agent/pkg/utils"
	libsveltosv1alpha1 "github.com/projectsveltos/libsveltos/api/v1alpha1"
	logs "github.com/projectsveltos/libsveltos/lib/logsettings"
)

// evaluateClassifiers evaluates all classifiers awaiting evaluation
func (m *manager) evaluateClassifiers(ctx context.Context) {
	for {
		m.log.V(logs.LogDebug).Info("Evaluating Classifiers")
		m.mu.Lock()
		// Copy queue content. That is only operation that
		// needs to be done in a mutex protect section
		jobQueueCopy := make([]string, len(m.jobQueue))
		copy(jobQueueCopy, m.jobQueue)
		// Reset current queue
		m.jobQueue = make([]string, 0)
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

		// Sleep before next evaluation
		time.Sleep(m.interval)
	}
}

// evaluateClassifierInstance evaluates whether current state of the cluster
// matches specified Classifier instance.
// Creates (or updates if already exists) a ClassifierReport.
func (m *manager) evaluateClassifierInstance(ctx context.Context, classifierName string) error {
	classifier := &libsveltosv1alpha1.Classifier{}

	logger := m.log.WithValues("classifier", classifierName)
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
	classifier *libsveltosv1alpha1.Classifier) (bool, error) {

	currentVersion, err := utils.GetKubernetesVersion(ctx, m.Client, m.log)
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

	for i := range classifier.Spec.KubernetesVersionConstraints {
		kubernetesVersionConstraint := &classifier.Spec.KubernetesVersionConstraints[i]

		var c *semver.Constraints
		switch kubernetesVersionConstraint.Comparison {
		case string(libsveltosv1alpha1.ComparisonEqual):
			c, err = semver.NewConstraint(fmt.Sprintf("= %s", kubernetesVersionConstraint.Version))
		case string(libsveltosv1alpha1.ComparisonNotEqual):
			c, err = semver.NewConstraint(fmt.Sprintf("!= %s", kubernetesVersionConstraint.Version))
		case string(libsveltosv1alpha1.ComparisonGreaterThan):
			c, err = semver.NewConstraint(fmt.Sprintf("> %s", kubernetesVersionConstraint.Version))
		case string(libsveltosv1alpha1.ComparisonGreaterThanOrEqualTo):
			c, err = semver.NewConstraint(fmt.Sprintf(">= %s", kubernetesVersionConstraint.Version))
		case string(libsveltosv1alpha1.ComparisonLessThan):
			c, err = semver.NewConstraint(fmt.Sprintf("< %s", kubernetesVersionConstraint.Version))
		case string(libsveltosv1alpha1.ComparisonLessThanOrEqualTo):
			c, err = semver.NewConstraint(fmt.Sprintf("<= %s", kubernetesVersionConstraint.Version))
		}
		if err != nil {
			m.log.Error(err, "failed to build constraints")
		}

		if !c.Check(currentSemVersion) {
			return false, nil
		}
	}
	// Check if the version meets the constraints. The a variable will be true.
	return true, nil
}

func (m *manager) areResourcesAMatch(ctx context.Context,
	classifier *libsveltosv1alpha1.Classifier) (bool, error) {

	for i := range classifier.Spec.DeployedResourceConstraints {
		r := &classifier.Spec.DeployedResourceConstraints[i]
		isMatch, err := m.isResourceAMatch(ctx, r)
		if err != nil {
			return false, err
		}
		if !isMatch {
			return false, nil
		}
	}
	return true, nil
}

func (m *manager) isResourceAMatch(ctx context.Context,
	deployedResource *libsveltosv1alpha1.DeployedResourceConstraint) (bool, error) {

	gvk := schema.GroupVersionKind{
		Group:   deployedResource.Group,
		Version: deployedResource.Version,
		Kind:    deployedResource.Kind,
	}

	dc := discovery.NewDiscoveryClientForConfigOrDie(m.config)
	groupResources, err := restmapper.GetAPIGroupResources(dc)
	if err != nil {
		return false, err
	}
	mapper := restmapper.NewDiscoveryRESTMapper(groupResources)

	d := dynamic.NewForConfigOrDie(m.config)

	mapping, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		if meta.IsNoMatchError(err) {
			return false, nil
		}
		return false, err
	}

	resourceId := schema.GroupVersionResource{
		Group:    gvk.Group,
		Version:  gvk.Version,
		Resource: mapping.Resource.Resource,
	}

	options := metav1.ListOptions{}

	if len(deployedResource.LabelFilters) > 0 {
		labelFilter := ""
		for i := range deployedResource.LabelFilters {
			if labelFilter != "" {
				labelFilter += ","
			}
			f := deployedResource.LabelFilters[i]
			if f.Operation == libsveltosv1alpha1.OperationEqual {
				labelFilter += fmt.Sprintf("%s=%s", f.Key, f.Value)
			} else {
				labelFilter += fmt.Sprintf("%s!=%s", f.Key, f.Value)
			}
		}

		options.LabelSelector = labelFilter
	}

	if len(deployedResource.FieldFilters) > 0 {
		fieldFilter := ""
		for i := range deployedResource.FieldFilters {
			if fieldFilter != "" {
				fieldFilter += ","
			}
			f := deployedResource.FieldFilters[i]
			if f.Operation == libsveltosv1alpha1.OperationEqual {
				fieldFilter += fmt.Sprintf("%s=%s", f.Field, f.Value)
			} else {
				fieldFilter += fmt.Sprintf("%s!=%s", f.Field, f.Value)
			}
		}

		options.FieldSelector = fieldFilter
	}

	if deployedResource.Namespace != "" {
		if options.FieldSelector != "" {
			options.FieldSelector += ","
		}
		options.FieldSelector += fmt.Sprintf("metadata.namespace=%s", deployedResource.Namespace)
	}

	list, err := d.Resource(resourceId).List(ctx, options)
	if err != nil {
		return false, err
	}

	if deployedResource.MinCount != nil {
		if len(list.Items) < *deployedResource.MinCount {
			return false, nil
		}
	}

	if deployedResource.MaxCount != nil {
		if len(list.Items) > *deployedResource.MaxCount {
			return false, nil
		}
	}

	return true, nil
}

// getClassifierReport returns ClassifierReport instance that needs to be created
func (m *manager) getClassifierReport(classifierName string, isMatch bool) *libsveltosv1alpha1.ClassifierReport {
	return &libsveltosv1alpha1.ClassifierReport{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: utils.ReportNamespace,
			Name:      classifierName,
			Labels: map[string]string{
				libsveltosv1alpha1.ClassifierLabelName: classifierName,
			},
		},
		Spec: libsveltosv1alpha1.ClassifierReportSpec{
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
	err = libsveltosv1alpha1.AddToScheme(s)
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
func (m *manager) sendClassifierReport(ctx context.Context, classifier *libsveltosv1alpha1.Classifier) error {
	logger := m.log.WithValues("classifier", classifier.Name)

	classifierReport := &libsveltosv1alpha1.ClassifierReport{}
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

	classifierReportName := libsveltosv1alpha1.GetClassifierReportName(classifier.Name, m.clusterName)
	classifierReportNamespace := m.clusterNamespace

	currentClassifierReport := &libsveltosv1alpha1.ClassifierReport{}

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
			currentClassifierReport.Labels = map[string]string{
				libsveltosv1alpha1.ClassifierReportClusterLabel: libsveltosv1alpha1.GetClusterInfo(m.clusterNamespace, m.clusterName),
			}
			return agentClient.Create(ctx, currentClassifierReport)
		}
		return err
	}

	currentClassifierReport.Spec.Match = classifierReport.Spec.Match
	currentClassifierReport.Labels = map[string]string{
		libsveltosv1alpha1.ClassifierReportClusterLabel: libsveltosv1alpha1.GetClusterInfo(m.clusterNamespace, m.clusterName),
	}
	return agentClient.Update(ctx, currentClassifierReport)
}

func (m *manager) getKubeconfig(ctx context.Context) ([]byte, error) {
	secret := &corev1.Secret{}
	key := client.ObjectKey{
		Namespace: libsveltosv1alpha1.ClassifierSecretNamespace,
		Name:      libsveltosv1alpha1.ClassifierSecretName,
	}

	if err := m.Get(ctx, key, secret); err != nil {
		return nil, errors.Wrap(err,
			fmt.Sprintf("Failed to get secret %s", key))
	}

	for _, contents := range secret.Data {
		return contents, nil
	}

	return nil, nil
}

// createClassifierReport creates ClassifierReport or updates it if already exists.
func (m *manager) createClassifierReport(ctx context.Context, classifier *libsveltosv1alpha1.Classifier,
	isMatch bool) error {

	logger := m.log.WithValues("classifier", classifier.Name)

	classifierReport := &libsveltosv1alpha1.ClassifierReport{}
	err := m.Get(ctx,
		types.NamespacedName{Namespace: utils.ReportNamespace, Name: classifier.Name}, classifierReport)
	if err == nil {
		return m.updateClassifierReport(ctx, classifier, isMatch, classifierReport)
	}

	if err != nil && !apierrors.IsNotFound(err) {
		logger.Error(err, "failed to get ClassifierReport")
		return err
	}

	logger.V(logs.LogDebug).Info("creating ClassifierReport")
	classifierReport = m.getClassifierReport(classifier.Name, isMatch)
	err = m.Create(ctx, classifierReport)
	if err != nil {
		logger.Error(err, "failed to create ClassifierReport")
		return err
	}

	return m.updateClassifierReportStatus(ctx, classifier)
}

// updateClassifierReport updates ClassifierReport
func (m *manager) updateClassifierReport(ctx context.Context, classifier *libsveltosv1alpha1.Classifier,
	isMatch bool, classifierReport *libsveltosv1alpha1.ClassifierReport) error {

	logger := m.log.WithValues("classifier", classifier.Name)
	logger.V(logs.LogDebug).Info("updating ClassifierReport")
	if classifierReport.Labels == nil {
		classifierReport.Labels = map[string]string{}
	}
	classifierReport.Labels[libsveltosv1alpha1.ClassifierLabelName] = classifier.Name
	classifierReport.Spec.Match = isMatch

	err := m.Update(ctx, classifierReport)
	if err != nil {
		logger.Error(err, "failed to update ClassifierReport")
		return err
	}

	return m.updateClassifierReportStatus(ctx, classifier)
}

// updateClassifierReportStatus updates ClassifierReport Status by marking Phase as ReportWaitingForDelivery
func (m *manager) updateClassifierReportStatus(ctx context.Context, classifier *libsveltosv1alpha1.Classifier) error {
	m.log.V(logs.LogDebug).Info("updating ClassifierReport status")

	logger := m.log.WithValues("classifier", classifier.Name)

	classifierReport := &libsveltosv1alpha1.ClassifierReport{}
	err := m.Get(ctx,
		types.NamespacedName{Namespace: utils.ReportNamespace, Name: classifier.Name}, classifierReport)
	if err != nil {
		logger.Error(err, "failed to get ClassifierReport")
		return err
	}

	phase := libsveltosv1alpha1.ReportWaitingForDelivery
	classifierReport.Status.Phase = &phase

	return m.Status().Update(ctx, classifierReport)
}

func (m *manager) cleanClassifierReport(ctx context.Context, classifierName string) error {
	// Find classifierReport and delete it. In the management cluster classifierReport
	// is removed when Classifier is removed
	classifierReport := &libsveltosv1alpha1.ClassifierReport{}
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
