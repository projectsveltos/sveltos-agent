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

package controllers_test

import (
	"context"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/klog/v2/klogr"

	libsveltosv1alpha1 "github.com/projectsveltos/libsveltos/api/v1alpha1"
	libsveltosset "github.com/projectsveltos/libsveltos/lib/set"
	"github.com/projectsveltos/sveltos-agent/controllers"
	"github.com/projectsveltos/sveltos-agent/pkg/evaluation"
	"github.com/projectsveltos/sveltos-agent/pkg/scope"
)

var _ = Describe("Controllers: reloader controller", func() {
	var watcherCtx context.Context
	var cancel context.CancelFunc

	BeforeEach(func() {
		watcherCtx, cancel = context.WithCancel(context.Background())
		evaluation.InitializeManager(watcherCtx, klogr.New(), testEnv.Config, testEnv.Client,
			randomString(), randomString(), libsveltosv1alpha1.ClusterTypeCapi, 10, false)
	})

	AfterEach(func() {
		reloaders := &libsveltosv1alpha1.ReloaderList{}
		Expect(testEnv.List(context.TODO(), reloaders)).To(Succeed())

		for i := range reloaders.Items {
			Expect(testEnv.Delete(context.TODO(), &reloaders.Items[i]))
		}

		cancel()
	})

	It("updateMaps updates map of Reloader using DeployedResourceConstraints verion as criteria", func() {
		reloader := getReloader()

		reloader.Spec = libsveltosv1alpha1.ReloaderSpec{
			ReloaderInfo: []libsveltosv1alpha1.ReloaderInfo{
				{
					Namespace: randomString(),
					Name:      randomString(),
					Kind:      "Deployment",
				},
				{
					Namespace: randomString(),
					Name:      randomString(),
					Kind:      "StatefulSet",
				},
				{
					Namespace: randomString(),
					Name:      randomString(),
					Kind:      "DaemonSet",
				},
			},
		}

		Expect(testEnv.Create(watcherCtx, reloader)).To(Succeed())
		Expect(waitForObject(watcherCtx, testEnv.Client, reloader)).To(Succeed())

		reconciler := &controllers.ReloaderReconciler{
			Client:       testEnv.Client,
			Scheme:       scheme,
			Mux:          sync.RWMutex{},
			GVKReloaders: make(map[schema.GroupVersionKind]*libsveltosset.Set),
		}

		controllers.ReloaderUpdateMaps(reconciler, reloader)

		for i := range reloader.Spec.ReloaderInfo {
			gvk := schema.GroupVersionKind{
				// Deployment/StatefulSet/DaemonSet are all appsv1
				Group:   appsv1.SchemeGroupVersion.Group,
				Version: appsv1.SchemeGroupVersion.Version,
				Kind:    reloader.Spec.ReloaderInfo[i].Kind,
			}

			v, ok := reconciler.GVKReloaders[gvk]
			Expect(ok).To(BeTrue())
			Expect(v.Len()).To(Equal(1))
			items := reconciler.GVKReloaders[gvk].Items()
			Expect(items[0].Name).To(Equal(reloader.Name))
		}
	})

	It("reconcileDelete remove reloader from GVKReloaders map", func() {
		reloader := getReloader()
		Expect(testEnv.Create(watcherCtx, reloader)).To(Succeed())
		Expect(waitForObject(watcherCtx, testEnv.Client, reloader)).To(Succeed())

		gvk := schema.GroupVersionKind{
			Group:   appsv1.SchemeGroupVersion.Group,
			Version: appsv1.SchemeGroupVersion.Version,
			Kind:    "Deployment",
		}

		reconciler := &controllers.ReloaderReconciler{
			Client:       testEnv.Client,
			Scheme:       scheme,
			Mux:          sync.RWMutex{},
			GVKReloaders: make(map[schema.GroupVersionKind]*libsveltosset.Set),
		}

		policyRef := controllers.GetKeyFromObject(scheme, reloader)
		reconciler.GVKReloaders[gvk] = &libsveltosset.Set{}
		reconciler.GVKReloaders[gvk].Insert(policyRef)

		reloaderScope, err := scope.NewReloaderScope(scope.ReloaderScopeParams{
			Client:   testEnv.Client,
			Logger:   klogr.New(),
			Reloader: reloader,
		})
		Expect(err).To(BeNil())

		controllers.ReloaderReconcileDelete(reconciler, reloaderScope, klogr.New())
		Expect(reconciler.GVKReloaders[gvk].Len()).To(Equal(0))
	})

})
