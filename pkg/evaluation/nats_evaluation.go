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

package evaluation

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/cloudevents/sdk-go/v2/event"
	"github.com/go-logr/logr"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	libsveltosv1beta1 "github.com/projectsveltos/libsveltos/api/v1beta1"
	logs "github.com/projectsveltos/libsveltos/lib/logsettings"
)

type messagingUser struct {
	User     string `json:"user"`
	Password string `json:"password"`
}

type clientCert struct {
	CertPem []byte `json:"certPem"`
	KeyPem  []byte `json:"keyPem"`
}

type messagingAuthorization struct {
	User       *messagingUser `json:"user,omitempty"`
	Token      *string        `json:"token,omitempty"`
	ClientCert *clientCert    `json:"clientCert,omitempty"`
	RootCA     []byte         `json:"rootCA,omitempty"`
}

type configuration struct {
	URL           string                 `json:"url"`
	Subjects      []string               `json:"subjects"`
	Authorization messagingAuthorization `json:"authorization,omitempty"`
}

type natsConfiguration struct {
	Configuration configuration `json:"configuration"`
}

type jetstreamConfiguration struct {
	Configuration configuration `json:"configuration"`
}

type messagingConfig struct {
	Nats      *natsConfiguration      `json:"nats,omitempty"`
	Jetstream *jetstreamConfiguration `json:"jetstream,omitempty"`
}

const (
	natsSecretNamespace = "projectsveltos"
	natsSecretName      = "sveltos-nats" //nolint: gosec // name of the secret with NATS/JetStream configuration
	natsKey             = "sveltos-nats"
)

// listenForCloudEvents listens for CloudEvents over NATS/JetStream
func (m *manager) listenForCloudEvents(ctx context.Context) {
	// First load configuration
	configuration := m.loadMessagingConfiguration(ctx)

	// Then start a watcher. As soon as we start the watcher, if Secret
	// exists we get notified. Since Secret hash has already been saved
	// this notification will lead to no restart.
	// Do not change order. Otherwise a spurious restart might happen.
	m.startWatcherOnSecret(ctx)

	if configuration == nil {
		m.log.V(logs.LogInfo).Info("no messaging configuration")
		return
	}

	m.listenToNats(ctx, configuration.Nats)
	m.listenToJetStream(ctx, configuration.Jetstream)
}

func (m *manager) listenToNats(ctx context.Context, configuration *natsConfiguration) {
	if configuration == nil {
		m.log.V(logs.LogDebug).Info("no nats configuration")
		return
	}

	config := configuration.Configuration

	if config.URL == "" {
		m.log.V(logs.LogDebug).Info("nats URL is required")
	}

	if config.Subjects == nil {
		m.log.V(logs.LogDebug).Info("nats Subjects is required")
	}

	options := m.connectOptions(config.Authorization)

	var err error
	var nc *nats.Conn
	for {
		nc, err = nats.Connect(config.URL, options...)
		if err != nil {
			m.log.V(logs.LogInfo).Info(fmt.Sprintf("failed to connect to NATS: %v", err))
			if errors.Is(err, nats.ErrNoServers) {
				time.Sleep(time.Second)
				continue
			}
			return
		}
		break
	}

	m.log.V(logs.LogInfo).Info("connected to NATS")

	for i := range config.Subjects {
		subject := config.Subjects[i]
		_, err = nc.Subscribe(subject, func(msg *nats.Msg) {
			ce := event.New()

			err := ce.UnmarshalJSON(msg.Data)
			if err != nil {
				m.log.V(logs.LogInfo).Info(fmt.Sprintf("is not a cloud event: %v", err))
				return
			}

			if !m.isJSON(&ce) {
				m.log.V(logs.LogInfo).Info("cloud event data does not contain json")
				return
			}

			logger := m.log.WithValues("natsSubject", subject, "source", ce.Source(),
				"type", ce.Type(), "subject", ce.Subject())

			logger.V(logs.LogInfo).Info("received cloudEvent")

			err = m.evaluateCloudEvent(ctx, subject, &ce, logger)
			if err != nil {
				logger.V(logs.LogInfo).Info("cloudEvent lost due to error: %v", err)
			}
		})
		if err != nil {
			m.log.V(logs.LogInfo).Info("restarting due to subscription err: %v", err)
			restart()
		}
	}
}

