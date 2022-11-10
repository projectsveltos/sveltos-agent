/*
Copyright 2022. projectsveltos.io. All rights reserved.

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
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	"github.com/projectsveltos/classifier-agent/pkg/utils"
	libsveltosv1alpha1 "github.com/projectsveltos/libsveltos/api/v1alpha1"
	libsveltosutils "github.com/projectsveltos/libsveltos/lib/utils"
)

// marking test serial as it requires pod to be restarted
var _ = Describe("Classification: crd", Serial, func() {
	var key string
	var value string
	const (
		namePrefix = "crd-"
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

	It("Classifier with CRD not present at time classifier is created", Label("FV"), func() {
		currentServiceMonitorCRD := &apiextensionsv1.CustomResourceDefinition{}
		err := k8sClient.Get(context.TODO(), types.NamespacedName{Name: "servicemonitors.monitoring.coreos.com"},
			currentServiceMonitorCRD)
		if err == nil {
			Expect(k8sClient.Delete(context.TODO(), currentServiceMonitorCRD))
		} else {
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		}

		minCount := 3
		maxCount := 5

		classifier := libsveltosv1alpha1.Classifier{
			ObjectMeta: metav1.ObjectMeta{
				Name: namePrefix + randomString(),
			},
			Spec: libsveltosv1alpha1.ClassifierSpec{
				ClassifierLabels: []libsveltosv1alpha1.ClassifierLabel{
					{Key: randomString(), Value: randomString()},
				},
				DeployedResourceConstraints: []libsveltosv1alpha1.DeployedResourceConstraint{
					{
						MinCount: &minCount,
						MaxCount: &maxCount,
						Group:    "monitoring.coreos.com",
						Version:  "v1",
						Kind:     "ServiceMonitor",
						LabelFilters: []libsveltosv1alpha1.LabelFilter{
							{Key: key, Value: value, Operation: libsveltosv1alpha1.OperationEqual},
						},
					},
				},
			},
		}

		By(fmt.Sprintf("Creating Classifier %s. Watches ServiceMonitor resources to classify", classifier.Name))
		Expect(k8sClient.Create(context.TODO(), &classifier)).To(Succeed())

		By("Verifying Cluster is currently not a match (ServiceMonitor CRD is not present in the cluster yet)")
		Eventually(func() bool {
			classifierReport := &libsveltosv1alpha1.ClassifierReport{}
			err = k8sClient.Get(context.TODO(),
				types.NamespacedName{Namespace: utils.ReportNamespace, Name: classifier.Name}, classifierReport)
			return err == nil && !classifierReport.Spec.Match
		}, timeout, pollingInterval).Should(BeTrue())

		By("Posting ServiceMonitor CRD")
		var serviceMonitor *unstructured.Unstructured
		serviceMonitor, err = libsveltosutils.GetUnstructured([]byte(serviceMonitorCRD))
		Expect(err).To(BeNil())
		Expect(k8sClient.Create(context.TODO(), serviceMonitor)).To(Succeed())

		Expect(k8sClient.Get(context.TODO(), types.NamespacedName{Name: "servicemonitors.monitoring.coreos.com"},
			currentServiceMonitorCRD)).To(Succeed())

		By("Creating enough ServiceMonitor for cluster to match Classifier")
		for i := 0; i < minCount; i++ {
			tmpNs := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: randomString(),
					Labels: map[string]string{
						key: value,
					},
				},
			}
			Expect(k8sClient.Create(context.TODO(), tmpNs)).To(Succeed())

			tmpServiceMonitorYAML := fmt.Sprintf(serviceMonitorInstance, tmpNs.Name, key, value)
			var tmpServiceMonitor *unstructured.Unstructured
			tmpServiceMonitor, err = libsveltosutils.GetUnstructured([]byte(tmpServiceMonitorYAML))
			Expect(err).To(BeNil())
			Expect(k8sClient.Create(context.TODO(), tmpServiceMonitor)).To(Succeed())
		}

		By("Verifying Cluster is a match (ServiceMonitor CRD is not present in the cluster yet)")
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
