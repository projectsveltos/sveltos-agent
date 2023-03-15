package evaluation_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	libsveltosv1alpha1 "github.com/projectsveltos/libsveltos/api/v1alpha1"
	libsveltosutils "github.com/projectsveltos/libsveltos/lib/utils"
	"github.com/projectsveltos/sveltos-agent/pkg/evaluation"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2/klogr"
)

const (
	eventFileName = "eventsource.yaml"

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

func verifyEvent(dirName string) {
	files, err := os.ReadDir(dirName)
	Expect(err).To(BeNil())

	for i := range files {
		if files[i].IsDir() {
			verifyEvents(filepath.Join(dirName, files[i].Name()))
			continue
		}
	}

	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	evaluation.InitializeManagerWithSkip(context.TODO(), klogr.New(), nil, c, 10)
	manager := evaluation.GetManager()
	Expect(manager).ToNot(BeNil())

	By(fmt.Sprintf("Validating eventSource in dir: %s", dirName))
	eventSource := getEventSource(dirName)
	Expect(eventSource).ToNot(BeNil())

	matchingResource := getResource(dirName, matchingFileName)
	if matchingResource == nil {
		By(fmt.Sprintf("%s file not present", matchingFileName))
	} else {
		By("Verifying matching content")
		isMatch, err := evaluation.IsMatchForEventSource(manager, matchingResource, eventSource.Spec.Script, klogr.New())
		Expect(err).To(BeNil())
		Expect(isMatch).To(BeTrue())
	}

	nonMatchingResource := getResource(dirName, nonMatchingFileName)
	if nonMatchingResource == nil {
		By(fmt.Sprintf("%s file not present", nonMatchingFileName))
	} else {
		By("Verifying non-matching content")
		isMatch, err := evaluation.IsMatchForEventSource(manager, nonMatchingResource, eventSource.Spec.Script, klogr.New())
		Expect(err).To(BeNil())
		Expect(isMatch).To(BeFalse())
	}
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

	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	evaluation.InitializeManagerWithSkip(context.TODO(), klogr.New(), nil, c, 10)
	manager := evaluation.GetManager()
	Expect(manager).ToNot(BeNil())

	By(fmt.Sprintf("Validating healthCheck in dir: %s", dirName))
	healthCheck := getHealthCheck(dirName)
	Expect(healthCheck).ToNot(BeNil())

	healthyResource := getResource(dirName, healthyFileName)
	if healthyResource == nil {
		By(fmt.Sprintf("%s file not present", healthyFileName))
	} else {
		By("Verifying healthy resource")
		result, err := evaluation.GetResourceHealthStatus(manager, healthyResource, healthCheck.Spec.Script)
		Expect(err).To(BeNil())
		Expect(result.HealthStatus).To(Equal(libsveltosv1alpha1.HealthStatusHealthy))
	}

	progressingResource := getResource(dirName, progressingFileName)
	if progressingResource == nil {
		By(fmt.Sprintf("%s file not present", progressingFileName))
	} else {
		By("Verifying progressing resource")
		result, err := evaluation.GetResourceHealthStatus(manager, progressingResource, healthCheck.Spec.Script)
		Expect(err).To(BeNil())
		Expect(result.HealthStatus).To(Equal(libsveltosv1alpha1.HealthStatusProgressing))
	}

	degradedResource := getResource(dirName, degradedFileName)
	if degradedResource == nil {
		By(fmt.Sprintf("%s file not present", degradedFileName))
	} else {
		By("Verifying degraded resource")
		result, err := evaluation.GetResourceHealthStatus(manager, degradedResource, healthCheck.Spec.Script)
		Expect(err).To(BeNil())
		Expect(result.HealthStatus).To(Equal(libsveltosv1alpha1.HealthStatusDegraded))
	}

	suspendedResource := getResource(dirName, suspendedFileName)
	if suspendedResource == nil {
		By(fmt.Sprintf("%s file not present", suspendedFileName))
	} else {
		By("Verifying suspended resource")
		result, err := evaluation.GetResourceHealthStatus(manager, suspendedResource, healthCheck.Spec.Script)
		Expect(err).To(BeNil())
		Expect(result.HealthStatus).To(Equal(libsveltosv1alpha1.HealthStatusSuspended))
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

func getResource(dirName, fileName string) *unstructured.Unstructured {
	resourceFileName := filepath.Join(dirName, fileName)

	_, err := os.Stat(resourceFileName)
	if os.IsNotExist(err) {
		return nil
	}
	Expect(err).To(BeNil())

	content, err := os.ReadFile(resourceFileName)
	Expect(err).To(BeNil())

	u, err := libsveltosutils.GetUnstructured(content)
	Expect(err).To(BeNil())

	return u
}
