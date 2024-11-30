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

package evaluation_test

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	"github.com/go-logr/logr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2/textlogger"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	libsveltosv1beta1 "github.com/projectsveltos/libsveltos/api/v1beta1"
	"github.com/projectsveltos/libsveltos/lib/k8s_utils"
	"github.com/projectsveltos/sveltos-agent/pkg/evaluation"
	"github.com/projectsveltos/sveltos-agent/pkg/utils"
)

var (
	// returns true for deployments with availableReplicas less than requested replicas
	degradedDeploymentLuaScript = `
function evaluate()
	hs = {}
	hs.matching = false
	hs.message = ""
    if obj.status ~= nil then
        if obj.status.availableReplicas ~= nil then
			if obj.status.availableReplicas == obj.spec.replicas then
				hs.matching = false
				hs.message = "Available replicas matches requested replicas"
			end
        	if obj.status.availableReplicas ~= obj.spec.replicas then
				hs.matching = true
				hs.message = "Available replicas does not match requested replicas"
            end
        end
	end
    return hs
end
`

	// Needs in order:
	// - replicas (spec)
	// - availableReplicas (status)
	// - readyReplicas (status)
	// - replicas (status)
	deploymentTemplate = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: nginx-deployment
  namespace: default
  labels:
    app: nginx
spec:
  replicas: %d
  selector:
    matchLabels:
      app: nginx
  template:
    metadata:
      labels:
        app: nginx
    spec:
      containers:
      - name: nginx
        image: nginx:1.14.2
        ports:
        - containerPort: 80
status:
  availableReplicas: %d
  readyReplicas: %d
  replicas: %d`
)

var _ = Describe("Manager: healthcheck evaluation", func() {
	var eventSource *libsveltosv1beta1.EventSource
	var clusterNamespace string
	var clusterName string
	var clusterType libsveltosv1beta1.ClusterType
	var logger logr.Logger

	BeforeEach(func() {
		evaluation.Reset()
		clusterNamespace = utils.ReportNamespace
		clusterName = randomString()
		clusterType = libsveltosv1beta1.ClusterTypeCapi
		logger = textlogger.NewLogger(textlogger.NewConfig(textlogger.Verbosity(1)))
	})

	AfterEach(func() {
		if eventSource != nil {
			err := testEnv.Client.Delete(context.TODO(), eventSource)
			if err != nil {
				Expect(apierrors.IsNotFound(err)).To(BeTrue())
			}
		}
	})

	It("isMatchForEventSource: evaluates deployment as match", func() {
		evaluation.InitializeManagerWithSkip(context.TODO(), logger,
			testEnv.Config, testEnv.Client, clusterNamespace, clusterName, clusterType, 10)

		manager := evaluation.GetManager()
		Expect(manager).ToNot(BeNil())

		replicas := 3
		depl, err := k8s_utils.GetUnstructured([]byte(fmt.Sprintf(deploymentTemplate, replicas, 1, 1, 1)))
		Expect(err).To(BeNil())

		var isMatch bool
		isMatch, err = evaluation.IsMatchForEventSource(manager, depl, degradedDeploymentLuaScript,
			logger)
		Expect(err).To(BeNil())
		Expect(isMatch).To(BeTrue())
	})

	It("isMatchForEventSource: evaluates deployment as non match", func() {
		evaluation.InitializeManagerWithSkip(context.TODO(), logger,
			testEnv.Config, testEnv.Client, clusterNamespace, clusterName, clusterType, 10)

		manager := evaluation.GetManager()
		Expect(manager).ToNot(BeNil())

		replicas := 3
		depl, err := k8s_utils.GetUnstructured([]byte(fmt.Sprintf(deploymentTemplate, replicas,
			replicas, replicas, replicas)))
		Expect(err).To(BeNil())

		var isMatch bool
		isMatch, err = evaluation.IsMatchForEventSource(manager, depl, degradedDeploymentLuaScript,
			logger)
		Expect(err).To(BeNil())
		Expect(isMatch).To(BeFalse()) // Not a match because availableReplicas = replicas
	})

	It("fetchEventSourceResources: fetches all resources mathing label selectors", func() {
		namespaces := make([]string, 0)

		namespace := randomString()
		labels := map[string]string{
			randomString(): randomString(),
			randomString(): randomString(),
		}
		labelFilters := make([]libsveltosv1beta1.LabelFilter, 0)
		for k := range labels {
			labelFilters = append(labelFilters, libsveltosv1beta1.LabelFilter{
				Key: k, Operation: libsveltosv1beta1.OperationEqual, Value: labels[k],
			})
		}

		By("Create namespace")
		createNamespace(namespace)

		namespaces = append(namespaces, namespace)
		matchingServices := 3
		By(fmt.Sprintf("Creating %d services in the correct namespace with proper labels", matchingServices))
		for i := 0; i < matchingServices; i++ {
			service := getService()
			service.Namespace = namespace
			service.Labels = labels
			Expect(testEnv.Create(context.TODO(), service)).To(Succeed())
		}

		By(fmt.Sprintf("Creating %d services in the incorrect namespace with proper labels", matchingServices))
		for i := 0; i < matchingServices; i++ {
			service := getService()
			randomNamespace := namespace + randomString()
			createNamespace(randomNamespace)
			service.Namespace = randomNamespace
			namespaces = append(namespaces, randomNamespace)
			service.Labels = labels
			Expect(testEnv.Create(context.TODO(), service)).To(Succeed())
		}

		By(fmt.Sprintf("Creating %d services in the correct namespace with wrong labels", matchingServices))
		for i := 0; i < matchingServices; i++ {
			service := getService()
			service.Namespace = namespace
			service.Labels = map[string]string{randomString(): randomString()}
			Expect(testEnv.Create(context.TODO(), service)).To(Succeed())
			waitForObject(context.TODO(), testEnv.Client, service)
		}

		evaluation.InitializeManagerWithSkip(context.TODO(), logger,
			testEnv.Config, testEnv.Client, clusterNamespace, clusterName, clusterType, 10)

		eventSource = &libsveltosv1beta1.EventSource{
			ObjectMeta: metav1.ObjectMeta{
				Name: randomString(),
			},
			Spec: libsveltosv1beta1.EventSourceSpec{
				ResourceSelectors: []libsveltosv1beta1.ResourceSelector{
					{
						Group:        "",
						Version:      "v1",
						Kind:         "Service",
						Namespace:    namespace,
						LabelFilters: labelFilters,
					},
				},
			},
		}

		manager := evaluation.GetManager()
		Expect(manager).ToNot(BeNil())
		deployments, err := evaluation.FetchResourcesMatchingEventSource(manager, context.TODO(), eventSource,
			logger)
		Expect(err).To(BeNil())
		Expect(len(deployments)).To(Equal(matchingServices))

		By("Deleting all created namespaces")
		for i := range namespaces {
			ns := &corev1.Namespace{}
			Expect(testEnv.Get(context.TODO(), types.NamespacedName{Name: namespaces[i]}, ns)).To(Succeed())
			Expect(testEnv.Delete(context.TODO(), ns)).To(Succeed())
		}
	})

	It("getEventMatchingResources: returns resources matching an EventSource", func() {
		namespace := randomString()
		createNamespace(namespace)

		By("Creating a deployment in healthy state")
		healthyDepl := getDeployment(1, 1)
		healthyDepl.Namespace = namespace
		Expect(testEnv.Create(context.TODO(), healthyDepl)).To(Succeed())
		waitForObject(context.TODO(), testEnv.Client, healthyDepl)

		healthyDepl.Status.AvailableReplicas = 1
		healthyDepl.Status.ReadyReplicas = 1
		healthyDepl.Status.Replicas = 1
		Expect(testEnv.Status().Update(context.TODO(), healthyDepl)).To(Succeed())

		By("Creating a deployment in progressing state")
		progressingDepl := getDeployment(2, 1)
		progressingDepl.Namespace = namespace
		Expect(testEnv.Create(context.TODO(), progressingDepl)).To(Succeed())
		waitForObject(context.TODO(), testEnv.Client, progressingDepl)
		progressingDepl.Status.AvailableReplicas = 1
		progressingDepl.Status.ReadyReplicas = 1
		progressingDepl.Status.Replicas = 1
		Expect(testEnv.Status().Update(context.TODO(), progressingDepl)).To(Succeed())

		randomNs := namespace + randomString()
		createNamespace(randomNs)

		By("Creating a deployment in different namespace")
		healthyNonMatchingDepl := getDeployment(1, 1)
		healthyNonMatchingDepl.Namespace = randomNs
		Expect(testEnv.Create(context.TODO(), healthyNonMatchingDepl)).To(Succeed())
		waitForObject(context.TODO(), testEnv.Client, healthyNonMatchingDepl)

		By("Creating an EventSource matching the Deployments")
		eventSource = &libsveltosv1beta1.EventSource{
			ObjectMeta: metav1.ObjectMeta{
				Name: randomString(),
			},
			Spec: libsveltosv1beta1.EventSourceSpec{
				ResourceSelectors: []libsveltosv1beta1.ResourceSelector{
					{
						Group:     "apps",
						Version:   "v1",
						Kind:      "Deployment",
						Namespace: namespace,
						Evaluate:  degradedDeploymentLuaScript,
					},
				},
				CollectResources: true,
			},
		}

		evaluation.InitializeManagerWithSkip(context.TODO(), logger,
			testEnv.Config, testEnv.Client, clusterNamespace, clusterName, clusterType, 10)

		manager := evaluation.GetManager()
		Expect(manager).ToNot(BeNil())

		matchingResources, collectedResources, err :=
			evaluation.GetEventMatchingResources(manager, context.TODO(), eventSource,
				logger)
		Expect(err).To(BeNil())
		Expect(len(matchingResources)).To(Equal(1))
		Expect(matchingResources[0].Name).To(Equal(progressingDepl.Name))
		Expect(collectedResources).ToNot(BeNil())
		Expect(len(collectedResources)).To(Equal(1))
	})

	It("fetchResourcesMatchingResourceSelector selects a specific resource", func() {
		evaluation.InitializeManagerWithSkip(context.TODO(), logger,
			testEnv.Config, testEnv.Client, clusterNamespace, clusterName, clusterType, 10)

		manager := evaluation.GetManager()
		Expect(manager).ToNot(BeNil())

		namespace := randomString()
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: namespace,
			},
		}
		Expect(testEnv.Create(context.TODO(), ns)).To(Succeed())
		waitForObject(context.TODO(), testEnv, ns)

		serviceAccount := &corev1.ServiceAccount{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: namespace,
				Name:      randomString(),
			},
		}
		Expect(testEnv.Create(context.TODO(), serviceAccount)).To(Succeed())
		waitForObject(context.TODO(), testEnv, serviceAccount)

		eventSource = &libsveltosv1beta1.EventSource{
			ObjectMeta: metav1.ObjectMeta{
				Name: randomString(),
			},
			Spec: libsveltosv1beta1.EventSourceSpec{
				ResourceSelectors: []libsveltosv1beta1.ResourceSelector{
					{
						Group:     "",
						Version:   "v1",
						Kind:      "ServiceAccount",
						Namespace: namespace,
						Name:      serviceAccount.Name,
					},
				},
			},
		}

		resources, err := evaluation.FetchResourcesMatchingResourceSelector(manager,
			context.TODO(), &eventSource.Spec.ResourceSelectors[0],
			"", "", logger)
		Expect(err).To(BeNil())
		Expect(len(resources)).To(Equal(1))
		Expect(resources[0].GetNamespace()).To(Equal(serviceAccount.Namespace))
		Expect(resources[0].GetName()).To(Equal(serviceAccount.Name))
	})

	It("createEventReport creates eventReport", func() {
		evaluation.InitializeManagerWithSkip(context.TODO(), logger,
			testEnv.Config, testEnv.Client, clusterNamespace, clusterName, clusterType, 10)

		manager := evaluation.GetManager()
		Expect(manager).ToNot(BeNil())

		namespace := randomString()

		eventSource = &libsveltosv1beta1.EventSource{
			ObjectMeta: metav1.ObjectMeta{
				Name: randomString(),
			},
			Spec: libsveltosv1beta1.EventSourceSpec{
				ResourceSelectors: []libsveltosv1beta1.ResourceSelector{
					{
						Group:     "apps",
						Version:   "v1",
						Kind:      "Deployment",
						Namespace: namespace,
					},
				},
			},
		}

		deplRef := &corev1.ObjectReference{
			Namespace:  namespace,
			Name:       randomString(),
			APIVersion: "apps/v1",
			Kind:       "Deployment",
		}
		matchingResources := []corev1.ObjectReference{*deplRef}

		Expect(evaluation.CreateEventReport(manager, context.TODO(), eventSource, matchingResources, nil)).To(Succeed())

		Eventually(func() bool {
			eventReport := &libsveltosv1beta1.EventReport{}
			err := testEnv.Get(context.TODO(),
				types.NamespacedName{Namespace: utils.ReportNamespace, Name: eventSource.Name},
				eventReport)
			if err != nil {
				return false
			}
			return eventReport.Status.Phase != nil
		}, timeout, pollingInterval).Should(BeTrue())
	})

	It("createEventReport updates eventReport", func() {
		evaluation.InitializeManagerWithSkip(context.TODO(), logger,
			testEnv.Config, testEnv.Client, clusterNamespace, clusterName, clusterType, 10)

		manager := evaluation.GetManager()
		Expect(manager).ToNot(BeNil())

		namespace := randomString()

		eventSource = &libsveltosv1beta1.EventSource{
			ObjectMeta: metav1.ObjectMeta{
				Name: randomString(),
			},
			Spec: libsveltosv1beta1.EventSourceSpec{
				ResourceSelectors: []libsveltosv1beta1.ResourceSelector{
					{
						Group:     "apps",
						Version:   "v1",
						Kind:      "Deployment",
						Namespace: namespace,
					},
				},
			},
		}

		eventReport := &libsveltosv1beta1.EventReport{
			ObjectMeta: metav1.ObjectMeta{
				Name:      eventSource.Name,
				Namespace: utils.ReportNamespace,
			},
		}

		Expect(testEnv.Client.Create(context.TODO(), eventReport)).To(Succeed())
		waitForObject(context.TODO(), testEnv.Client, eventReport)

		deplRef := &corev1.ObjectReference{
			Namespace:  namespace,
			Name:       randomString(),
			APIVersion: "apps/v1",
			Kind:       "Deployment",
		}
		matchingResources := []corev1.ObjectReference{*deplRef}
		Expect(evaluation.CreateEventReport(manager, context.TODO(), eventSource, matchingResources, nil)).To(Succeed())

		Eventually(func() bool {
			eventReport := &libsveltosv1beta1.EventReport{}
			err := testEnv.Get(context.TODO(), types.NamespacedName{Namespace: utils.ReportNamespace, Name: eventSource.Name},
				eventReport)
			if err != nil {
				return false
			}
			return eventReport.Status.Phase != nil &&
				reflect.DeepEqual(eventReport.Spec.MatchingResources, matchingResources)
		}, timeout, pollingInterval).Should(BeTrue())
	})

	It("sendEventReport sends EventReport to management cluster", func() {
		eventSource = &libsveltosv1beta1.EventSource{
			ObjectMeta: metav1.ObjectMeta{
				Name: randomString(),
			},
			Spec: libsveltosv1beta1.EventSourceSpec{
				ResourceSelectors: []libsveltosv1beta1.ResourceSelector{
					{
						Group:   "",
						Version: "v1",
						Kind:    "Namespace",
					},
				},
			},
		}
		Expect(testEnv.Create(context.TODO(), eventSource)).To(Succeed())
		waitForObject(context.TODO(), testEnv.Client, eventSource)

		phase := libsveltosv1beta1.ReportDelivering
		eventReport := &libsveltosv1beta1.EventReport{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: utils.ReportNamespace,
				Name:      eventSource.Name,
			},
			Spec: libsveltosv1beta1.EventReportSpec{
				EventSourceName: eventSource.Name,
			},
			Status: libsveltosv1beta1.EventReportStatus{
				Phase: &phase,
			},
		}

		Expect(testEnv.Create(context.TODO(), eventReport)).To(Succeed())
		waitForObject(context.TODO(), testEnv.Client, eventReport)

		evaluation.InitializeManagerWithSkip(context.TODO(), logger,
			testEnv.Config, testEnv.Client, clusterNamespace, clusterName, clusterType, 10)
		manager := evaluation.GetManager()

		Expect(evaluation.SendEventReport(manager, context.TODO(), eventSource)).To(Succeed())

		// SendEventReport creates eventReport in manager.clusterNamespace

		// Use Eventually so cache is in sync
		eventReportName := libsveltosv1beta1.GetEventReportName(eventSource.Name, clusterName, &clusterType)
		Eventually(func() error {
			currentEventReport := &libsveltosv1beta1.EventReport{}
			return testEnv.Get(context.TODO(),
				types.NamespacedName{Namespace: clusterNamespace, Name: eventReportName}, currentEventReport)
		}, timeout, pollingInterval).Should(BeNil())

		currentEventReport := &libsveltosv1beta1.EventReport{}
		Expect(testEnv.Get(context.TODO(),
			types.NamespacedName{Namespace: clusterNamespace, Name: eventReportName}, currentEventReport)).To(Succeed())
		Expect(currentEventReport.Spec.ClusterName).To(Equal(clusterName))
		Expect(currentEventReport.Spec.ClusterNamespace).To(Equal(clusterNamespace))
		Expect(currentEventReport.Spec.EventSourceName).To(Equal(eventSource.Name))
		Expect(currentEventReport.Spec.ClusterType).To(Equal(clusterType))
		v, ok := currentEventReport.Labels[libsveltosv1beta1.EventReportClusterNameLabel]
		Expect(ok).To(BeTrue())
		Expect(v).To(Equal(clusterName))

		v, ok = currentEventReport.Labels[libsveltosv1beta1.EventReportClusterTypeLabel]
		Expect(ok).To(BeTrue())
		Expect(v).To(Equal(strings.ToLower(string(libsveltosv1beta1.ClusterTypeCapi))))

		v, ok = currentEventReport.Labels[libsveltosv1beta1.EventSourceNameLabel]
		Expect(ok).To(BeTrue())
		Expect(v).To(Equal(eventSource.Name))
	})

	It("cleanEventReport marks eventReport as deleted", func() {
		phase := libsveltosv1beta1.ReportDelivering
		eventReport := &libsveltosv1beta1.EventReport{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: utils.ReportNamespace,
				Name:      eventSource.Name,
				Finalizers: []string{
					libsveltosv1beta1.EventReportFinalizer,
				},
			},
			Spec: libsveltosv1beta1.EventReportSpec{
				EventSourceName: eventSource.Name,
			},
			Status: libsveltosv1beta1.EventReportStatus{
				Phase: &phase,
			},
		}

		initObjects := []client.Object{
			eventReport,
		}

		c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(initObjects...).
			WithObjects(initObjects...).Build()

		evaluation.InitializeManagerWithSkip(context.TODO(), logger,
			nil, c, clusterNamespace, clusterName, clusterType, 10)

		manager := evaluation.GetManager()
		Expect(manager).ToNot(BeNil())

		Expect(evaluation.CleanEventReport(manager, context.TODO(), eventReport.Name)).To(Succeed())

		err := c.Get(context.TODO(),
			types.NamespacedName{Name: eventReport.Name, Namespace: utils.ReportNamespace},
			eventReport)
		Expect(err).To(BeNil())
		Expect(eventReport.DeletionTimestamp.IsZero()).To(BeFalse())

		phase = libsveltosv1beta1.ReportWaitingForDelivery
		Expect(eventReport.Status.Phase).ToNot(BeNil())
		Expect(*eventReport.Status.Phase).To(Equal(phase))
	})

	It("MarshalSliceOfUnstructured", func() {
		key := randomString()
		value := randomString()

		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: randomString(),
			},
		}
		Expect(testEnv.Create(context.TODO(), ns)).To(Succeed())
		waitForObject(context.TODO(), testEnv.Client, ns)

		depl1 := getDeployment(3, 3)
		depl1.Namespace = ns.Name
		depl1.Labels = map[string]string{key: value}
		Expect(testEnv.Create(context.TODO(), depl1)).To(Succeed())
		waitForObject(context.TODO(), testEnv.Client, depl1)

		depl2 := getDeployment(3, 3)
		depl2.Namespace = ns.Name
		depl2.Labels = map[string]string{key: value}
		Expect(testEnv.Create(context.TODO(), depl2)).To(Succeed())
		waitForObject(context.TODO(), testEnv.Client, depl2)

		eventSource = &libsveltosv1beta1.EventSource{
			ObjectMeta: metav1.ObjectMeta{
				Name: randomString(),
			},
			Spec: libsveltosv1beta1.EventSourceSpec{
				ResourceSelectors: []libsveltosv1beta1.ResourceSelector{
					{
						Group:   "apps",
						Version: "v1",
						Kind:    "Deployment",
						LabelFilters: []libsveltosv1beta1.LabelFilter{
							{Key: key, Operation: libsveltosv1beta1.OperationEqual, Value: value},
						},
						Namespace: ns.Name,
					},
				},
				CollectResources: true,
			},
		}

		evaluation.InitializeManagerWithSkip(context.TODO(), logger,
			testEnv.Config, testEnv.Client, clusterNamespace, clusterName, clusterType, 10)

		manager := evaluation.GetManager()
		Expect(manager).ToNot(BeNil())

		_, collectedRespurce, err := evaluation.GetEventMatchingResources(manager, context.TODO(), eventSource,
			logger)
		Expect(err).To(BeNil())
		Expect(collectedRespurce).ToNot(BeNil())
		Expect(len(collectedRespurce)).To(Equal(2))
		var result []byte
		result, err = evaluation.MarshalSliceOfUnstructured(manager, collectedRespurce)
		Expect(err).To(BeNil())
		Expect(result).ToNot(BeNil())

		elements := strings.Split(string(result), "---")
		Expect(len(elements)).ToNot(BeZero())

		foundDepl1 := false
		foundDepl2 := false
		for i := range elements {
			if elements[i] == "" {
				continue
			}

			var policy *unstructured.Unstructured
			policy, err = k8s_utils.GetUnstructured([]byte(elements[i]))
			Expect(err).To(BeNil())

			if policy.GetNamespace() == depl1.Namespace && policy.GetName() == depl1.Name {
				foundDepl1 = true
			}

			if policy.GetNamespace() == depl2.Namespace && policy.GetName() == depl2.Name {
				foundDepl2 = true
			}
		}
		Expect(foundDepl1).To(BeTrue())
		Expect(foundDepl2).To(BeTrue())
	})
})
