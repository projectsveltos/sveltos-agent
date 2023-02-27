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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2/klogr"

	"github.com/projectsveltos/classifier-health-agent/controllers"
	"github.com/projectsveltos/classifier-health-agent/pkg/evaluation"
	"github.com/projectsveltos/classifier-health-agent/pkg/scope"
	libsveltosv1alpha1 "github.com/projectsveltos/libsveltos/api/v1alpha1"
	libsveltosset "github.com/projectsveltos/libsveltos/lib/set"
)

var _ = Describe("Controllers: healthCheck controller", func() {
	var watcherCtx context.Context
	var cancel context.CancelFunc

	BeforeEach(func() {
		watcherCtx, cancel = context.WithCancel(context.Background())
		evaluation.InitializeManager(watcherCtx, klogr.New(), testEnv.Config, testEnv.Client,
			randomString(), randomString(), libsveltosv1alpha1.ClusterTypeCapi, 10, false)
	})

	AfterEach(func() {
		healthChecks := &libsveltosv1alpha1.HealthCheckList{}
		Expect(testEnv.List(context.TODO(), healthChecks)).To(Succeed())

		for i := range healthChecks.Items {
			Expect(testEnv.Delete(context.TODO(), &healthChecks.Items[i]))
		}

		cancel()
	})

	It("updateMaps updates map of HealthCheck using DeployedResourceConstraints verion as criteria", func() {
		healthCheck := getHealthCheck()
		Expect(testEnv.Create(watcherCtx, healthCheck)).To(Succeed())
		Expect(waitForObject(watcherCtx, testEnv.Client, healthCheck)).To(Succeed())

		reconciler := &controllers.HealthCheckReconciler{
			Client:          testEnv.Client,
			Scheme:          scheme,
			Mux:             sync.RWMutex{},
			GVKHealthChecks: make(map[schema.GroupVersionKind]*libsveltosset.Set),
			HealthCheckMap:  make(map[types.NamespacedName]*libsveltosset.Set),
			ReferenceMap:    make(map[corev1.ObjectReference]*libsveltosset.Set),
		}

		controllers.HealthCheckUpdateMaps(reconciler, healthCheck)
		Expect(len(reconciler.GVKHealthChecks)).To(Equal(1))
		gvk := schema.GroupVersionKind{
			Group:   healthCheck.Spec.Group,
			Version: healthCheck.Spec.Version,
			Kind:    healthCheck.Spec.Kind,
		}
		v, ok := reconciler.GVKHealthChecks[gvk]
		Expect(ok).To(BeTrue())
		Expect(v.Len()).To(Equal(1))
		items := reconciler.GVKHealthChecks[gvk].Items()
		Expect(items[0].Name).To(Equal(healthCheck.Name))
	})

	It("reconcileDelete remove healthCheck from GVKHealthChecks map", func() {
		healthCheck := getHealthCheck()
		Expect(testEnv.Create(watcherCtx, healthCheck)).To(Succeed())
		Expect(waitForObject(watcherCtx, testEnv.Client, healthCheck)).To(Succeed())

		gvk := schema.GroupVersionKind{
			Group:   healthCheck.Spec.Group,
			Version: healthCheck.Spec.Version,
			Kind:    healthCheck.Spec.Kind,
		}

		reconciler := &controllers.HealthCheckReconciler{
			Client:          testEnv.Client,
			Scheme:          scheme,
			Mux:             sync.RWMutex{},
			GVKHealthChecks: make(map[schema.GroupVersionKind]*libsveltosset.Set),
			HealthCheckMap:  make(map[types.NamespacedName]*libsveltosset.Set),
			ReferenceMap:    make(map[corev1.ObjectReference]*libsveltosset.Set),
		}

		policyRef := controllers.GetKeyFromObject(scheme, healthCheck)
		reconciler.GVKHealthChecks[gvk] = &libsveltosset.Set{}
		reconciler.GVKHealthChecks[gvk].Insert(policyRef)

		healthCheckScope, err := scope.NewHealthCheckScope(scope.HealthCheckScopeParams{
			Client:      testEnv.Client,
			Logger:      klogr.New(),
			HealthCheck: healthCheck,
		})
		Expect(err).To(BeNil())

		_, err = controllers.HealthCheckReconcileDelete(reconciler, context.TODO(), healthCheckScope, klogr.New())
		Expect(err).To(BeNil())
		Expect(reconciler.GVKHealthChecks[gvk].Len()).To(Equal(0))
	})
})
