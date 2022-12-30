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
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/klog/v2/klogr"

	"github.com/projectsveltos/classifier-agent/controllers"
	"github.com/projectsveltos/classifier-agent/pkg/classification"
	"github.com/projectsveltos/classifier-agent/pkg/scope"
	libsveltosv1alpha1 "github.com/projectsveltos/libsveltos/api/v1alpha1"
	libsveltosset "github.com/projectsveltos/libsveltos/lib/set"
)

var _ = Describe("Controllers: node controller", func() {
	var watcherCtx context.Context
	var cancel context.CancelFunc

	BeforeEach(func() {
		watcherCtx, cancel = context.WithCancel(context.Background())
		classification.InitializeManager(watcherCtx, klogr.New(), testEnv.Config, testEnv.Client,
			randomString(), randomString(), libsveltosv1alpha1.ClusterTypeCapi, nil, 10, false)
	})

	AfterEach(func() {
		classifiers := &libsveltosv1alpha1.ClassifierList{}
		Expect(testEnv.List(context.TODO(), classifiers)).To(Succeed())

		for i := range classifiers.Items {
			Expect(testEnv.Delete(context.TODO(), &classifiers.Items[i]))
		}

		cancel()
	})

	It("updateMaps updates map of Classifier using Kubernets verion as criteria", func() {
		classifier := getClassifierWithKubernetesConstraints()

		// Use Eventually so cache is in sync
		Eventually(func() bool {
			err := testEnv.Create(watcherCtx, classifier)
			if err != nil {
				Expect(meta.IsNoMatchError(err)).To(BeTrue())
				return false
			}
			return true
		}, timeout, pollingInterval).Should(BeTrue())

		Expect(waitForObject(watcherCtx, testEnv.Client, classifier)).To(Succeed())

		reconciler := &controllers.ClassifierReconciler{
			Client:             testEnv.Client,
			Scheme:             scheme,
			Mux:                sync.RWMutex{},
			GVKClassifiers:     make(map[schema.GroupVersionKind]*libsveltosset.Set),
			VersionClassifiers: libsveltosset.Set{},
		}

		controllers.UpdateMaps(reconciler, classifier)
		Expect(reconciler.VersionClassifiers.Len()).To(Equal(1))
		items := reconciler.VersionClassifiers.Items()
		Expect(items[0].Name).To(Equal(classifier.Name))
		Expect(len(reconciler.GVKClassifiers)).To(Equal(0))
	})

	It("updateMaps updates map of Classifier using DeployedResourceConstraints verion as criteria", func() {
		classifier := getClassifierWithResourceConstraints()
		Expect(len(classifier.Spec.DeployedResourceConstraints) > 0).To(BeTrue())
		Expect(testEnv.Create(watcherCtx, classifier)).To(Succeed())
		Expect(waitForObject(watcherCtx, testEnv.Client, classifier)).To(Succeed())

		reconciler := &controllers.ClassifierReconciler{
			Client:             testEnv.Client,
			Scheme:             scheme,
			Mux:                sync.RWMutex{},
			GVKClassifiers:     make(map[schema.GroupVersionKind]*libsveltosset.Set),
			VersionClassifiers: libsveltosset.Set{},
		}

		controllers.UpdateMaps(reconciler, classifier)
		Expect(reconciler.VersionClassifiers.Len()).To(Equal(0))
		Expect(len(reconciler.GVKClassifiers)).To(Equal(1))
		gvk := schema.GroupVersionKind{
			Group:   classifier.Spec.DeployedResourceConstraints[0].Group,
			Version: classifier.Spec.DeployedResourceConstraints[0].Version,
			Kind:    classifier.Spec.DeployedResourceConstraints[0].Kind,
		}
		v, ok := reconciler.GVKClassifiers[gvk]
		Expect(ok).To(BeTrue())
		Expect(v.Len()).To(Equal(1))
		items := reconciler.GVKClassifiers[gvk].Items()
		Expect(items[0].Name).To(Equal(classifier.Name))
	})

	It("reconcileDelete remove classifier from VersionClassifiers map", func() {
		classifier := getClassifierWithKubernetesConstraints()
		Expect(testEnv.Create(watcherCtx, classifier)).To(Succeed())
		Expect(waitForObject(watcherCtx, testEnv.Client, classifier)).To(Succeed())

		reconciler := &controllers.ClassifierReconciler{
			Client:             testEnv.Client,
			Scheme:             scheme,
			Mux:                sync.RWMutex{},
			GVKClassifiers:     make(map[schema.GroupVersionKind]*libsveltosset.Set),
			VersionClassifiers: libsveltosset.Set{},
		}

		policyRef := controllers.GetKeyFromObject(scheme, classifier)
		reconciler.VersionClassifiers.Insert(policyRef)

		classifierScope, err := scope.NewClassifierScope(scope.ClassifierScopeParams{
			Client:         testEnv.Client,
			Logger:         klogr.New(),
			Classifier:     classifier,
			ControllerName: "classifier",
		})
		Expect(err).To(BeNil())

		_, err = controllers.ReconcileDelete(reconciler, classifierScope, klogr.New())
		Expect(err).To(BeNil())
		Expect(reconciler.VersionClassifiers.Len()).To(Equal(0))
	})

	It("reconcileDelete remove classifier from GVKClassifiers map", func() {
		classifier := getClassifierWithResourceConstraints()
		Expect(len(classifier.Spec.DeployedResourceConstraints) > 0).To(BeTrue())
		Expect(testEnv.Create(watcherCtx, classifier)).To(Succeed())
		Expect(waitForObject(watcherCtx, testEnv.Client, classifier)).To(Succeed())

		gvk := schema.GroupVersionKind{
			Group:   classifier.Spec.DeployedResourceConstraints[0].Group,
			Version: classifier.Spec.DeployedResourceConstraints[0].Version,
			Kind:    classifier.Spec.DeployedResourceConstraints[0].Kind,
		}

		reconciler := &controllers.ClassifierReconciler{
			Client:             testEnv.Client,
			Scheme:             scheme,
			Mux:                sync.RWMutex{},
			GVKClassifiers:     make(map[schema.GroupVersionKind]*libsveltosset.Set),
			VersionClassifiers: libsveltosset.Set{},
		}

		policyRef := controllers.GetKeyFromObject(scheme, classifier)
		reconciler.GVKClassifiers[gvk] = &libsveltosset.Set{}
		reconciler.GVKClassifiers[gvk].Insert(policyRef)

		classifierScope, err := scope.NewClassifierScope(scope.ClassifierScopeParams{
			Client:         testEnv.Client,
			Logger:         klogr.New(),
			Classifier:     classifier,
			ControllerName: "classifier",
		})
		Expect(err).To(BeNil())

		_, err = controllers.ReconcileDelete(reconciler, classifierScope, klogr.New())
		Expect(err).To(BeNil())
		Expect(reconciler.VersionClassifiers.Len()).To(Equal(0))
		Expect(reconciler.GVKClassifiers[gvk].Len()).To(Equal(0))
	})
})
