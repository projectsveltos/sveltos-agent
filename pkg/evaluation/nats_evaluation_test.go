/*
Copyright 2024. projectsveltos.io. All rights reserved.

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

package evaluation_test

import (
	"context"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	cloudevents "github.com/cloudevents/sdk-go/v2"
	"github.com/cloudevents/sdk-go/v2/event"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2/textlogger"

	libsveltosv1beta1 "github.com/projectsveltos/libsveltos/api/v1beta1"
	"github.com/projectsveltos/sveltos-agent/pkg/evaluation"
	"github.com/projectsveltos/sveltos-agent/pkg/utils"
)

var _ = Describe("Manager: nats evaluation", func() {
	var eventSource *libsveltosv1beta1.EventSource
	var clusterNamespace string
	var clusterName string
	var clusterType libsveltosv1beta1.ClusterType
	var logger logr.Logger

	type ExampleData struct {
		Message string `json:"message"`
		Value   int    `json:"value"`
	}
	var jsonData ExampleData

	BeforeEach(func() {
		evaluation.Reset()
		clusterNamespace = utils.ReportNamespace
		clusterName = randomString()
		clusterType = libsveltosv1beta1.ClusterTypeCapi
		logger = textlogger.NewLogger(textlogger.NewConfig(textlogger.Verbosity(1)))

		jsonData = ExampleData{
			Message: randomString(),
			Value:   42,
		}
	})

	AfterEach(func() {
		if eventSource != nil {
			err := testEnv.Client.Delete(context.TODO(), eventSource)
			if err != nil {
				Expect(apierrors.IsNotFound(err)).To(BeTrue())
			}
		}
	})

	It("processCloudEvent: evaluates cloudEvent is a match", func() {
		evaluation.InitializeManagerWithSkip(context.TODO(), logger,
			testEnv.Config, testEnv.Client, clusterNamespace, clusterName, clusterType, 10)

		manager := evaluation.GetManager()
		Expect(manager).ToNot(BeNil())

		ce := getCloudEvent(randomString(), randomString(), randomString())
		Expect(ce.SetData(cloudevents.ApplicationJSON, jsonData)).To(Succeed())
		eventSource := &libsveltosv1beta1.EventSource{
			ObjectMeta: metav1.ObjectMeta{
				Name: randomString(),
			},
			Spec: libsveltosv1beta1.EventSourceSpec{
				MessagingMatchCriteria: []libsveltosv1beta1.MessagingMatchCriteria{
					{
						CloudEventSource:  ce.Context.GetSource(),
						CloudEventType:    ce.Context.GetType(),
						CloudEventSubject: ce.Context.GetSubject(),
					},
				},
			},
		}

		Expect(testEnv.Create(context.TODO(), eventSource)).To(Succeed())
		waitForObject(context.TODO(), testEnv.Client, eventSource)

		Expect(evaluation.ProcessCloudEvent(manager, context.TODO(), "", ce,
			eventSource.Name, logger)).To(Succeed())

		// CloudEvent is a match, so EventReport is created
		Eventually(func() bool {
			eventReport := &libsveltosv1beta1.EventReport{}
			err := testEnv.Get(context.TODO(),
				types.NamespacedName{Namespace: utils.ReportNamespace, Name: eventSource.Name},
				eventReport)
			if err != nil {
				return false
			}

			return len(eventReport.Spec.CloudEvents) == 1
		}, timeout, pollingInterval).Should(BeTrue())

		Expect(evaluation.ProcessCloudEvent(manager, context.TODO(), "", ce,
			eventSource.Name, logger)).To(Succeed())

		// CloudEvent is a match, so EventReport is updated. New CloudEvent
		// is appended to previous one
		Eventually(func() bool {
			eventReport := &libsveltosv1beta1.EventReport{}
			err := testEnv.Get(context.TODO(),
				types.NamespacedName{Namespace: utils.ReportNamespace, Name: eventSource.Name},
				eventReport)
			if err != nil {
				return false
			}

			return len(eventReport.Spec.CloudEvents) == 2
		}, timeout, pollingInterval).Should(BeTrue())

	})

	It("processCloudEvent: evaluates cloudEvent is a not match", func() {
		evaluation.InitializeManagerWithSkip(context.TODO(), logger,
			testEnv.Config, testEnv.Client, clusterNamespace, clusterName, clusterType, 10)

		manager := evaluation.GetManager()
		Expect(manager).ToNot(BeNil())

		ce := getCloudEvent(randomString(), randomString(), randomString())
		Expect(ce.SetData(cloudevents.ApplicationJSON, jsonData)).To(Succeed())
		eventSource := &libsveltosv1beta1.EventSource{
			ObjectMeta: metav1.ObjectMeta{
				Name: randomString(),
			},
			Spec: libsveltosv1beta1.EventSourceSpec{
				MessagingMatchCriteria: []libsveltosv1beta1.MessagingMatchCriteria{
					{
						CloudEventSource:  randomString(), // not a match
						CloudEventType:    ce.Context.GetType(),
						CloudEventSubject: ce.Context.GetSubject(),
					},
				},
			},
		}

		Expect(testEnv.Create(context.TODO(), eventSource)).To(Succeed())
		waitForObject(context.TODO(), testEnv.Client, eventSource)

		Expect(evaluation.ProcessCloudEvent(manager, context.TODO(), "", ce,
			eventSource.Name, logger)).To(Succeed())

		// CloudEvent is a match, so EventReport is created
		Consistently(func() bool {
			eventReport := &libsveltosv1beta1.EventReport{}
			err := testEnv.Get(context.TODO(),
				types.NamespacedName{Namespace: utils.ReportNamespace, Name: eventSource.Name},
				eventReport)
			if err == nil {
				return false // only for a match EventReport is created
			}
			return apierrors.IsNotFound(err)
		}, timeout, pollingInterval).Should(BeTrue())
	})

	It("loadMessagingConfiguration: load NATS/JetStream configuration from Secret", func() {
		evaluation.InitializeManagerWithSkip(context.TODO(), logger,
			testEnv.Config, testEnv.Client, clusterNamespace, clusterName, clusterType, 10)

		manager := evaluation.GetManager()
		Expect(manager).ToNot(BeNil())

		configString := `{
			"nats": {
				"configuration": {
					"url": "nats://nats.nats.svc.cluster.local:4222",
					"subjects": ["test", "foo"],
					"authorization": {
						"user": {
							"user": "admin",
							"password": "your_password"
						}
					}
				}
			}
		}`

		secret := corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: evaluation.NatsSecretNamespace,
				Name:      evaluation.NatsSecretName,
			},
			Data: map[string][]byte{
				evaluation.NatsKey: []byte(configString),
			},
		}
		Expect(testEnv.Create(context.TODO(), &secret)).To(Succeed())
		waitForObject(context.TODO(), testEnv, &secret)

		currentConfig := evaluation.LoadMessagingConfiguration(manager, context.TODO())
		Expect(currentConfig).ToNot(BeNil())
		Expect(currentConfig.Nats).ToNot(BeNil())
		Expect(currentConfig.Nats.Configuration.Authorization.User.User).ToNot(BeNil())
		Expect(currentConfig.Nats.Configuration.Authorization.User.Password).ToNot(BeNil())
	})

	It("connectOptions returns nats.Option", func() {
		evaluation.InitializeManagerWithSkip(context.TODO(), logger,
			testEnv.Config, testEnv.Client, clusterNamespace, clusterName, clusterType, 10)

		manager := evaluation.GetManager()
		Expect(manager).ToNot(BeNil())

		authorization := evaluation.MessagingAuthorization{
			User: &evaluation.MessagingUser{
				User:     randomString(),
				Password: randomString(),
			},
		}

		options := evaluation.ConnectOptions(manager, authorization)
		Expect(len(options)).To(Equal(1))
	})

	It("isCloudEventAMatch identifies matching CloudEvents", func() {
		evaluation.InitializeManagerWithSkip(context.TODO(), logger,
			testEnv.Config, testEnv.Client, clusterNamespace, clusterName, clusterType, 10)

		manager := evaluation.GetManager()
		Expect(manager).ToNot(BeNil())

		sourcePrefix := randomString()
		ceSource := sourcePrefix + randomString()
		subjectSuffix := randomString()
		ceSubject := randomString() + subjectSuffix

		ceTypeMid := randomString()
		ceType := randomString() + ceTypeMid + randomString()

		natsSubjectPrefix := randomString()
		natsSubject := natsSubjectPrefix + randomString()

		ce := getCloudEvent(ceSource, ceSubject, ceType)
		Expect(ce.SetData(cloudevents.ApplicationJSON, jsonData)).To(Succeed())

		matchingCriteria := libsveltosv1beta1.MessagingMatchCriteria{
			Subject:           fmt.Sprintf("^%s.*", natsSubjectPrefix), // match any string starting with $natsSubjectPrefix,
			CloudEventSource:  fmt.Sprintf("^%s.*", sourcePrefix),      // match any string starting with $sourcePrefix
			CloudEventSubject: fmt.Sprintf(".*%s$", subjectSuffix),     // match any string ending with $subjectSuffix
			CloudEventType:    fmt.Sprintf(".*%s.*", ceTypeMid),        // match any string containing $ceTypeMid
		}

		Expect(evaluation.IsCloudEventAMatch(manager, ce, natsSubject, matchingCriteria)).To(BeTrue())

		// CE Subject is not a match anymore
		copyCE := ce.Clone()
		copyCE.SetSubject(randomString())
		Expect(evaluation.IsCloudEventAMatch(manager, &copyCE, randomString(), matchingCriteria)).To(BeFalse())

		// CE Source is not a match anymore
		copyCE = ce.Clone()
		copyCE.SetSource(randomString())
		Expect(evaluation.IsCloudEventAMatch(manager, &copyCE, randomString(), matchingCriteria)).To(BeFalse())

		// CE Type is not a match anymore
		copyCE = ce.Clone()
		copyCE.SetType(randomString())
		Expect(evaluation.IsCloudEventAMatch(manager, &copyCE, randomString(), matchingCriteria)).To(BeFalse())

		// NATS Subject is not a match
		Expect(evaluation.IsCloudEventAMatch(manager, ce, randomString(), matchingCriteria)).To(BeFalse())
	})
})

func getCloudEvent(ceSource, ceSubject, ceType string) *event.Event {
	ce := event.New()
	ce.SetSource(ceSource)
	ce.SetSubject(ceSubject)
	ce.SetType(ceType)
	return &ce
}
