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
	"k8s.io/klog/v2"
	"k8s.io/klog/v2/textlogger"

	libsveltosv1beta1 "github.com/projectsveltos/libsveltos/api/v1beta1"
	libsveltosset "github.com/projectsveltos/libsveltos/lib/set"
	"github.com/projectsveltos/sveltos-agent/controllers"
	"github.com/projectsveltos/sveltos-agent/pkg/evaluation"
	"github.com/projectsveltos/sveltos-agent/pkg/scope"
)

var _ = Describe("Controllers: eventSource controller", func() {
	var watcherCtx context.Context
	var cancel context.CancelFunc

	BeforeEach(func() {
		watcherCtx, cancel = context.WithCancel(context.Background())
		evaluation.InitializeManager(watcherCtx, textlogger.NewLogger(textlogger.NewConfig(textlogger.Verbosity(1))),
			testEnv.Config, testEnv.Client, randomString(), randomString(), randomString(),
			libsveltosv1beta1.ClusterTypeCapi, int64(3), false)
	})

	AfterEach(func() {
		eventSources := &libsveltosv1beta1.EventSourceList{}
		Expect(testEnv.List(context.TODO(), eventSources)).To(Succeed())

		for i := range eventSources.Items {
			Expect(testEnv.Delete(context.TODO(), &eventSources.Items[i]))
		}

		cancel()
	})

	It("updateMaps updates map of EventSource using a gvk", func() {
		eventSource := getEventSource()
		Expect(testEnv.Create(watcherCtx, eventSource)).To(Succeed())
		Expect(waitForObject(watcherCtx, testEnv.Client, eventSource)).To(Succeed())

		reconciler := &controllers.EventSourceReconciler{
			Client:          testEnv.Client,
			Scheme:          scheme,
			Mux:             sync.RWMutex{},
			GVKEventSources: make(map[schema.GroupVersionKind]*libsveltosset.Set),
		}

		controllers.EventSourceUpdateMaps(reconciler, eventSource)
		Expect(len(reconciler.GVKEventSources)).To(Equal(1))
		for i := range eventSource.Spec.ResourceSelectors {
			rs := &eventSource.Spec.ResourceSelectors[i]
			gvk := schema.GroupVersionKind{
				Group:   rs.Group,
				Version: rs.Version,
				Kind:    rs.Kind,
			}
			v, ok := reconciler.GVKEventSources[gvk]
			Expect(ok).To(BeTrue())
			Expect(v.Len()).To(Equal(1))
			items := reconciler.GVKEventSources[gvk].Items()
			Expect(items[0].Name).To(Equal(eventSource.Name))
		}
	})

	It("reconcileDelete remove eventSource from GVKEventSources map", func() {
		eventSource := getEventSource()
		Expect(testEnv.Create(watcherCtx, eventSource)).To(Succeed())
		Expect(waitForObject(watcherCtx, testEnv.Client, eventSource)).To(Succeed())

		policyRef := controllers.GetKeyFromObject(scheme, eventSource)

		reconciler := &controllers.EventSourceReconciler{
			Client:          testEnv.Client,
			Scheme:          scheme,
			Mux:             sync.RWMutex{},
			GVKEventSources: make(map[schema.GroupVersionKind]*libsveltosset.Set),
		}

		gvks := []schema.GroupVersionKind{}
		for i := range eventSource.Spec.ResourceSelectors {
			rs := &eventSource.Spec.ResourceSelectors[i]
			gvk := schema.GroupVersionKind{
				Group:   rs.Group,
				Version: rs.Version,
				Kind:    rs.Kind,
			}
			gvks = append(gvks, gvk)

			reconciler.GVKEventSources[gvk] = &libsveltosset.Set{}
			reconciler.GVKEventSources[gvk].Insert(policyRef)

		}

		eventSourceScope, err := scope.NewEventSourceScope(scope.EventSourceScopeParams{
			Client:      testEnv.Client,
			Logger:      klog.FromContext(ctx),
			EventSource: eventSource,
		})
		Expect(err).To(BeNil())

		controllers.EventSourceReconcileDelete(reconciler, context.TODO(), eventSourceScope,
			textlogger.NewLogger(textlogger.NewConfig(textlogger.Verbosity(1))))
		for i := range gvks {
			Expect(reconciler.GVKEventSources[gvks[i]].Len()).To(Equal(0))
		}
	})
})
