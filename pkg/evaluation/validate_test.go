package evaluation_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2/textlogger"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	libsveltosv1alpha1 "github.com/projectsveltos/libsveltos/api/v1alpha1"
	libsveltosutils "github.com/projectsveltos/libsveltos/lib/utils"
	"github.com/projectsveltos/sveltos-agent/pkg/evaluation"
	"github.com/projectsveltos/sveltos-agent/pkg/utils"
)

const (
	eventFileName      = "eventsource.yaml"
	classifierFileName = "classifier.yaml"

	matchingFileName    = "matching.yaml"
	nonMatchingFileName = "non-matching.yaml"

	healthCheckFileName = "healthcheck.yaml"
	healthyFileName     = "healthy.yaml"
	progressingFileName = "progressing.yaml"
	degradedFileName    = "degraded.yaml"
	suspendedFileName   = "suspended.yaml"
)

var _ = Describe("Event", Label("VERIFY_LUA"), func() {

	It("Verify all events", func() {
		const eventDir = "./events"

		dirs, err := os.ReadDir(eventDir)
		Expect(err).To(BeNil())

		for i := range dirs {
			if dirs[i].IsDir() {
				verifyEvents(filepath.Join(eventDir, dirs[i].Name()))
			}
		}
	})

	It("Verify all healthChecks", func() {
		const healthCheckDir = "./healthchecks"

		dirs, err := os.ReadDir(healthCheckDir)
		Expect(err).To(BeNil())

		for i := range dirs {
			if dirs[i].IsDir() {
				verifyHealthChecks(filepath.Join(healthCheckDir, dirs[i].Name()))
			}
		}
	})

	It("Verify all classifiers", func() {
		const classifierDir = "./classifiers"

		dirs, err := os.ReadDir(classifierDir)
		Expect(err).To(BeNil())

		for i := range dirs {
			if dirs[i].IsDir() {
				verifyClassifiers(filepath.Join(classifierDir, dirs[i].Name()))
			}
		}
	})
})

func verifyEvents(dirName string) {
	By(fmt.Sprintf("Verifying events %s", dirName))

	dirs, err := os.ReadDir(dirName)
	Expect(err).To(BeNil())

	fileCount := 0

	for i := range dirs {
		if dirs[i].IsDir() {
			verifyEvents(fmt.Sprintf("%s/%s", dirName, dirs[i].Name()))
		} else {
			fileCount++
		}
	}

	if fileCount > 0 {
		verifyEvent(dirName)
	}
}

func verifyHealthChecks(dirName string) {
	By(fmt.Sprintf("Verifying healthChecks %s", dirName))

	dirs, err := os.ReadDir(dirName)
	Expect(err).To(BeNil())

	fileCount := 0

	for i := range dirs {
		if dirs[i].IsDir() {
			verifyHealthChecks(fmt.Sprintf("%s/%s", dirName, dirs[i].Name()))
		} else {
			fileCount++
		}
	}

	if fileCount > 0 {
		verifyHealthCheck(dirName)
	}
}

func verifyClassifiers(dirName string) {
	By(fmt.Sprintf("Verifying classifier %s", dirName))

	dirs, err := os.ReadDir(dirName)
	Expect(err).To(BeNil())

	fileCount := 0

	for i := range dirs {
		if dirs[i].IsDir() {
			verifyClassifiers(fmt.Sprintf("%s/%s", dirName, dirs[i].Name()))
		} else {
			fileCount++
		}
	}

	if fileCount > 0 {
		verifyClassifier(dirName)
	}
}

func verifyEvent(dirName string) {
	files, err := os.ReadDir(dirName)
	Expect(err).To(BeNil())

	for i := range files {
		if files[i].IsDir() {
			verifyEvents(filepath.Join(dirName, files[i].Name()))
			continue
		}
	}

	By(fmt.Sprintf("Validating eventSource in dir: %s", dirName))
	eventSource := getEventSource(dirName)
	Expect(eventSource).ToNot(BeNil())

	matchingResources := getResources(dirName, matchingFileName)
	if matchingResources == nil {
		By(fmt.Sprintf("%s file not present", matchingFileName))
	} else {
		validateResourceSelectorLuaScripts(eventSource.Spec.ResourceSelectors, matchingResources, true)
	}

	nonMatchingResources := getResources(dirName, nonMatchingFileName)
	if nonMatchingResources == nil {
		By(fmt.Sprintf("%s file not present", nonMatchingFileName))
	} else {
		validateResourceSelectorLuaScripts(eventSource.Spec.ResourceSelectors, nonMatchingResources, false)
	}

	validateAggregatedSelectionLuaScript(eventSource, matchingResources, nonMatchingResources)
}

