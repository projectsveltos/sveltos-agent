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

package fv_test

import (
	"context"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	libsveltosv1beta1 "github.com/projectsveltos/libsveltos/api/v1beta1"
	"github.com/projectsveltos/libsveltos/lib/k8s_utils"
	"github.com/projectsveltos/sveltos-agent/pkg/utils"
)

const (
	luaPausedScript = `
function evaluate()
	local statuses = {}
	for _, resource in ipairs(resources) do
	  if resource.spec.paused == true then
        status = "Suspended"
        table.insert(statuses, {resource=resource, status = status})
	  else
        status = "Healthy"
        table.insert(statuses, {resource=resource, status = status})
	  end
	end
	
	local hs = {}
	if #statuses > 0 then
	  hs.resources = statuses 
	end
	return hs
end	
`

	nginxDeployment = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: %s
  namespace: default
  labels:
    app: nginx
spec:
  replicas: 3
  selector:
    matchLabels:
      app: nginx
  template:
    metadata:
      labels:
        app: nginx
    spec:
      serviceAccountName: %s
      containers:
      - name: nginx
        image: nginx:1.14.2
        ports:
        - containerPort: 80
`
)

var _ = Describe("Classification", func() {
	var key string
	var value string
	const (
		namePrefix = "healthcheck-"
	)

	BeforeEach(func() {
		key = randomString()
		value = randomString()
	})

	AfterEach(func() {
		listOptions := []client.ListOption{
			client.MatchingLabels{
				key: value,
			},
		}

		namespaceList := &corev1.NamespaceList{}
		Expect(k8sClient.List(context.TODO(), namespaceList, listOptions...)).To(Succeed())

		for i := range namespaceList.Items {
			Expect(k8sClient.Delete(context.TODO(), &namespaceList.Items[i])).To(Succeed())
		}
	})

	It("Evaluate healthCheck", Label("FV"), func() {
		By("Creating a nginx deployment")
		deploymentName := nginxPrefix + randomString()
		u, err := k8s_utils.GetUnstructured([]byte(fmt.Sprintf(nginxDeployment, deploymentName, "default")))
		Expect(err).To(BeNil())
		err = k8sClient.Create(context.TODO(), u)
		if err != nil {
			Expect(apierrors.IsAlreadyExists(err)).To(BeTrue())
		}

		healthCheck := libsveltosv1beta1.HealthCheck{
			ObjectMeta: metav1.ObjectMeta{
				Name: namePrefix + randomString(),
				Annotations: map[string]string{
					"projectsveltos.io/deployed-by-sveltos": "ok",
				},
			},
			Spec: libsveltosv1beta1.HealthCheckSpec{
				ResourceSelectors: []libsveltosv1beta1.ResourceSelector{
					{
						Group:   "apps",
						Version: "v1",
						Kind:    "Deployment",
					},
				},
				EvaluateHealth: luaPausedScript,
			},
		}

		By(fmt.Sprintf("Creating HealthCheck %s. Watches Paused Deployments", healthCheck.Name))
		Expect(k8sClient.Create(context.TODO(), &healthCheck)).To(Succeed())

		By("Verifying HealthCheckReport has match for nginx Deployment in Healthy state")
		verifyHealthCheckReport(u.GetNamespace(), u.GetName(), healthCheck.Name, libsveltosv1beta1.HealthStatusHealthy)

		By("Pause nginx namespace")
		currentDeployment := &appsv1.Deployment{}
		Expect(k8sClient.Get(context.TODO(), types.NamespacedName{Namespace: u.GetNamespace(), Name: u.GetName()},
			currentDeployment)).To(Succeed())
		currentDeployment.Spec.Paused = true
		Expect(k8sClient.Update(context.TODO(), currentDeployment)).To(Succeed())

		By("Verifying HealthCheckReport has match for nginx Deployment in Suspended state")
		verifyHealthCheckReport(u.GetNamespace(), u.GetName(), healthCheck.Name, libsveltosv1beta1.HealthStatusSuspended)

		By("UnPause nginx namespace")
		Expect(k8sClient.Get(context.TODO(), types.NamespacedName{Namespace: u.GetNamespace(), Name: u.GetName()},
			currentDeployment)).To(Succeed())
		currentDeployment.Spec.Paused = false
		Expect(k8sClient.Update(context.TODO(), currentDeployment)).To(Succeed())

		By("Verifying HealthCheckReport has match for nginx Deployment in Healthy state")
		verifyHealthCheckReport(u.GetNamespace(), u.GetName(), healthCheck.Name, libsveltosv1beta1.HealthStatusHealthy)

		By(fmt.Sprintf("Deleting HealthCheck %s", healthCheck.Name))
		Expect(k8sClient.Get(context.TODO(), types.NamespacedName{Name: healthCheck.Name}, &healthCheck))
		Expect(k8sClient.Delete(context.TODO(), &healthCheck)).To(Succeed())

		By("Verifying HealthCheckReport is marked for deletion")
		Eventually(func() bool {
			healthCheckReport := &libsveltosv1beta1.HealthCheckReport{}
			err := k8sClient.Get(context.TODO(),
				types.NamespacedName{Namespace: utils.ReportNamespace, Name: healthCheck.Name}, healthCheckReport)
			if err != nil {
				return apierrors.IsNotFound(err)
			}
			return !healthCheckReport.DeletionTimestamp.IsZero()
		}, timeout, pollingInterval).Should(BeTrue())

		By("Deleting nginx deployment")
		Expect(k8sClient.Get(context.TODO(), types.NamespacedName{Namespace: u.GetNamespace(), Name: u.GetName()},
			currentDeployment)).To(Succeed())
		Expect(k8sClient.Delete(context.TODO(), currentDeployment))
	})
})

func verifyHealthCheckReport(deplNamespace, deplName, healthCheckName string,
	healthStatus libsveltosv1beta1.HealthStatus) {

	Eventually(func() bool {
		healthCheckReport := &libsveltosv1beta1.HealthCheckReport{}
		err := k8sClient.Get(context.TODO(),
			types.NamespacedName{Namespace: utils.ReportNamespace, Name: healthCheckName}, healthCheckReport)
		if err != nil {
			return false
		}
		if healthCheckReport.Spec.ResourceStatuses == nil {
			return false
		}
		for i := range healthCheckReport.Spec.ResourceStatuses {
			s := healthCheckReport.Spec.ResourceStatuses[i]
			if s.HealthStatus == healthStatus &&
				s.ObjectRef.Namespace == deplNamespace &&
				s.ObjectRef.Name == deplName {

				return true
			}
		}
		return false
	}, timeout, pollingInterval).Should(BeTrue())
}
