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

	libsveltosv1alpha1 "github.com/projectsveltos/libsveltos/api/v1alpha1"
	"github.com/projectsveltos/sveltos-agent/pkg/utils"
)

var (
	aggregatedClassification = `      function table_equal(t1, t2)
	local metatable = {}
	metatable.__eq = function(t1, t2)
	  if type(t1) ~= "table" or type(t2) ~= "table" then
		return false
	  end

	  local keys = {}
	  for k in pairs(t1) do
		keys[k] = true
	  end

	  for k in pairs(t2) do
		if not keys[k] then
		  return false
		end
	  end

	  for k, v in pairs(t1) do
		if t2[k] ~= v then
		  return false
		end
	  end

	  return true
	end

	setmetatable(t1, metatable)
	setmetatable(t2, metatable)

	return t1 == t2
  end

  function evaluate()
	local hs = {}
	hs.message = ""
	hs.matching = false

	local deployments = {}
	local pods = {}
	local services = {}
	local orphanedDeployments = {}

	-- Separate deployments, pods, and services from the resources
	for _, resource in ipairs(resources) do
	  local kind = resource.kind
	  if kind == "Deployment" then
		table.insert(deployments, resource)
	  elseif kind == "Pod" then
		table.insert(pods, resource)
	  elseif kind == "Service" then
		table.insert(services, resource)
	  end
	end

	-- Identify deployments that have no pods or services associated with them
	for _, deployment in ipairs(deployments) do
	  local deploymentName = deployment.metadata.name
	  local hasPod = false
	  local hasService = false

	  for _, pod in ipairs(pods) do
		if pod.metadata.namespace == deployment.metadata.namespace then
		  for _, owner in ipairs(pod.metadata.ownerReferences) do
			if owner.name == deploymentName then
			  hasPod = true
			  break
			end  
		  end
		end 
	  end

	  for _, service in ipairs(services) do
		if service.metadata.namespace == deployment.metadata.namespace then
		  if table_equal(service.spec.selector, deployment.metadata.labels) then
			hasService = true
			break
		  end
		end
	  end

	  if not hasPod and not hasService then
		table.insert(orphanedDeployments, deployment)
		break
	  end
	end

	if #orphanedDeployments > 0 then
	  hs.matching = true
	end
	return hs
  end`
)

var _ = Describe("Classification", func() {
	var key string
	var value string
	const (
		namePrefix = "aggregated-classify-"
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

	It("React to classifier and deployed resources", Label("FV"), func() {
		classifier := libsveltosv1alpha1.Classifier{
			ObjectMeta: metav1.ObjectMeta{
				Name: namePrefix + randomString(),
				Annotations: map[string]string{
					"projectsveltos.io/deployed-by-sveltos": "ok",
				},
			},
			Spec: libsveltosv1alpha1.ClassifierSpec{
				ClassifierLabels: []libsveltosv1alpha1.ClassifierLabel{
					{Key: randomString(), Value: randomString()},
				},
				DeployedResourceConstraint: &libsveltosv1alpha1.DeployedResourceConstraint{
					ResourceSelectors: []libsveltosv1alpha1.ResourceSelector{
						{
							Group:   "apps",
							Version: "v1",
							Kind:    "Deployment",
							LabelFilters: []libsveltosv1alpha1.LabelFilter{
								{Key: key, Value: value, Operation: libsveltosv1alpha1.OperationEqual},
							},
						},
						{
							Group:   "",
							Version: "v1",
							Kind:    "Service",
							LabelFilters: []libsveltosv1alpha1.LabelFilter{
								{Key: key, Value: value, Operation: libsveltosv1alpha1.OperationEqual},
							},
						},
						{
							Group:   "",
							Version: "v1",
							Kind:    "Pod",
							LabelFilters: []libsveltosv1alpha1.LabelFilter{
								{Key: key, Value: value, Operation: libsveltosv1alpha1.OperationEqual},
							},
						},
					},
					AggregatedClassification: aggregatedClassification,
				},
			},
		}

		By(fmt.Sprintf("Creating Classifier %s. Watches Deployments, Pods and Service resources to classify",
			classifier.Name))
		Expect(k8sClient.Create(context.TODO(), &classifier)).To(Succeed())

		By("Verifying Cluster is currently not a match")
		Eventually(func() bool {
			classifierReport := &libsveltosv1alpha1.ClassifierReport{}
			err := k8sClient.Get(context.TODO(),
				types.NamespacedName{Namespace: utils.ReportNamespace, Name: classifier.Name}, classifierReport)
			return err == nil && !classifierReport.Spec.Match
		}, timeout, pollingInterval).Should(BeTrue())

		nsName := randomString()
		By(fmt.Sprintf("Creating namespace %s", nsName))
		tmpNs := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: nsName,
				Labels: map[string]string{
					key: value,
				},
			},
		}
		Expect(k8sClient.Create(context.TODO(), tmpNs)).To(Succeed())

		deploymentName := randomString()
		By(fmt.Sprintf("Creating deployment %s/%s (no service matches it and replica is 0)",
			nsName, deploymentName))
		replicas := int32(0)
		deployment := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      deploymentName,
				Namespace: nsName,
				Labels: map[string]string{
					key: value,
				},
			},
			Spec: appsv1.DeploymentSpec{
				Replicas: &replicas,
				Selector: &metav1.LabelSelector{
					MatchLabels: map[string]string{
						key: value,
					},
				},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							key: value,
						},
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
		}
		Expect(k8sClient.Create(context.TODO(), deployment)).To(Succeed())

		By("Verifying Cluster is currently a match")
		Eventually(func() bool {
			classifierReport := &libsveltosv1alpha1.ClassifierReport{}
			err := k8sClient.Get(context.TODO(),
				types.NamespacedName{Namespace: utils.ReportNamespace, Name: classifier.Name}, classifierReport)
			return err == nil && classifierReport.Spec.Match
		}, timeout, pollingInterval).Should(BeTrue())

		By(fmt.Sprintf("Deleting Classifier %s", classifier.Name))
		currentClassifier := &libsveltosv1alpha1.Classifier{}
		Expect(k8sClient.Get(context.TODO(), types.NamespacedName{Name: classifier.Name}, currentClassifier)).To(Succeed())
		Expect(k8sClient.Delete(context.TODO(), currentClassifier)).To(Succeed())

		By("Verifying ClassifierReport is gone")
		Eventually(func() bool {
			classifierReport := &libsveltosv1alpha1.ClassifierReport{}
			err := k8sClient.Get(context.TODO(),
				types.NamespacedName{Namespace: utils.ReportNamespace, Name: classifier.Name}, classifierReport)
			return err != nil && apierrors.IsNotFound(err)
		}, timeout, pollingInterval).Should(BeTrue())
	})
})
