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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
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
	deploymentReplicaCheck = `
function evaluate()
    local statuses = {}
    for _, resource in ipairs(resources) do
	  local status = "Progressing"
	  local message = ""
	  if resource.status ~= nil then
        if resource.status.availableReplicas ~= nil then
          if resource.status.availableReplicas == resource.spec.replicas then
            status="Healthy"
          end
          if resource.status.availableReplicas ~= resource.spec.replicas then
            message = "expected replicas: " .. resource.spec.replicas .. " available: " .. resource.status.availableReplicas
            status="Progressing"
          end
        end
	  end

	  table.insert(statuses, {resource=resource, status =  status, message = message})
	end

	local hs = {}
	if #statuses > 0 then
	  hs.resources = statuses 
	end
    return hs
end
`

	// Credit to argoCD. Using this to validate any script used in argoCD can be used here.
	clusterCheck = `
function getStatusBasedOnPhase(obj, hs)
    status = "Progressing"
    message = "Waiting for clusters"
    if obj.status ~= nil and obj.status.phase ~= nil then
        if obj.status.phase == "Provisioned" then
            status = "Healthy"
            message = "Cluster is running"
        end
        if obj.status.phase == "Failed" then
            status = "Degraded"
            message = ""
        end
    end
    return status, message
end

function evaluate()
	local statuses = {}

	for _, resource in ipairs(resources) do
	  if resource.spec.paused ~= nil and obj.spec.paused then
	    table.insert(statuses, {resource=resource, status="Suspended", message="Cluster is paused"})
      else
        status,message = getStatusBasedOnPhase(resource)
        table.insert(statuses, {resource=resource, status=status, message=message}) 
	  end 	
	end

	local hs = {}
	if #statuses > 0 then
	  hs.resources = statuses 
	end
	return hs
end	
`

	cluster_degraded = `apiVersion: cluster.x-k8s.io/v1alpha3
kind: Cluster
metadata:
  labels:
    app.kubernetes.io/managed-by: Helm
    app.kubernetes.io/version: 0.3.11
    argocd.argoproj.io/instance: test
    cluster.x-k8s.io/cluster-name: test
  name: test
  namespace: test
spec:
  clusterNetwork:
    pods:
      cidrBlocks:
        - 10.20.10.0/19
    services:
      cidrBlocks:
        - 10.10.10.0/19
  controlPlaneRef:
    apiVersion: controlplane.cluster.x-k8s.io/v1alpha3
    kind: KubeadmControlPlane
  infrastructureRef:
    apiVersion: infrastructure.cluster.x-k8s.io/v1alpha3
    kind: VSphereCluster
status:
  conditions:
    - lastTransitionTime: '2020-12-21T07:41:10Z'
      status: 'False'
      message: "Error message"
      type: Ready
    - lastTransitionTime: '2020-12-29T09:16:28Z'
      status: 'True'
      type: ControlPlaneReady
    - lastTransitionTime: '2020-11-24T09:15:24Z'
      status: 'True'
      type: InfrastructureReady
  controlPlaneInitialized: true
  controlPlaneReady: true
  infrastructureReady: true
  observedGeneration: 4
  phase: Failed`
)