func (m *manager) listenToJetStream(ctx context.Context, configuration *jetstreamConfiguration) {
	if configuration == nil {
		m.log.V(logs.LogDebug).Info("no jetStream configuration")
		return
	}

	config := configuration.Configuration

	if config.URL == "" {
		m.log.V(logs.LogDebug).Info("jetStream URL is required")
	}

	if config.Subjects == nil {
		m.log.V(logs.LogDebug).Info("jetStream Subjects is required")
	}

	options := m.connectOptions(config.Authorization)

	var err error
	var nc *nats.Conn
	for {
		nc, err = nats.Connect(config.URL, options...)
		if err != nil {
			m.log.V(logs.LogInfo).Info(fmt.Sprintf("failed to connect to NATS: %v", err))
			if errors.Is(err, nats.ErrNoServers) {
				time.Sleep(time.Second)
				continue
			}
			return
		}
		break
	}

	// create jetstream context from nats connection
	js, err := jetstream.New(nc)
	if err != nil {
		m.log.V(logs.LogInfo).Info(fmt.Sprintf("failed to get jetstream: %v", err))
		return
	}

	m.log.V(logs.LogInfo).Info("connected to JetStream")

	for i := range config.Subjects {
		subject := config.Subjects[i]
		stream, err := js.Stream(ctx, subject)
		if err != nil {
			m.log.V(logs.LogInfo).Info(fmt.Sprintf("Failed to get stream for subject: %s. %v", subject, err))
			continue
		}
		// retrieve consumer handle from a stream
		cons, err := stream.Consumer(ctx, "sveltos-agent")
		if err != nil {
			m.log.V(logs.LogInfo).Info(fmt.Sprintf("Failed to get consumer for subject: %s. %v", subject, err))
			continue
		}

		m.log.V(logs.LogInfo).Info(fmt.Sprintf("connected to NATS subject %s", subject))

		_, err = cons.Consume(func(msg jetstream.Msg) {
			ce := event.New()

			err := ce.UnmarshalJSON(msg.Data())
			if err != nil {
				m.log.V(logs.LogInfo).Info(fmt.Sprintf("is not a cloud event: %v", err))
				return
			}

			if !m.isJSON(&ce) {
				m.log.V(logs.LogInfo).Info("cloud event data does not contain json")
				return
			}

			logger := m.log.WithValues("jeatStreamSubject", subject, "source", ce.Source(),
				"type", ce.Type(), "subject", ce.Subject())

			logger.V(logs.LogInfo).Info("received cloudEvent")

			err = m.evaluateCloudEvent(ctx, subject, &ce, logger)
			if err != nil {
				err = msg.NakWithDelay(time.Second)
				if err != nil {
					logger.V(logs.LogInfo).Info(fmt.Sprintf("failed to nak: %v", err))
				}
				return
			}
			_ = msg.Ack()
		})
		if err != nil {
			m.log.V(logs.LogInfo).Info("restarting due to consume err: %v", err)
			restart()
		}
	}
}

// startWatcherOnSecret starts a watcher on Secret. NATS configuration is on a Secret
// when NATS configuration changes, sveltos-agent restarts
func (m *manager) startWatcherOnSecret(ctx context.Context) {
	for {
		err := m.startWatcher(ctx, getSecretGVK())
		if err == nil {
			break
		}
		m.log.V(logs.LogInfo).Info(fmt.Sprintf("failed to start watcher on Secrets: %v", err))
	}
}

