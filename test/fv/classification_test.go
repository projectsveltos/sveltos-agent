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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	libsveltosv1alpha1 "github.com/projectsveltos/libsveltos/api/v1alpha1"
	"github.com/projectsveltos/sveltos-agent/pkg/utils"
)

var _ = Describe("Classification", func() {
	var key string
	var value string
	const (
		namePrefix = "classify-"
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
		minCount := 3
		maxCount := 8

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
						Group:    "",
						Version:  "v1",
						Kind:     "Namespace",
						LabelFilters: []libsveltosv1alpha1.LabelFilter{
							{Key: key, Value: value, Operation: libsveltosv1alpha1.OperationEqual},
						},
					},
				},
			},
		}

		By(fmt.Sprintf("Creating Classifier %s. Watches Namespace resources to classify", classifier.Name))
		Expect(k8sClient.Create(context.TODO(), &classifier)).To(Succeed())

		By("Verifying Cluster is currently not a match")
		Eventually(func() bool {
			classifierReport := &libsveltosv1alpha1.ClassifierReport{}
			err := k8sClient.Get(context.TODO(),
				types.NamespacedName{Namespace: utils.ReportNamespace, Name: classifier.Name}, classifierReport)
			return err == nil && !classifierReport.Spec.Match
		}, timeout, pollingInterval).Should(BeTrue())

		By("Creating enough namespaces with proper labels for cluster to match Classifier")
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
		}

		By("Verifying Cluster is currently a match")
		Eventually(func() bool {
			classifierReport := &libsveltosv1alpha1.ClassifierReport{}
			err := k8sClient.Get(context.TODO(),
				types.NamespacedName{Namespace: utils.ReportNamespace, Name: classifier.Name}, classifierReport)
			return err == nil && classifierReport.Spec.Match
		}, timeout, pollingInterval).Should(BeTrue())

		By("Creating more namespaces with proper labels for cluster to not be a match Classifier")
		for i := 0; i < maxCount; i++ {
			tmpNs := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: randomString(),
					Labels: map[string]string{
						key: value,
					},
				},
			}
			Expect(k8sClient.Create(context.TODO(), tmpNs)).To(Succeed())
		}

		By("Verifying Cluster is currently not a match")
		Eventually(func() bool {
			classifierReport := &libsveltosv1alpha1.ClassifierReport{}
			err := k8sClient.Get(context.TODO(),
				types.NamespacedName{Namespace: utils.ReportNamespace, Name: classifier.Name}, classifierReport)
			return err == nil && !classifierReport.Spec.Match
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
