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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	libsveltosv1alpha1 "github.com/projectsveltos/libsveltos/api/v1alpha1"
	libsveltosutils "github.com/projectsveltos/libsveltos/lib/utils"
	"github.com/projectsveltos/sveltos-agent/pkg/utils"
)

const (
	luaPausedScript = `
function evaluate()
	hs = {}
	hs.status = "Healthy"
    hs.message = ""	
	if obj.spec.paused == true then
		hs.status = "Suspended"
	end
	return hs
end	
`

	nginxDeployment = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: nginx-deployment
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
		u, err := libsveltosutils.GetUnstructured([]byte(nginxDeployment))
		Expect(err).To(BeNil())
		Expect(k8sClient.Create(context.TODO(), u)).To(Succeed())

		healthCheck := libsveltosv1alpha1.HealthCheck{
			ObjectMeta: metav1.ObjectMeta{
				Name: namePrefix + randomString(),
				Finalizers: []string{
					libsveltosv1alpha1.HealthCheckFinalizer,
				},
			},
			Spec: libsveltosv1alpha1.HealthCheckSpec{
				Group:   "apps",
				Version: "v1",
				Kind:    "Deployment",
				Script:  luaPausedScript,
			},
		}

		By(fmt.Sprintf("Creating HealthCheck %s. Watches Paused Deployments", healthCheck.Name))
		Expect(k8sClient.Create(context.TODO(), &healthCheck)).To(Succeed())

		By("Verifying HealthCheckReport has match for nginx Deployment in Healthy state")
		verifyHealthCheckReport(u.GetNamespace(), u.GetName(), healthCheck.Name, libsveltosv1alpha1.HealthStatusHealthy)

		By("Pause nginx namespace")
		currentDeployment := &appsv1.Deployment{}
		Expect(k8sClient.Get(context.TODO(), types.NamespacedName{Namespace: u.GetNamespace(), Name: u.GetName()},
			currentDeployment)).To(Succeed())
		currentDeployment.Spec.Paused = true
		Expect(k8sClient.Update(context.TODO(), currentDeployment)).To(Succeed())

		By("Verifying HealthCheckReport has match for nginx Deployment in Suspended state")
		verifyHealthCheckReport(u.GetNamespace(), u.GetName(), healthCheck.Name, libsveltosv1alpha1.HealthStatusSuspended)

		By("UnPause nginx namespace")
		Expect(k8sClient.Get(context.TODO(), types.NamespacedName{Namespace: u.GetNamespace(), Name: u.GetName()},
			currentDeployment)).To(Succeed())
		currentDeployment.Spec.Paused = false
		Expect(k8sClient.Update(context.TODO(), currentDeployment)).To(Succeed())

		By("Verifying HealthCheckReport has match for nginx Deployment in Healthy state")
		verifyHealthCheckReport(u.GetNamespace(), u.GetName(), healthCheck.Name, libsveltosv1alpha1.HealthStatusHealthy)

		By(fmt.Sprintf("Deleting HealthCheck %s", healthCheck.Name))
		Expect(k8sClient.Get(context.TODO(), types.NamespacedName{Name: healthCheck.Name}, &healthCheck))
		Expect(k8sClient.Delete(context.TODO(), &healthCheck)).To(Succeed())

		By("Verifying HealthCheckReport is marked for deletion")
		Eventually(func() bool {
			healthCheckReport := &libsveltosv1alpha1.HealthCheckReport{}
			err := k8sClient.Get(context.TODO(),
				types.NamespacedName{Namespace: utils.ReportNamespace, Name: healthCheck.Name}, healthCheckReport)
			return err == nil && !healthCheckReport.DeletionTimestamp.IsZero()
		}, timeout, pollingInterval).Should(BeTrue())

		By("Deleting nginx deployment")
		Expect(k8sClient.Get(context.TODO(), types.NamespacedName{Namespace: u.GetNamespace(), Name: u.GetName()},
			currentDeployment)).To(Succeed())
		Expect(k8sClient.Delete(context.TODO(), currentDeployment))
	})
})

func verifyHealthCheckReport(deplNamespace, deplName, healthCheckName string,
	healthStatus libsveltosv1alpha1.HealthStatus) {

	Eventually(func() bool {
		healthCheckReport := &libsveltosv1alpha1.HealthCheckReport{}
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