// loadNATSConfiguration fetches the Secret with NATS configuration.
// If present, hash is evaluated and stored (so to detect configuration changes)
// Configuration is used to connect to NATS server.
func (m *manager) loadMessagingConfiguration(ctx context.Context) *messagingConfig {
	for {
		secret := &corev1.Secret{}
		err := m.Client.Get(ctx,
			types.NamespacedName{Namespace: natsSecretNamespace, Name: natsSecretName}, secret)
		if err != nil {
			if apierrors.IsNotFound(err) {
				m.log.V(logs.LogInfo).Info("no messaging configuration")
				return nil
			}
			m.log.V(logs.LogInfo).Info(fmt.Sprintf("failed to read messaging configuration %v", err))
			time.Sleep(time.Second)
			continue
		}

		configuration := messagingConfig{}
		err = json.Unmarshal(secret.Data[natsKey], &configuration)
		if err != nil {
			m.log.V(logs.LogInfo).Info(fmt.Sprintf("failed to read NATS configuration %v", err))
		}

		m.messagingConfigurationHash = m.evaluateHash(secret)
		return &configuration
	}
}

func (m *manager) evaluateHash(secret *corev1.Secret) []byte {
	if secret.Data == nil {
		return nil
	}

	if _, ok := secret.Data[natsKey]; !ok {
		return nil
	}

	h := sha256.New()
	var config string
	config += string(secret.Data[natsKey])
	h.Write([]byte(config))
	return h.Sum(nil)
}

func (m *manager) reactToNATS(gvk *schema.GroupVersionKind, obj interface{}) {
	if gvk == nil {
		return
	}

	secretGVK := getSecretGVK()
	if !reflect.DeepEqual(*gvk, *secretGVK) {
		return
	}

	unstructuredSecret, ok := obj.(*unstructured.Unstructured)
	if !ok {
		m.log.V(logs.LogInfo).Info("not a unstructured.Unstructured")
		return
	}

	if unstructuredSecret.GetNamespace() == natsSecretNamespace && unstructuredSecret.GetName() == natsSecretName {
		secret := &corev1.Secret{}
		err := runtime.DefaultUnstructuredConverter.FromUnstructured(unstructuredSecret.Object, secret)
		if err != nil {
			m.log.V(logs.LogInfo).Info(fmt.Sprintf("failed to convert to Secret %v, Restarting", err))
			restart()
		}

		currentHash := m.evaluateHash(secret)
		if !reflect.DeepEqual(currentHash, m.messagingConfigurationHash) {
			m.log.V(logs.LogInfo).Info("nats configuration changed, restarting")
			restart()
		}
	}
}

func restart() {
	if killErr := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); killErr != nil {
		panic("kill -TERM failed")
	}
}

func (m *manager) isJSON(ce *event.Event) bool {
	contentType := ce.DataContentType()
	return strings.Contains(contentType, "application/json")
}

func (m *manager) evaluateCloudEvent(ctx context.Context, natsSubject string, ce *event.Event,
	logger logr.Logger) error {

	m.nats.RLock()
	// Copy queue content. That is only operation that
	// needs to be done in a mutex protect section
	eventSourceForCloudEvent := make([]string, len(m.natsEventSources))
	i := 0
	for k := range m.natsEventSources {
		eventSourceForCloudEvent[i] = k
		i++
	}
	m.nats.RUnlock()

	for i := range eventSourceForCloudEvent {
		err := m.processCloudEvent(ctx, natsSubject, ce, eventSourceForCloudEvent[i], logger)
		if err != nil {
			return err
		}
	}

	return nil
}

