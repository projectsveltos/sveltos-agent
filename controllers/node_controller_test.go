/*
Copyright 2022.

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

package controllers_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2/klogr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/projectsveltos/classifier-agent/controllers"
	"github.com/projectsveltos/classifier-agent/pkg/classification"
	libsveltosv1alpha1 "github.com/projectsveltos/libsveltos/api/v1alpha1"
)

var _ = Describe("Controllers: node controller", func() {
	var watcherCtx context.Context
	var cancel context.CancelFunc

	BeforeEach(func() {
		watcherCtx, cancel = context.WithCancel(context.Background())
	})

	AfterEach(func() {
		classifiers := &libsveltosv1alpha1.ClassifierList{}
		Expect(testEnv.List(context.TODO(), classifiers)).To(Succeed())

		for i := range classifiers.Items {
			Expect(testEnv.Delete(context.TODO(), &classifiers.Items[i]))
		}

		cancel()
	})

	It("findClassifierUsingKubernetesVersion returns classifiers using Kubernetes version", func() {
		classifier1 := getClassifierWithKubernetesConstraints()
		Expect(testEnv.Create(watcherCtx, classifier1)).To(Succeed())
		Expect(waitForObject(watcherCtx, testEnv.Client, classifier1)).To(Succeed())

		classifier2 := getClassifierWithResourceConstraints()
		Expect(testEnv.Create(watcherCtx, classifier2)).To(Succeed())
		Expect(waitForObject(watcherCtx, testEnv.Client, classifier2)).To(Succeed())

		reconciler := &controllers.NodeReconciler{
			Client: testEnv.Client,
			Scheme: scheme,
		}

		classifiers, err := controllers.FindClassifierUsingKubernetesVersion(reconciler, watcherCtx, klogr.New())
		Expect(err).To(BeNil())
		Expect(classifiers).ToNot(BeNil())
		Expect(classifiers).To(ContainElement(classifier1.Name))
		Expect(classifiers).ToNot(ContainElement(classifier2.Name))
	})

	It("findClassifierUsingKubernetesVersion returns classifiers using Kubernetes version", func() {
		node := getControlPlaneNode()
		Expect(testEnv.Create(watcherCtx, node)).To(Succeed())
		Expect(waitForObject(watcherCtx, testEnv.Client, node)).To(Succeed())

		// Get node and update status with version
		currentNode := corev1.Node{}
		Expect(testEnv.Get(watcherCtx, types.NamespacedName{Name: node.Name}, &currentNode)).To(Succeed())
		currentNode.Status = node.Status
		Expect(testEnv.Status().Update(watcherCtx, &currentNode)).To(Succeed())

		classification.InitializeManager(watcherCtx, klogr.New(), testEnv.Config, testEnv.Client,
			randomString(), randomString(), libsveltosv1alpha1.ClusterTypeCapi, nil, 10, false)

		reconciler := &controllers.NodeReconciler{
			Client: testEnv.Client,
			Scheme: scheme,
		}

		controllers.SetKubernetesVersion(reconciler, "v1.24.0")

		nodeName := client.ObjectKey{
			Name: node.Name,
		}
		_, err := reconciler.Reconcile(context.TODO(), ctrl.Request{
			NamespacedName: nodeName,
		})
		Expect(err).ToNot(HaveOccurred())

		Expect(controllers.GetKubernetesVersion(reconciler)).To(Equal(node.Status.NodeInfo.KubeletVersion))
	})
})