func verifyHealthCheck(dirName string) {
	files, err := os.ReadDir(dirName)
	Expect(err).To(BeNil())

	for i := range files {
		if files[i].IsDir() {
			verifyHealthChecks(filepath.Join(dirName, files[i].Name()))
			continue
		}
	}

	clusterNamespace := utils.ReportNamespace
	clusterName := randomString()
	clusterType := libsveltosv1alpha1.ClusterTypeCapi

	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	evaluation.InitializeManagerWithSkip(context.TODO(), textlogger.NewLogger(textlogger.NewConfig(textlogger.Verbosity(1))),
		nil, c, clusterNamespace, clusterName, clusterType, 10)
	manager := evaluation.GetManager()
	Expect(manager).ToNot(BeNil())

	By(fmt.Sprintf("Validating healthCheck in dir: %s", dirName))
	healthCheck := getHealthCheck(dirName)
	Expect(healthCheck).ToNot(BeNil())

	healthyResources := getResources(dirName, healthyFileName)
	if healthyResources == nil {
		By(fmt.Sprintf("%s file not present", healthyFileName))
	} else {
		By("Verifying healthy resource")
		result, err := evaluation.GetResourceHealthStatuses(manager, healthyResources, healthCheck.Spec.EvaluateHealth)
		Expect(err).To(BeNil())
		for i := range result.Resources {
			Expect(result.Resources[i].Status).To(Equal(libsveltosv1alpha1.HealthStatusHealthy))
		}
	}

	progressingResources := getResources(dirName, progressingFileName)
	if progressingResources == nil {
		By(fmt.Sprintf("%s file not present", progressingFileName))
	} else {
		By("Verifying progressing resource")
		result, err := evaluation.GetResourceHealthStatuses(manager, progressingResources, healthCheck.Spec.EvaluateHealth)
		Expect(err).To(BeNil())
		for i := range result.Resources {
			Expect(result.Resources[i].Status).To(Equal(libsveltosv1alpha1.HealthStatusProgressing))
		}
	}

	degradedResources := getResources(dirName, degradedFileName)
	if degradedResources == nil {
		By(fmt.Sprintf("%s file not present", degradedFileName))
	} else {
		By("Verifying degraded resource")
		result, err := evaluation.GetResourceHealthStatuses(manager, degradedResources, healthCheck.Spec.EvaluateHealth)
		Expect(err).To(BeNil())
		for i := range result.Resources {
			Expect(result.Resources[i].Status).To(Equal(libsveltosv1alpha1.HealthStatusDegraded))
		}
	}

	suspendedResources := getResources(dirName, suspendedFileName)
	if suspendedResources == nil {
		By(fmt.Sprintf("%s file not present", suspendedFileName))
	} else {
		By("Verifying suspended resource")
		result, err := evaluation.GetResourceHealthStatuses(manager, suspendedResources, healthCheck.Spec.EvaluateHealth)
		Expect(err).To(BeNil())
		for i := range result.Resources {
			Expect(result.Resources[i].Status).To(Equal(libsveltosv1alpha1.HealthStatusSuspended))
		}
	}
}

func verifyClassifier(dirName string) {
	files, err := os.ReadDir(dirName)
	Expect(err).To(BeNil())

	for i := range files {
		if files[i].IsDir() {
			verifyClassifiers(filepath.Join(dirName, files[i].Name()))
			continue
		}
	}

	clusterNamespace := utils.ReportNamespace
	clusterName := randomString()
	clusterType := libsveltosv1alpha1.ClusterTypeCapi

	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	evaluation.InitializeManagerWithSkip(context.TODO(), textlogger.NewLogger(textlogger.NewConfig(textlogger.Verbosity(1))),
		nil, c, clusterNamespace, clusterName, clusterType, 10)
	manager := evaluation.GetManager()
	Expect(manager).ToNot(BeNil())

	By(fmt.Sprintf("Validating classifier in dir: %s", dirName))
	classifier := getClassifier(dirName)
	Expect(classifier).ToNot(BeNil())

	matchingResources := getResources(dirName, matchingFileName)
	if matchingResources == nil {
		By(fmt.Sprintf("%s file not present", matchingFileName))
	} else {
		By("Verifying matching content")
		validateResourceSelectorLuaScripts(classifier.Spec.DeployedResourceConstraint.ResourceSelectors,
			matchingResources, true)
	}

	nonMatchingResources := getResources(dirName, nonMatchingFileName)
	if nonMatchingResources == nil {
		By(fmt.Sprintf("%s file not present", nonMatchingFileName))
	} else {
		By("Verifying non-matching content")
		validateResourceSelectorLuaScripts(classifier.Spec.DeployedResourceConstraint.ResourceSelectors,
			nonMatchingResources, false)
	}
}

