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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	libsveltosv1beta1 "github.com/projectsveltos/libsveltos/api/v1beta1"
	"github.com/projectsveltos/libsveltos/lib/k8s_utils"
	"github.com/projectsveltos/sveltos-agent/pkg/utils"
)

const (
	luaAvailableReplicasScript = `
function evaluate()
    hs = {}
	hs.matching = false
	hs.message = ""	
	if obj.status ~= nil then
	  if obj.status.unavailableReplicas ~= nil then
		  hs.matching = true
		  hs.message = "Available replicas does not match requested replicas"
      end
	end
	return hs
end
`
)

var _ = Describe("Events", func() {
	const (
		namePrefix = "event-"
	)

	It("Evaluate eventSource", Label("FV"), func() {
		By("Creating a nginx deployment")
		deploymentName := nginxPrefix + randomString()
		u, err := k8s_utils.GetUnstructured([]byte(fmt.Sprintf(nginxDeployment, deploymentName, randomString())))
		Expect(err).To(BeNil())
		err = k8sClient.Create(context.TODO(), u)
		if err != nil {
			Expect(apierrors.IsAlreadyExists(err)).To(BeTrue())
		}

		eventSource := libsveltosv1beta1.EventSource{
			ObjectMeta: metav1.ObjectMeta{
				Name: namePrefix + randomString(),
				Annotations: map[string]string{
					"projectsveltos.io/deployed-by-sveltos": "ok",
				},
			},
			Spec: libsveltosv1beta1.EventSourceSpec{
				ResourceSelectors: []libsveltosv1beta1.ResourceSelector{
					{
						Group:    "apps",
						Version:  "v1",
						Kind:     "Deployment",
						Evaluate: luaAvailableReplicasScript,
					},
				},
			},
		}

		By(fmt.Sprintf("Creating eventSource %s. Deployments with availableReplicas != replicas are match", eventSource.Name))
		Expect(k8sClient.Create(context.TODO(), &eventSource)).To(Succeed())

		By("Verifying EventReport has match for nginx Deployment")
		verifyEventReport("Deployment", u.GetNamespace(), u.GetName(), eventSource.Name)

		By(fmt.Sprintf("Deleting EventSource %s", eventSource.Name))
		Expect(k8sClient.Get(context.TODO(), types.NamespacedName{Name: eventSource.Name}, &eventSource))
		Expect(k8sClient.Delete(context.TODO(), &eventSource)).To(Succeed())

		By("Verifying EventReport is marked for deletion")
		Eventually(func() bool {
			eventReport := &libsveltosv1beta1.EventReport{}
			err := k8sClient.Get(context.TODO(),
				types.NamespacedName{Namespace: utils.ReportNamespace, Name: eventSource.Name}, eventReport)
			if err != nil {
				return apierrors.IsNotFound(err)
			}
			return !eventReport.DeletionTimestamp.IsZero()
		}, timeout, pollingInterval).Should(BeTrue())

		By("Deleting nginx deployment")
		currentDeployment := &appsv1.Deployment{}
		Expect(k8sClient.Get(context.TODO(), types.NamespacedName{Namespace: u.GetNamespace(), Name: u.GetName()},
			currentDeployment)).To(Succeed())
		Expect(k8sClient.Delete(context.TODO(), currentDeployment))
	})
})

func verifyEventReport(resourceKind, resourceNamespace, resourceName, eventSourceName string) {
	Eventually(func() bool {
		eventReport := &libsveltosv1beta1.EventReport{}
		err := k8sClient.Get(context.TODO(),
			types.NamespacedName{Namespace: utils.ReportNamespace, Name: eventSourceName}, eventReport)
		if err != nil {
			return false
		}
		if eventReport.Spec.MatchingResources == nil {
			return false
		}
		for i := range eventReport.Spec.MatchingResources {
			resource := eventReport.Spec.MatchingResources[i]
			if resource.Kind == resourceKind &&
				resource.Namespace == resourceNamespace &&
				resource.Name == resourceName {

				return true
			}
		}
		return false
	}, timeout, pollingInterval).Should(BeTrue())
}
