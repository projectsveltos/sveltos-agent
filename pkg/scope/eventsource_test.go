/*
Copyright 2023. projectsveltos.io. All rights reserved. projectsveltos.io. All rights reserved.

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

package scope_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2/textlogger"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	libsveltosv1beta1 "github.com/projectsveltos/libsveltos/api/v1beta1"
	"github.com/projectsveltos/sveltos-agent/pkg/scope"
)

const (
	eventSourceNamePrefix = "scope-"
)

var _ = Describe("EventSourceScope", func() {
	var eventSource *libsveltosv1beta1.EventSource
	var c client.Client

	BeforeEach(func() {
		eventSource = &libsveltosv1beta1.EventSource{
			ObjectMeta: metav1.ObjectMeta{
				Name: eventSourceNamePrefix + randomString(),
			},
		}

		scheme := setupScheme()
		initObjects := []client.Object{eventSource}
		c = fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(initObjects...).WithObjects(initObjects...).Build()
	})

	It("Return nil,error if client is not specified", func() {
		params := scope.EventSourceScopeParams{
			EventSource: eventSource,
			Logger:      textlogger.NewLogger(textlogger.NewConfig(textlogger.Verbosity(1))),
		}

		scope, err := scope.NewEventSourceScope(params)
		Expect(err).To(HaveOccurred())
		Expect(scope).To(BeNil())
	})

	It("Return nil,error if EventSource is not specified", func() {
		params := scope.EventSourceScopeParams{
			Client: c,
			Logger: textlogger.NewLogger(textlogger.NewConfig(textlogger.Verbosity(1))),
		}

		scope, err := scope.NewEventSourceScope(params)
		Expect(err).To(HaveOccurred())
		Expect(scope).To(BeNil())
	})

	It("Name returns EventSource Name", func() {
		params := scope.EventSourceScopeParams{
			Client:      c,
			EventSource: eventSource,
			Logger:      textlogger.NewLogger(textlogger.NewConfig(textlogger.Verbosity(1))),
		}

		scope, err := scope.NewEventSourceScope(params)
		Expect(err).ToNot(HaveOccurred())
		Expect(scope).ToNot(BeNil())

		Expect(scope.Name()).To(Equal(eventSource.Name))
	})
})