var _ = Describe("Manager: healthcheck evaluation", func() {
	var healthCheck *libsveltosv1beta1.HealthCheck
	var clusterNamespace string
	var clusterName string
	var clusterType libsveltosv1beta1.ClusterType

	BeforeEach(func() {
		evaluation.Reset()
		clusterNamespace = utils.ReportNamespace
		clusterName = randomString()
		clusterType = libsveltosv1beta1.ClusterTypeCapi
	})

	AfterEach(func() {
		if healthCheck != nil {
			err := testEnv.Client.Delete(context.TODO(), healthCheck)
			if err != nil {
				Expect(apierrors.IsNotFound(err)).To(BeTrue())
			}
		}
	})

	It("GetResourceHealthStatus: evaluates Cluster as degraded", func() {
		evaluation.InitializeManagerWithSkip(context.TODO(), textlogger.NewLogger(textlogger.NewConfig(textlogger.Verbosity(1))),
			testEnv.Config, testEnv.Client, clusterNamespace, clusterName, clusterType, 10)

		manager := evaluation.GetManager()
		Expect(manager).ToNot(BeNil())

		cluster, err := k8s_utils.GetUnstructured([]byte(cluster_degraded))
		Expect(err).To(BeNil())

		var status *evaluation.HealthStatus
		status, err = evaluation.GetResourceHealthStatuses(manager, []*unstructured.Unstructured{cluster}, clusterCheck)
		Expect(err).To(BeNil())
		Expect(status).ToNot(BeNil())
		Expect(len(status.Resources)).To(Equal(1))
		Expect(status.Resources[0].Status).To(Equal(libsveltosv1beta1.HealthStatusDegraded))
	})

	It("GetResourceHealthStatus: evaluates Deployment in HealthStatusHealthy state", func() {
		var replicas int32 = 3
		var availableReplicas = 3
		depl := getDeployment(replicas, int32(availableReplicas))
		addTypeInformationToObject(scheme, depl)

		evaluation.InitializeManagerWithSkip(context.TODO(), textlogger.NewLogger(textlogger.NewConfig(textlogger.Verbosity(1))),
			testEnv.Config, testEnv.Client, clusterNamespace, clusterName, clusterType, 10)

		manager := evaluation.GetManager()
		Expect(manager).ToNot(BeNil())

		var content map[string]interface{}
		content, err := runtime.DefaultUnstructuredConverter.ToUnstructured(depl)
		Expect(err).To(BeNil())
		Expect(content).ToNot(BeNil())

		var u unstructured.Unstructured
		u.SetUnstructuredContent(content)

		var status *evaluation.HealthStatus
		status, err = evaluation.GetResourceHealthStatuses(manager, []*unstructured.Unstructured{&u}, deploymentReplicaCheck)
		Expect(err).To(BeNil())
		Expect(status).ToNot(BeNil())
		Expect(len(status.Resources)).To(Equal(1))
		Expect(status.Resources[0].Status).To(Equal(libsveltosv1beta1.HealthStatusHealthy))
	})

	It("GetResourceHealthStatus: evaluates Deployment in HealthStatusProgressing state", func() {
		var replicas int32 = 3
		var availableReplicas = 1
		depl := getDeployment(replicas, int32(availableReplicas))
		addTypeInformationToObject(scheme, depl)

		evaluation.InitializeManagerWithSkip(context.TODO(), textlogger.NewLogger(textlogger.NewConfig(textlogger.Verbosity(1))),
			testEnv.Config, testEnv.Client, clusterNamespace, clusterName, clusterType, 10)

		manager := evaluation.GetManager()
		Expect(manager).ToNot(BeNil())

		var content map[string]interface{}
		content, err := runtime.DefaultUnstructuredConverter.ToUnstructured(depl)
		Expect(err).To(BeNil())
		Expect(content).ToNot(BeNil())

		var u unstructured.Unstructured
		u.SetUnstructuredContent(content)

		var status *evaluation.HealthStatus
		status, err = evaluation.GetResourceHealthStatuses(manager, []*unstructured.Unstructured{&u}, deploymentReplicaCheck)
		Expect(err).To(BeNil())
		Expect(status).ToNot(BeNil())
		Expect(len(status.Resources)).To(Equal(1))
		Expect(status.Resources[0].Status).To(Equal(libsveltosv1beta1.HealthStatusProgressing))
		Expect(status.Resources[0].Message).To(Equal("expected replicas: 3 available: 1"))
	})

	It("fetchHealthCheckResources: fetches all resources mathing label selectors", func() {
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
			addTypeInformationToObject(scheme, service)
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
			addTypeInformationToObject(scheme, service)
		}

		By(fmt.Sprintf("Creating %d services in the correct namespace with wrong labels", matchingServices))
		for i := 0; i < matchingServices; i++ {
			service := getService()
			service.Namespace = namespace
			service.Labels = map[string]string{randomString(): randomString()}
			Expect(testEnv.Create(context.TODO(), service)).To(Succeed())
			waitForObject(context.TODO(), testEnv.Client, service)
			addTypeInformationToObject(scheme, service)
		}

		evaluation.InitializeManagerWithSkip(context.TODO(), textlogger.NewLogger(textlogger.NewConfig(textlogger.Verbosity(1))),
			testEnv.Config, testEnv.Client, clusterNamespace, clusterName, clusterType, 10)

		healthCheck = &libsveltosv1beta1.HealthCheck{
			ObjectMeta: metav1.ObjectMeta{
				Name: randomString(),
			},
			Spec: libsveltosv1beta1.HealthCheckSpec{
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
		deployments, err := evaluation.FetchHealthCheckResources(manager, context.TODO(), healthCheck)
		Expect(err).To(BeNil())
		Expect(len(deployments)).To(Equal(matchingServices))

		By("Deleting all created namespaces")
		for i := range namespaces {
			ns := &corev1.Namespace{}
			Expect(testEnv.Get(context.TODO(), types.NamespacedName{Name: namespaces[i]}, ns)).To(Succeed())
			Expect(testEnv.Delete(context.TODO(), ns)).To(Succeed())
		}
	})

	It("getHealthStatus: returns health status for matching resources", func() {
		namespace := randomString()
		createNamespace(namespace)

		By("Creating a deployment in healthy state")
		healthyDepl := getDeployment(1, 1)
		addTypeInformationToObject(scheme, healthyDepl)
		healthyDepl.Namespace = namespace
		Expect(testEnv.Create(context.TODO(), healthyDepl)).To(Succeed())
		waitForObject(context.TODO(), testEnv.Client, healthyDepl)

		healthyDepl.Status.AvailableReplicas = 1
		healthyDepl.Status.ReadyReplicas = 1
		healthyDepl.Status.Replicas = 1
		Expect(testEnv.Status().Update(context.TODO(), healthyDepl)).To(Succeed())

		By("Creating a deployment in progressing state")
		progressingDepl := getDeployment(2, 0)
		addTypeInformationToObject(scheme, progressingDepl)
		progressingDepl.Namespace = namespace
		Expect(testEnv.Create(context.TODO(), progressingDepl)).To(Succeed())
		waitForObject(context.TODO(), testEnv.Client, progressingDepl)

		By("Creating an HealthCheck matching the Deployments")
		healthCheck = &libsveltosv1beta1.HealthCheck{
			ObjectMeta: metav1.ObjectMeta{
				Name: randomString(),
			},
			Spec: libsveltosv1beta1.HealthCheckSpec{
				ResourceSelectors: []libsveltosv1beta1.ResourceSelector{
					{
						Group:     "apps",
						Version:   "v1",
						Kind:      "Deployment",
						Namespace: namespace,
					},
				},
				EvaluateHealth: deploymentReplicaCheck,
			},
		}

		evaluation.InitializeManagerWithSkip(context.TODO(), textlogger.NewLogger(textlogger.NewConfig(textlogger.Verbosity(1))),
			testEnv.Config, testEnv.Client, clusterNamespace, clusterName, clusterType, 10)

		manager := evaluation.GetManager()
		Expect(manager).ToNot(BeNil())

		result, err := evaluation.GetHealthStatus(manager, context.TODO(), healthCheck)
		Expect(err).To(BeNil())
		Expect(len(result)).To(Equal(2))
	})

	It("getHealthStatus: returns health status containing resources", func() {
		namespace := randomString()
		createNamespace(namespace)

		By("Creating a deployment in healthy state")
		healthyDepl := getDeployment(1, 1)
		addTypeInformationToObject(scheme, healthyDepl)
		healthyDepl.Namespace = namespace
		Expect(testEnv.Create(context.TODO(), healthyDepl)).To(Succeed())
		waitForObject(context.TODO(), testEnv.Client, healthyDepl)

		healthyDepl.Status.AvailableReplicas = 1
		healthyDepl.Status.ReadyReplicas = 1
		healthyDepl.Status.Replicas = 1
		Expect(testEnv.Status().Update(context.TODO(), healthyDepl)).To(Succeed())

		By("Creating a deployment in progressing state")
		progressingDepl := getDeployment(2, 0)
		addTypeInformationToObject(scheme, progressingDepl)
		progressingDepl.Namespace = namespace
		Expect(testEnv.Create(context.TODO(), progressingDepl)).To(Succeed())
		waitForObject(context.TODO(), testEnv.Client, progressingDepl)

		By("Creating an HealthCheck matching the Deployments")
		healthCheck = &libsveltosv1beta1.HealthCheck{
			ObjectMeta: metav1.ObjectMeta{
				Name: randomString(),
			},
			Spec: libsveltosv1beta1.HealthCheckSpec{
				ResourceSelectors: []libsveltosv1beta1.ResourceSelector{
					{
						Group:     "apps",
						Version:   "v1",
						Kind:      "Deployment",
						Namespace: namespace,
					},
				},
				EvaluateHealth:   deploymentReplicaCheck,
				CollectResources: true,
			},
		}

		evaluation.InitializeManagerWithSkip(context.TODO(), textlogger.NewLogger(textlogger.NewConfig(textlogger.Verbosity(1))),
			testEnv.Config, testEnv.Client, clusterNamespace, clusterName, clusterType, 10)

		manager := evaluation.GetManager()
		Expect(manager).ToNot(BeNil())

		statuses, err := evaluation.GetHealthStatus(manager, context.TODO(), healthCheck)
		Expect(err).To(BeNil())
		Expect(len(statuses)).To(Equal(2))
		for i := range statuses {
			Expect(statuses[i].Resource).ToNot(BeNil())
		}
	})

	It("createHealthCheckReport creates healthCheckReport", func() {
		evaluation.InitializeManagerWithSkip(context.TODO(), textlogger.NewLogger(textlogger.NewConfig(textlogger.Verbosity(1))),
			testEnv.Config, testEnv.Client, clusterNamespace, clusterName, clusterType, 10)

		manager := evaluation.GetManager()
		Expect(manager).ToNot(BeNil())

		namespace := randomString()

		healthCheck = &libsveltosv1beta1.HealthCheck{
			ObjectMeta: metav1.ObjectMeta{
				Name: randomString(),
			},
			Spec: libsveltosv1beta1.HealthCheckSpec{
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
		statuses := []libsveltosv1beta1.ResourceStatus{
			{ObjectRef: *deplRef, HealthStatus: libsveltosv1beta1.HealthStatusHealthy},
		}
		Expect(evaluation.CreateHealthCheckReport(manager, context.TODO(), healthCheck, statuses)).To(Succeed())

		Eventually(func() bool {
			healthCheckReport := &libsveltosv1beta1.HealthCheckReport{}
			err := testEnv.Get(context.TODO(), types.NamespacedName{Namespace: utils.ReportNamespace, Name: healthCheck.Name},
				healthCheckReport)
			if err != nil {
				return false
			}
			return healthCheckReport.Status.Phase != nil
		}, timeout, pollingInterval).Should(BeTrue())
	})

	It("createHealthCheckReport updates healthCheckReport", func() {
		evaluation.InitializeManagerWithSkip(context.TODO(), textlogger.NewLogger(textlogger.NewConfig(textlogger.Verbosity(1))),
			testEnv.Config, testEnv.Client, clusterNamespace, clusterName, clusterType, 10)

		manager := evaluation.GetManager()
		Expect(manager).ToNot(BeNil())

		namespace := randomString()

		healthCheck = &libsveltosv1beta1.HealthCheck{
			ObjectMeta: metav1.ObjectMeta{
				Name: randomString(),
			},
			Spec: libsveltosv1beta1.HealthCheckSpec{
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

		healthCheckReport := &libsveltosv1beta1.HealthCheckReport{
			ObjectMeta: metav1.ObjectMeta{
				Name:      healthCheck.Name,
				Namespace: utils.ReportNamespace,
			},
		}

		Expect(testEnv.Client.Create(context.TODO(), healthCheckReport)).To(Succeed())
		waitForObject(context.TODO(), testEnv.Client, healthCheckReport)

		deplRef := &corev1.ObjectReference{
			Namespace:  namespace,
			Name:       randomString(),
			APIVersion: "apps/v1",
			Kind:       "Deployment",
		}
		statuses := []libsveltosv1beta1.ResourceStatus{
			{ObjectRef: *deplRef, HealthStatus: libsveltosv1beta1.HealthStatusHealthy},
		}
		Expect(evaluation.CreateHealthCheckReport(manager, context.TODO(), healthCheck, statuses)).To(Succeed())

		Eventually(func() bool {
			healthCheckReport := &libsveltosv1beta1.HealthCheckReport{}
			err := testEnv.Get(context.TODO(), types.NamespacedName{Namespace: utils.ReportNamespace, Name: healthCheck.Name},
				healthCheckReport)
			if err != nil {
				return false
			}
			return healthCheckReport.Status.Phase != nil &&
				reflect.DeepEqual(healthCheckReport.Spec.ResourceStatuses, statuses)
		}, timeout, pollingInterval).Should(BeTrue())
	})

	It("sendHealthCheckReport sends HealthCheckReport to management cluster", func() {
		healthCheck = &libsveltosv1beta1.HealthCheck{
			ObjectMeta: metav1.ObjectMeta{
				Name: randomString(),
			},
			Spec: libsveltosv1beta1.HealthCheckSpec{
				ResourceSelectors: []libsveltosv1beta1.ResourceSelector{
					{
						Group:   "",
						Version: "v1",
						Kind:    "Namespace",
					},
				},
				// Not used by test
				EvaluateHealth: randomString(),
			},
		}
		Expect(testEnv.Create(context.TODO(), healthCheck)).To(Succeed())
		waitForObject(context.TODO(), testEnv.Client, healthCheck)

		phase := libsveltosv1beta1.ReportDelivering
		healthCheckReport := &libsveltosv1beta1.HealthCheckReport{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: utils.ReportNamespace,
				Name:      healthCheck.Name,
			},
			Spec: libsveltosv1beta1.HealthCheckReportSpec{
				HealthCheckName: healthCheck.Name,
			},
			Status: libsveltosv1beta1.HealthCheckReportStatus{
				Phase: &phase,
			},
		}

		Expect(testEnv.Create(context.TODO(), healthCheckReport)).To(Succeed())
		waitForObject(context.TODO(), testEnv.Client, healthCheckReport)

		evaluation.InitializeManagerWithSkip(context.TODO(), textlogger.NewLogger(textlogger.NewConfig(textlogger.Verbosity(1))),
			testEnv.Config, testEnv.Client, clusterNamespace, clusterName, clusterType, 10)
		manager := evaluation.GetManager()

		Expect(evaluation.SendHealthCheckReport(manager, context.TODO(), healthCheck)).To(Succeed())

		// SendHealthCheckReport creates healthCheckReport in manager.clusterNamespace

		// Use Eventually so cache is in sync
		healthCheckReportName := libsveltosv1beta1.GetHealthCheckReportName(healthCheck.Name, clusterName, &clusterType)
		Eventually(func() error {
			currentHealthCheckReport := &libsveltosv1beta1.HealthCheckReport{}
			return testEnv.Get(context.TODO(),
				types.NamespacedName{Namespace: clusterNamespace, Name: healthCheckReportName}, currentHealthCheckReport)
		}, timeout, pollingInterval).Should(BeNil())

		currentHealthCheckReport := &libsveltosv1beta1.HealthCheckReport{}
		Expect(testEnv.Get(context.TODO(),
			types.NamespacedName{Namespace: clusterNamespace, Name: healthCheckReportName}, currentHealthCheckReport)).To(Succeed())
		Expect(currentHealthCheckReport.Spec.ClusterName).To(Equal(clusterName))
		Expect(currentHealthCheckReport.Spec.ClusterNamespace).To(Equal(clusterNamespace))
		Expect(currentHealthCheckReport.Spec.HealthCheckName).To(Equal(healthCheck.Name))
		Expect(currentHealthCheckReport.Spec.ClusterType).To(Equal(clusterType))
		v, ok := currentHealthCheckReport.Labels[libsveltosv1beta1.HealthCheckReportClusterNameLabel]
		Expect(ok).To(BeTrue())
		Expect(v).To(Equal(clusterName))

		v, ok = currentHealthCheckReport.Labels[libsveltosv1beta1.HealthCheckReportClusterTypeLabel]
		Expect(ok).To(BeTrue())
		Expect(v).To(Equal(strings.ToLower(string(libsveltosv1beta1.ClusterTypeCapi))))

		v, ok = currentHealthCheckReport.Labels[libsveltosv1beta1.HealthCheckNameLabel]
		Expect(ok).To(BeTrue())
		Expect(v).To(Equal(healthCheck.Name))
	})

	It("cleanHealthCheckReport marks healthCheckReport as deleted", func() {
		phase := libsveltosv1beta1.ReportDelivering
		healthCheckReport := &libsveltosv1beta1.HealthCheckReport{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: utils.ReportNamespace,
				Name:      healthCheck.Name,
				Finalizers: []string{
					libsveltosv1beta1.HealthCheckReportFinalizer,
				},
			},
			Spec: libsveltosv1beta1.HealthCheckReportSpec{
				HealthCheckName: healthCheck.Name,
			},
			Status: libsveltosv1beta1.HealthCheckReportStatus{
				Phase: &phase,
			},
		}

		initObjects := []client.Object{
			healthCheckReport,
		}

		c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(initObjects...).WithObjects(initObjects...).Build()

		evaluation.InitializeManagerWithSkip(context.TODO(), textlogger.NewLogger(textlogger.NewConfig(textlogger.Verbosity(1))),
			nil, c, clusterNamespace, clusterName, clusterType, 10)

		manager := evaluation.GetManager()
		Expect(manager).ToNot(BeNil())

		Expect(evaluation.CleanHealthCheckReport(manager, context.TODO(), healthCheckReport.Name)).To(Succeed())

		err := c.Get(context.TODO(), types.NamespacedName{Name: healthCheckReport.Name, Namespace: utils.ReportNamespace},
			healthCheckReport)
		Expect(err).To(BeNil())
		Expect(healthCheckReport.DeletionTimestamp.IsZero()).To(BeFalse())

		phase = libsveltosv1beta1.ReportWaitingForDelivery
		Expect(healthCheckReport.Status.Phase).ToNot(BeNil())
		Expect(*healthCheckReport.Status.Phase).To(Equal(phase))
	})
})
