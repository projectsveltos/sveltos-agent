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

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/klog/v2/textlogger"

	libsveltosv1alpha1 "github.com/projectsveltos/libsveltos/api/v1alpha1"
	libsveltosset "github.com/projectsveltos/libsveltos/lib/set"
	"github.com/projectsveltos/sveltos-agent/controllers"
	"github.com/projectsveltos/sveltos-agent/pkg/evaluation"
	"github.com/projectsveltos/sveltos-agent/pkg/scope"
)

var _ = Describe("Controllers: healthCheck controller", func() {
	var watcherCtx context.Context
	var cancel context.CancelFunc

	BeforeEach(func() {
		watcherCtx, cancel = context.WithCancel(context.Background())
		evaluation.InitializeManager(watcherCtx, textlogger.NewLogger(textlogger.NewConfig(textlogger.Verbosity(1))),
			testEnv.Config, testEnv.Client, randomString(), randomString(), libsveltosv1alpha1.ClusterTypeCapi, 10, false)
	})

	AfterEach(func() {
		healthChecks := &libsveltosv1alpha1.HealthCheckList{}
		Expect(testEnv.List(context.TODO(), healthChecks)).To(Succeed())

		for i := range healthChecks.Items {
			Expect(testEnv.Delete(context.TODO(), &healthChecks.Items[i]))
		}

		cancel()
	})

	It("updateMaps updates map of HealthCheck using a gvk", func() {
		healthCheck := getHealthCheck()
		Expect(testEnv.Create(watcherCtx, healthCheck)).To(Succeed())
		Expect(waitForObject(watcherCtx, testEnv.Client, healthCheck)).To(Succeed())

		rs := healthCheck.Spec.ResourceSelectors
		Expect(rs).ToNot(BeNil())

		reconciler := &controllers.HealthCheckReconciler{
			Client:          testEnv.Client,
			Scheme:          scheme,
			Mux:             sync.RWMutex{},
			GVKHealthChecks: make(map[schema.GroupVersionKind]*libsveltosset.Set),
		}

		controllers.HealthCheckUpdateMaps(reconciler, healthCheck)
		Expect(len(reconciler.GVKHealthChecks)).To(Equal(len(rs)))
		for i := range rs {
			gvk := schema.GroupVersionKind{
				Group:   rs[i].Group,
				Version: rs[i].Version,
				Kind:    rs[i].Kind,
			}
			v, ok := reconciler.GVKHealthChecks[gvk]
			Expect(ok).To(BeTrue())
			Expect(v.Len()).To(Equal(1))
			items := reconciler.GVKHealthChecks[gvk].Items()
			Expect(items[0].Name).To(Equal(healthCheck.Name))
		}
	})

	It("reconcileDelete remove healthCheck from GVKHealthChecks map", func() {
		healthCheck := getHealthCheck()
		Expect(testEnv.Create(watcherCtx, healthCheck)).To(Succeed())
		Expect(waitForObject(watcherCtx, testEnv.Client, healthCheck)).To(Succeed())

		rs := healthCheck.Spec.ResourceSelectors
		Expect(rs).ToNot(BeNil())

		reconciler := &controllers.HealthCheckReconciler{
			Client:          testEnv.Client,
			Scheme:          scheme,
			Mux:             sync.RWMutex{},
			GVKHealthChecks: make(map[schema.GroupVersionKind]*libsveltosset.Set),
		}

		for i := range rs {
			gvk := schema.GroupVersionKind{
				Group:   rs[i].Group,
				Version: rs[i].Version,
				Kind:    rs[i].Kind,
			}

			policyRef := controllers.GetKeyFromObject(scheme, healthCheck)
			reconciler.GVKHealthChecks[gvk] = &libsveltosset.Set{}
			reconciler.GVKHealthChecks[gvk].Insert(policyRef)
		}
		healthCheckScope, err := scope.NewHealthCheckScope(scope.HealthCheckScopeParams{
			Client:      testEnv.Client,
			Logger:      textlogger.NewLogger(textlogger.NewConfig(textlogger.Verbosity(1))),
			HealthCheck: healthCheck,
		})
		Expect(err).To(BeNil())

		controllers.HealthCheckReconcileDelete(reconciler, context.TODO(), healthCheckScope,
			textlogger.NewLogger(textlogger.NewConfig(textlogger.Verbosity(1))))

		for i := range rs {
			gvk := schema.GroupVersionKind{
				Group:   rs[i].Group,
				Version: rs[i].Version,
				Kind:    rs[i].Kind,
			}
			Expect(reconciler.GVKHealthChecks[gvk].Len()).To(Equal(0))
		}
	})
})