func (m *manager) processCloudEvent(ctx context.Context, natsSubject string, ce *event.Event,
	eventSourceName string, logger logr.Logger) error {

	eventSource := &libsveltosv1beta1.EventSource{}

	logger = logger.WithValues("eventSource", eventSourceName)
	logger.V(logs.LogDebug).Info("evaluating")

	err := m.Client.Get(ctx, types.NamespacedName{Name: eventSourceName}, eventSource)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return m.cleanEventReport(ctx, eventSourceName)
		}
		return err
	}

	if !eventSource.DeletionTimestamp.IsZero() {
		return m.cleanEventReport(ctx, eventSourceName)
	}

	ceJsonBytes, err := json.Marshal(ce)
	if err != nil {
		logger.Error(err, "failed to marshal cloudEvent: %v", err)
		return err
	}

	for i := range eventSource.Spec.MessagingMatchCriteria {
		criteria := eventSource.Spec.MessagingMatchCriteria[i]
		if m.isCloudEventAMatch(ce, natsSubject, criteria) {
			logger.V(logs.LogDebug).Info("message is a match")

			err = m.createEventReport(ctx, eventSource, nil, nil, ceJsonBytes)
			if err != nil {
				logger.Error(err, "failed to create/update EventReport")
				return err
			}

			if m.sendReport {
				err = m.sendEventReport(ctx, eventSource)
				if err != nil {
					logger.Error(err, "failed to send EventReport")
					return err
				}
			}

			return nil
		}
	}

	return nil
}

func (m *manager) isCloudEventAMatch(ce *event.Event, natsSubject string,
	criteria libsveltosv1beta1.MessagingMatchCriteria) bool {

	isSubjectMatch := m.isStringMatchingRegex(criteria.Subject, natsSubject)

	isCESourceMatch := m.isStringMatchingRegex(criteria.CloudEventSource, ce.Source())

	isCESubjectMatch := m.isStringMatchingRegex(criteria.CloudEventSubject, ce.Subject())

	isCETypeMatch := m.isStringMatchingRegex(criteria.CloudEventType, ce.Type())

	if isSubjectMatch && isCESourceMatch && isCESubjectMatch && isCETypeMatch {
		return true
	}

	return false
}

func (m *manager) isStringMatchingRegex(regexString, stringToCheck string) bool {
	if regexString == "" {
		return true
	}
	r, err := regexp.Compile(regexString)
	if err != nil {
		m.log.V(logs.LogInfo).Info(fmt.Sprintf("regexp %s not valida: %v", regexString, err))
		return false
	}
	return r.MatchString(stringToCheck)
}

func (m *manager) connectOptions(authorization messagingAuthorization) []nats.Option {
	options := make([]nats.Option, 0)
	if authorization.User != nil {
		options = append(options,
			nats.UserInfo(authorization.User.User, authorization.User.Password))
	}
	if authorization.Token != nil {
		options = append(options, nats.Token(*authorization.Token))
	}
	if authorization.ClientCert != nil {
		certPem, err := createTempFileWithContent(authorization.ClientCert.CertPem)
		if err != nil {
			m.log.V(logs.LogInfo).Info(fmt.Sprintf("failed to create file with cert: %v", err))
			return options
		}
		keyPem, err := createTempFileWithContent(authorization.ClientCert.KeyPem)
		if err != nil {
			m.log.V(logs.LogInfo).Info(fmt.Sprintf("failed to create file with key: %v", err))
			return options
		}
		options = append(options, nats.ClientCert(certPem, keyPem))
	}
	if authorization.RootCA != nil {
		rootCA, err := createTempFileWithContent(authorization.RootCA)
		if err != nil {
			m.log.V(logs.LogInfo).Info(fmt.Sprintf("failed to create file with root CA: %v", err))
			return options
		}
		options = append(options, nats.RootCAs(rootCA))
	}

	return options
}

func createTempFileWithContent(data []byte) (string, error) {
	tmpfile, err := os.CreateTemp("", "tempfile.*")
	if err != nil {
		return "", err
	}
	defer tmpfile.Close() // Ensure the file is closed when we're done

	// Write the data to the temporary file
	_, err = tmpfile.Write(data)
	if err != nil {
		// Clean up if there's an error writing
		os.Remove(tmpfile.Name())
		return "", err
	}

	// Important: Flush the buffer to ensure all data is written to disk
	err = tmpfile.Sync()
	if err != nil {
		os.Remove(tmpfile.Name())
		return "", err
	}

	return tmpfile.Name(), nil
}