func getEventSource(dirName string) *libsveltosv1alpha1.EventSource {
	eventSourceFileName := filepath.Join(dirName, eventFileName)
	content, err := os.ReadFile(eventSourceFileName)
	Expect(err).To(BeNil())

	u, err := libsveltosutils.GetUnstructured(content)
	Expect(err).To(BeNil())

	var eventSource libsveltosv1alpha1.EventSource
	err = runtime.DefaultUnstructuredConverter.
		FromUnstructured(u.UnstructuredContent(), &eventSource)
	Expect(err).To(BeNil())
	return &eventSource
}

func getHealthCheck(dirName string) *libsveltosv1alpha1.HealthCheck {
	healthCheckFileName := filepath.Join(dirName, healthCheckFileName)
	content, err := os.ReadFile(healthCheckFileName)
	Expect(err).To(BeNil())

	u, err := libsveltosutils.GetUnstructured(content)
	Expect(err).To(BeNil())

	var healthCheck libsveltosv1alpha1.HealthCheck
	err = runtime.DefaultUnstructuredConverter.
		FromUnstructured(u.UnstructuredContent(), &healthCheck)
	Expect(err).To(BeNil())
	return &healthCheck
}

func getClassifier(dirName string) *libsveltosv1alpha1.Classifier {
	classifierFileName := filepath.Join(dirName, classifierFileName)
	content, err := os.ReadFile(classifierFileName)
	Expect(err).To(BeNil())

	u, err := libsveltosutils.GetUnstructured(content)
	Expect(err).To(BeNil())

	var classifier libsveltosv1alpha1.Classifier
	err = runtime.DefaultUnstructuredConverter.
		FromUnstructured(u.UnstructuredContent(), &classifier)
	Expect(err).To(BeNil())
	return &classifier
}

func getResources(dirName, fileName string) []*unstructured.Unstructured {
	resourceFileName := filepath.Join(dirName, fileName)

	_, err := os.Stat(resourceFileName)
	if os.IsNotExist(err) {
		return nil
	}
	Expect(err).To(BeNil())

	content, err := os.ReadFile(resourceFileName)
	Expect(err).To(BeNil())

	resources := make([]*unstructured.Unstructured, 0)
	elements := strings.Split(string(content), "---")
	for i := range elements {
		u, err := libsveltosutils.GetUnstructured([]byte(elements[i]))
		Expect(err).To(BeNil())
		resources = append(resources, u)
	}

	return resources
}

func validateResourceSelectorLuaScripts(resourceSelectors []libsveltosv1alpha1.ResourceSelector,
	resources []*unstructured.Unstructured, expectedMatch bool) {

	for i := range resources {
		resource := resources[i]
		validateResourceSelectorLuaScript(resourceSelectors, resource, expectedMatch)
	}
}

func validateResourceSelectorLuaScript(resourceSelectors []libsveltosv1alpha1.ResourceSelector,
	resource *unstructured.Unstructured, expectedMatch bool) {

	clusterNamespace := utils.ReportNamespace
	clusterName := randomString()
	clusterType := libsveltosv1alpha1.ClusterTypeCapi

	l := textlogger.NewLogger(textlogger.NewConfig(textlogger.Verbosity(1)))
	evaluation.InitializeManagerWithSkip(context.TODO(), l, nil, testEnv, clusterNamespace, clusterName, clusterType, 10)
	manager := evaluation.GetManager()
	Expect(manager).ToNot(BeNil())

	for i := range resourceSelectors {
		rs := &resourceSelectors[i]
		if rs.Evaluate != "" {
			if rs.Kind == resource.GetKind() &&
				rs.Group == resource.GroupVersionKind().Group &&
				rs.Version == resource.GroupVersionKind().Version {

				isMatch, err := evaluation.IsMatchForEventSource(manager, resource, rs.Evaluate, l)
				Expect(err).To(BeNil())
				Expect(isMatch).To(Equal(expectedMatch))
			}
		}
	}
}

func validateAggregatedSelectionLuaScript(eventSource *libsveltosv1alpha1.EventSource,
	matchingResources []*unstructured.Unstructured, nonMatchingResources []*unstructured.Unstructured) {

	if eventSource.Spec.AggregatedSelection == "" {
		return
	}

	clusterNamespace := utils.ReportNamespace
	clusterName := randomString()
	clusterType := libsveltosv1alpha1.ClusterTypeCapi

	l := textlogger.NewLogger(textlogger.NewConfig(textlogger.Verbosity(1)))
	evaluation.InitializeManagerWithSkip(context.TODO(), l, nil, testEnv, clusterNamespace, clusterName, clusterType, 10)
	manager := evaluation.GetManager()
	Expect(manager).ToNot(BeNil())

	resources := matchingResources
	resources = append(resources, nonMatchingResources...)

	result, err := evaluation.AggregatedSelection(manager, eventSource.Spec.AggregatedSelection, resources, l)
	Expect(err).To(BeFalse())
	verifyResult(result, matchingResources, nonMatchingResources)
}
