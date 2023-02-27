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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2/klogr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/projectsveltos/classifier-health-agent/pkg/evaluation"
	"github.com/projectsveltos/classifier-health-agent/pkg/utils"
	"github.com/projectsveltos/libsveltos/api/v1alpha1"
	libsveltosv1alpha1 "github.com/projectsveltos/libsveltos/api/v1alpha1"
	libsveltosutils "github.com/projectsveltos/libsveltos/lib/utils"
)

var (
	deploymentReplicaCheck = `
function evaluate()
	hs = {}
    hs.status = "Progressing"
    hs.message = ""
	if obj.status ~= nil then
        if obj.status.availableReplicas ~= nil then
			if obj.status.availableReplicas == obj.spec.replicas then
				hs.status = "Healthy"
			end
        	if obj.status.availableReplicas ~= obj.spec.replicas then
				hs.status = "Progressing"
            	hs.message = "expected replicas: " .. obj.spec.replicas .. " available: " .. obj.status.availableReplicas
          end
        end
	end
    return hs
end
`

	// Credit to argoCD. Using this to validate any script used in argoCD can be used here.
	clusterCheck = `
function getStatusBasedOnPhase(obj, hs)
    hs.status = "Progressing"
    hs.message = "Waiting for clusters"
    if obj.status ~= nil and obj.status.phase ~= nil then
        if obj.status.phase == "Provisioned" then
            hs.status = "Healthy"
            hs.message = "Cluster is running"
        end
        if obj.status.phase == "Failed" then
            hs.status = "Degraded"
            hs.message = ""
        end
    end
    return hs
end

function getReadyContitionStatus(obj, hs)
    if obj.status ~= nil and obj.status.conditions ~= nil then
        for i, condition in ipairs(obj.status.conditions) do
        if condition.type == "Ready" and condition.status == "False" then
            hs.status = "Degraded"
            hs.message = condition.message
            return hs
        end
        end
    end
    return hs
end

function evaluate()
	hs = {}
	if obj.spec.paused ~= nil and obj.spec.paused then
    	hs.status = "Suspended"
    	hs.message = "Cluster is paused"
    	return hs
	end

	getStatusBasedOnPhase(obj, hs)
	getReadyContitionStatus(obj, hs)

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
	var healthCheck *libsveltosv1alpha1.HealthCheck

	BeforeEach(func() {
		evaluation.Reset()
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
		evaluation.InitializeManagerWithSkip(context.TODO(), klogr.New(), testEnv.Config, testEnv.Client, 10)

		manager := evaluation.GetManager()
		Expect(manager).ToNot(BeNil())

		cluster, err := libsveltosutils.GetUnstructured([]byte(cluster_degraded))
		Expect(err).To(BeNil())

		var status *v1alpha1.ResourceStatus
		status, err = evaluation.GetResourceHealthStatus(manager, cluster, clusterCheck)
		Expect(err).To(BeNil())
		Expect(status).ToNot(BeNil())
		Expect(status.HealthStatus).To(Equal(libsveltosv1alpha1.HealthStatusDegraded))
	})

	It("GetResourceHealthStatus: evaluates Deployment in HealthStatusHealthy state", func() {
		var replicas int32 = 3
		var availableReplicas = 3
		depl := getDeployment(replicas, int32(availableReplicas))

		evaluation.InitializeManagerWithSkip(context.TODO(), klogr.New(), testEnv.Config, testEnv.Client, 10)

		manager := evaluation.GetManager()
		Expect(manager).ToNot(BeNil())

		var content map[string]interface{}
		content, err := runtime.DefaultUnstructuredConverter.ToUnstructured(depl)
		Expect(err).To(BeNil())
		Expect(content).ToNot(BeNil())

		var u unstructured.Unstructured
		u.SetUnstructuredContent(content)

		var status *v1alpha1.ResourceStatus
		status, err = evaluation.GetResourceHealthStatus(manager, &u, deploymentReplicaCheck)
		Expect(err).To(BeNil())
		Expect(status).ToNot(BeNil())
		Expect(status.HealthStatus).To(Equal(libsveltosv1alpha1.HealthStatusHealthy))
	})

	It("GetResourceHealthStatus: evaluates Deployment in HealthStatusProgressing state", func() {
		var replicas int32 = 3
		var availableReplicas = 1
		depl := getDeployment(replicas, int32(availableReplicas))

		evaluation.InitializeManagerWithSkip(context.TODO(), klogr.New(), testEnv.Config, testEnv.Client, 10)

		manager := evaluation.GetManager()
		Expect(manager).ToNot(BeNil())

		var content map[string]interface{}
		content, err := runtime.DefaultUnstructuredConverter.ToUnstructured(depl)
		Expect(err).To(BeNil())
		Expect(content).ToNot(BeNil())

		var u unstructured.Unstructured
		u.SetUnstructuredContent(content)

		var status *v1alpha1.ResourceStatus
		status, err = evaluation.GetResourceHealthStatus(manager, &u, deploymentReplicaCheck)
		Expect(err).To(BeNil())
		Expect(status).ToNot(BeNil())
		Expect(status.HealthStatus).To(Equal(libsveltosv1alpha1.HealthStatusProgressing))
		Expect(status.Message).To(Equal("expected replicas: 3 available: 1"))
	})

	It("fetchHealthCheckResources: fetches all resources mathing label selectors", func() {
		namespaces := make([]string, 0)

		namespace := randomString()
		labels := map[string]string{
			randomString(): randomString(),
			randomString(): randomString(),
		}
		labelFilters := make([]libsveltosv1alpha1.LabelFilter, 0)
		for k := range labels {
			labelFilters = append(labelFilters, libsveltosv1alpha1.LabelFilter{
				Key: k, Operation: libsveltosv1alpha1.OperationEqual, Value: labels[k],
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
			Expect(waitForObject(context.TODO(), testEnv.Client, service)).To(Succeed())
		}

		evaluation.InitializeManagerWithSkip(context.TODO(), klogr.New(), testEnv.Config, testEnv.Client, 10)

		healthCheck = &libsveltosv1alpha1.HealthCheck{
			ObjectMeta: metav1.ObjectMeta{
				Name: randomString(),
			},
			Spec: libsveltosv1alpha1.HealthCheckSpec{
				Group:        "",
				Version:      "v1",
				Kind:         "Service",
				Namespace:    namespace,
				LabelFilters: labelFilters,
			},
		}

		manager := evaluation.GetManager()
		Expect(manager).ToNot(BeNil())
		deployments, err := evaluation.FetchHealthCheckResources(manager, context.TODO(), healthCheck)
		Expect(err).To(BeNil())
		Expect(len(deployments.Items)).To(Equal(matchingServices))

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
		healthyDepl.Namespace = namespace
		Expect(testEnv.Create(context.TODO(), healthyDepl)).To(Succeed())
		Expect(waitForObject(context.TODO(), testEnv.Client, healthyDepl)).To(Succeed())

		healthyDepl.Status.AvailableReplicas = 1
		healthyDepl.Status.ReadyReplicas = 1
		healthyDepl.Status.Replicas = 1
		Expect(testEnv.Status().Update(context.TODO(), healthyDepl)).To(Succeed())

		By("Creating a deployment in progressing state")
		progressingDepl := getDeployment(2, 0)
		progressingDepl.Namespace = namespace
		Expect(testEnv.Create(context.TODO(), progressingDepl)).To(Succeed())
		Expect(waitForObject(context.TODO(), testEnv.Client, progressingDepl)).To(Succeed())

		By("Creating ConfigMap with lua script")
		configMap := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				// namespace is supposed to be in projectsveltos, but for testing does not matter
				Namespace: namespace,
				Name:      randomString(),
			},
			Data: map[string]string{
				"lua": deploymentReplicaCheck,
			},
		}
		Expect(testEnv.Create(context.TODO(), configMap)).To(Succeed())
		Expect(waitForObject(context.TODO(), testEnv.Client, configMap)).To(Succeed())

		By("Creating an HealthCheck matching the Deployments and refercing ConfigMap")
		healthCheck = &libsveltosv1alpha1.HealthCheck{
			ObjectMeta: metav1.ObjectMeta{
				Name: randomString(),
			},
			Spec: libsveltosv1alpha1.HealthCheckSpec{
				Group:     "apps",
				Version:   "v1",
				Kind:      "Deployment",
				Namespace: namespace,
				PolicyRef: &libsveltosv1alpha1.PolicyRef{
					Kind:      string(libsveltosv1alpha1.ConfigMapReferencedResourceKind),
					Namespace: configMap.Namespace,
					Name:      configMap.Name,
				},
			},
		}

		evaluation.InitializeManagerWithSkip(context.TODO(), klogr.New(), testEnv.Config, testEnv.Client, 10)

		manager := evaluation.GetManager()
		Expect(manager).ToNot(BeNil())

		statuses, err := evaluation.GetHealthStatus(manager, context.TODO(), healthCheck)
		Expect(err).To(BeNil())
		Expect(len(statuses)).To(Equal(2))
	})

	It("createHealthCheckReport creates healthCheckReport", func() {
		evaluation.InitializeManagerWithSkip(context.TODO(), klogr.New(), testEnv.Config, testEnv.Client, 10)

		manager := evaluation.GetManager()
		Expect(manager).ToNot(BeNil())

		namespace := randomString()

		healthCheck = &libsveltosv1alpha1.HealthCheck{
			ObjectMeta: metav1.ObjectMeta{
				Name: randomString(),
			},
			Spec: libsveltosv1alpha1.HealthCheckSpec{
				Group:     "apps",
				Version:   "v1",
				Kind:      "Deployment",
				Namespace: namespace,
			},
		}

		deplRef := &corev1.ObjectReference{
			Namespace:  namespace,
			Name:       randomString(),
			APIVersion: "apps/v1",
			Kind:       "Deployment",
		}
		statuses := []libsveltosv1alpha1.ResourceStatus{
			{ObjectRef: *deplRef, HealthStatus: libsveltosv1alpha1.HealthStatusHealthy},
		}
		Expect(evaluation.CreateHealthCheckReport(manager, context.TODO(), healthCheck, statuses)).To(Succeed())

		Eventually(func() bool {
			healthCheckReport := &libsveltosv1alpha1.HealthCheckReport{}
			err := testEnv.Get(context.TODO(), types.NamespacedName{Namespace: utils.ReportNamespace, Name: healthCheck.Name},
				healthCheckReport)
			if err != nil {
				return false
			}
			return healthCheckReport.Status.Phase != nil
		}, timeout, pollingInterval).Should(BeTrue())
	})

	It("createHealthCheckReport updates healthCheckReport", func() {
		evaluation.InitializeManagerWithSkip(context.TODO(), klogr.New(), testEnv.Config, testEnv.Client, 10)

		manager := evaluation.GetManager()
		Expect(manager).ToNot(BeNil())

		namespace := randomString()

		healthCheck = &libsveltosv1alpha1.HealthCheck{
			ObjectMeta: metav1.ObjectMeta{
				Name: randomString(),
			},
			Spec: libsveltosv1alpha1.HealthCheckSpec{
				Group:     "apps",
				Version:   "v1",
				Kind:      "Deployment",
				Namespace: namespace,
			},
		}

		healthCheckReport := &libsveltosv1alpha1.HealthCheckReport{
			ObjectMeta: metav1.ObjectMeta{
				Name:      healthCheck.Name,
				Namespace: utils.ReportNamespace,
			},
		}

		Expect(testEnv.Client.Create(context.TODO(), healthCheckReport)).To(Succeed())
		Expect(waitForObject(context.TODO(), testEnv.Client, healthCheckReport)).To(Succeed())

		deplRef := &corev1.ObjectReference{
			Namespace:  namespace,
			Name:       randomString(),
			APIVersion: "apps/v1",
			Kind:       "Deployment",
		}
		statuses := []libsveltosv1alpha1.ResourceStatus{
			{ObjectRef: *deplRef, HealthStatus: libsveltosv1alpha1.HealthStatusHealthy},
		}
		Expect(evaluation.CreateHealthCheckReport(manager, context.TODO(), healthCheck, statuses)).To(Succeed())

		Eventually(func() bool {
			healthCheckReport := &libsveltosv1alpha1.HealthCheckReport{}
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
		healthCheck = &libsveltosv1alpha1.HealthCheck{
			ObjectMeta: metav1.ObjectMeta{
				Name: randomString(),
			},
			Spec: libsveltosv1alpha1.HealthCheckSpec{
				Group:   "",
				Version: "v1",
				Kind:    "Namespace",
			},
		}
		Expect(testEnv.Create(context.TODO(), healthCheck)).To(Succeed())
		Expect(waitForObject(context.TODO(), testEnv.Client, healthCheck)).To(Succeed())

		phase := libsveltosv1alpha1.ReportDelivering
		healthCheckReport := &libsveltosv1alpha1.HealthCheckReport{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: utils.ReportNamespace,
				Name:      healthCheck.Name,
			},
			Spec: libsveltosv1alpha1.HealthCheckReportSpec{
				HealthCheckName: healthCheck.Name,
			},
			Status: libsveltosv1alpha1.HealthCheckReportStatus{
				Phase: &phase,
			},
		}

		Expect(testEnv.Create(context.TODO(), healthCheckReport)).To(Succeed())
		Expect(waitForObject(context.TODO(), testEnv.Client, healthCheckReport)).To(Succeed())

		clusterNamespace := utils.ReportNamespace
		clusterName := randomString()
		clusterType := libsveltosv1alpha1.ClusterTypeCapi
		watcherCtx, cancel := context.WithCancel(context.Background())
		defer cancel()
		evaluation.InitializeManager(watcherCtx, klogr.New(), testEnv.Config, testEnv.Client,
			clusterNamespace, clusterName, clusterType, 10, false)
		manager := evaluation.GetManager()

		Expect(evaluation.SendHealthCheckReport(manager, context.TODO(), healthCheck)).To(Succeed())

		// SendHealthCheckReport creates healthCheckReport in manager.clusterNamespace

		// Use Eventually so cache is in sync
		healthCheckReportName := libsveltosv1alpha1.GetHealthCheckReportName(healthCheck.Name, clusterName, &clusterType)
		Eventually(func() error {
			currentHealthCheckReport := &libsveltosv1alpha1.HealthCheckReport{}
			return testEnv.Get(context.TODO(),
				types.NamespacedName{Namespace: clusterNamespace, Name: healthCheckReportName}, currentHealthCheckReport)
		}, timeout, pollingInterval).Should(BeNil())

		currentHealthCheckReport := &libsveltosv1alpha1.HealthCheckReport{}
		Expect(testEnv.Get(context.TODO(),
			types.NamespacedName{Namespace: clusterNamespace, Name: healthCheckReportName}, currentHealthCheckReport)).To(Succeed())
		Expect(currentHealthCheckReport.Spec.ClusterName).To(Equal(clusterName))
		Expect(currentHealthCheckReport.Spec.ClusterNamespace).To(Equal(clusterNamespace))
		Expect(currentHealthCheckReport.Spec.HealthCheckName).To(Equal(healthCheck.Name))
		Expect(currentHealthCheckReport.Spec.ClusterType).To(Equal(clusterType))
		v, ok := currentHealthCheckReport.Labels[libsveltosv1alpha1.HealthCheckReportClusterNameLabel]
		Expect(ok).To(BeTrue())
		Expect(v).To(Equal(clusterName))

		v, ok = currentHealthCheckReport.Labels[libsveltosv1alpha1.HealthCheckReportClusterTypeLabel]
		Expect(ok).To(BeTrue())
		Expect(v).To(Equal(strings.ToLower(string(libsveltosv1alpha1.ClusterTypeCapi))))

		v, ok = currentHealthCheckReport.Labels[libsveltosv1alpha1.HealthCheckLabelName]
		Expect(ok).To(BeTrue())
		Expect(v).To(Equal(healthCheck.Name))
	})

	It("cleanHealthCheckReport marks healthCheckReport as deleted", func() {
		phase := libsveltosv1alpha1.ReportDelivering
		healthCheckReport := &libsveltosv1alpha1.HealthCheckReport{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: utils.ReportNamespace,
				Name:      healthCheck.Name,
				Finalizers: []string{
					libsveltosv1alpha1.HealthCheckReportFinalizer,
				},
			},
			Spec: libsveltosv1alpha1.HealthCheckReportSpec{
				HealthCheckName: healthCheck.Name,
			},
			Status: libsveltosv1alpha1.HealthCheckReportStatus{
				Phase: &phase,
			},
		}

		initObjects := []client.Object{
			healthCheckReport,
		}

		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(initObjects...).Build()

		evaluation.InitializeManagerWithSkip(context.TODO(), klogr.New(), nil, c, 10)

		manager := evaluation.GetManager()
		Expect(manager).ToNot(BeNil())

		Expect(evaluation.CleanHealthCheckReport(manager, context.TODO(), healthCheckReport.Name)).To(Succeed())

		err := c.Get(context.TODO(), types.NamespacedName{Name: healthCheckReport.Name, Namespace: utils.ReportNamespace},
			healthCheckReport)
		Expect(err).To(BeNil())
		Expect(healthCheckReport.DeletionTimestamp.IsZero()).To(BeFalse())

		phase = libsveltosv1alpha1.ReportWaitingForDelivery
		Expect(healthCheckReport.Status.Phase).ToNot(BeNil())
		Expect(*healthCheckReport.Status.Phase).To(Equal(phase))
	})
})

func getDeployment(replicas, availableReplicas int32) *appsv1.Deployment {
	labels := map[string]string{randomString(): randomString()}
	labelSelector := metav1.LabelSelector{
		MatchLabels: labels,
	}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: randomString(),
			Name:      randomString(),
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &labelSelector,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  randomString(),
							Image: randomString(),
						},
					},
				},
			},
		},
		Status: appsv1.DeploymentStatus{
			AvailableReplicas: availableReplicas,
		},
	}
}

func getService() *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: randomString(),
			Name:      randomString(),
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{
				{
					Name: randomString(),
					Port: 443,
				},
			},
		},
	}
}

func createNamespace(name string) {
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
	}

	Expect(testEnv.Create(context.TODO(), ns)).To(Succeed())
	Expect(waitForObject(context.TODO(), testEnv.Client, ns)).To(Succeed())
}
