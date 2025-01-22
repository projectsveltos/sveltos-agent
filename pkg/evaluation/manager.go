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
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"emperror.dev/errors"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2/textlogger"
	"sigs.k8s.io/controller-runtime/pkg/client"

	libsveltosv1beta1 "github.com/projectsveltos/libsveltos/api/v1beta1"
	"github.com/projectsveltos/libsveltos/lib/crd"
	logs "github.com/projectsveltos/libsveltos/lib/logsettings"
	"github.com/projectsveltos/libsveltos/lib/sveltos_upgrade"
)

var (
	getManagerLock  = &sync.Mutex{}
	managerInstance *manager
)

type ReactToNotification func(gvk *schema.GroupVersionKind)

// manager represents a client implementing the ClassifierInterface
type manager struct {
	log logr.Logger
	client.Client
	config *rest.Config

	sendReport       bool
	clusterNamespace string
	clusterName      string
	clusterType      libsveltosv1beta1.ClusterType

	watchMu *sync.Mutex
	// rebuildResourceToWatch indicates (value different from zero) that list
	// of resources to watch needs to be rebuilt
	rebuildResourceToWatch uint32
	// resourcesToWatch contains list of GVKs to watch
	resourcesToWatch []schema.GroupVersionKind

	mu *sync.Mutex

	// classifierJobQueue contains name of all Classifier instances that need to be evaluated
	classifierJobQueue map[string]bool

	// healthCheckJobQueue contains name of all HealthCheck instances that need to be evaluated
	healthCheckJobQueue map[string]bool

	// eventSourceJobQueue contains name of all EventSource instances that need to be evaluated
	eventSourceJobQueue map[string]bool

	// reloaderJobQueue contains:
	// - kind:name of all Reloader instances that need to be evaluated
	// - kind:namespace/name of all ConfigMap instances that have changed
	// - kind:namespace/name of all Secrets instances that have changed
	reloaderJobQueue map[string]bool

	// interval is the interval at which queued Classifiers are evaluated
	interval time.Duration

	// List of gvk with a watcher
	// Key: GroupResourceVersion currently being watched
	// Value: stop channel
	watchers map[schema.GroupVersionKind]context.CancelFunc

	// List of resources to watch not installed in the cluster yet
	unknownResourcesToWatch []schema.GroupVersionKind

	// react is the method that gets invoked when any of the resources
	// being watched changes
	reactClassifier  ReactToNotification
	reactHealthCheck ReactToNotification
	reactEventSource ReactToNotification
	reactReloader    ReactToNotification
}

// InitializeManager initializes a manager implementing the ClassifierInterface
func InitializeManager(ctx context.Context, l logr.Logger, config *rest.Config, c client.Client,
	clusterNamespace, clusterName, version string, cluserType libsveltosv1beta1.ClusterType,
	intervalInSecond int64, sendReport bool) {

	if managerInstance == nil {
		getManagerLock.Lock()
		defer getManagerLock.Unlock()
		if managerInstance == nil {
			l.V(logs.LogInfo).Info(fmt.Sprintf("Creating manager now. Interval (in seconds): %d", intervalInSecond))
			managerInstance = &manager{log: l, Client: c, config: config}
			managerInstance.classifierJobQueue = make(map[string]bool)
			managerInstance.healthCheckJobQueue = make(map[string]bool)
			managerInstance.eventSourceJobQueue = make(map[string]bool)
			managerInstance.reloaderJobQueue = make(map[string]bool)
			managerInstance.interval = time.Duration(intervalInSecond) * time.Second
			managerInstance.mu = &sync.Mutex{}

			managerInstance.resourcesToWatch = make([]schema.GroupVersionKind, 0)
			managerInstance.rebuildResourceToWatch = 0
			managerInstance.watchMu = &sync.Mutex{}

			managerInstance.unknownResourcesToWatch = make([]schema.GroupVersionKind, 0)

			managerInstance.watchers = make(map[schema.GroupVersionKind]context.CancelFunc)

			managerInstance.sendReport = sendReport
			managerInstance.clusterNamespace = clusterNamespace
			managerInstance.clusterName = clusterName
			managerInstance.clusterType = cluserType

			initializeReloaderMaps()

			var wg sync.WaitGroup

			go managerInstance.evaluateClassifiers(ctx, &wg)
			wg.Add(1)
			go managerInstance.evaluateHealthChecks(ctx, &wg)
			wg.Add(1)
			go managerInstance.evaluateEventSources(ctx, &wg)
			wg.Add(1)
			go managerInstance.evaluateReloaders(ctx, &wg)
			wg.Add(1)

			go managerInstance.buildResourceToWatch(ctx)
			go managerInstance.storeVersionForCompatibilityChecks(ctx, version, &wg)
			// Start a watcher for CustomResourceDefinition
			go crd.WatchCustomResourceDefinition(ctx, managerInstance.config,
				restartIfNeeded, managerInstance.log)
		}
	}
}

// GetManager returns the manager instance implementing the ClassifierInterface.
// Returns nil if manager has not been initialized yet
func GetManager() *manager {
	getManagerLock.Lock()
	defer getManagerLock.Unlock()
	if managerInstance != nil {
		return managerInstance
	}
	return nil
}

func (m *manager) Reset() {
	getManagerLock.Lock()
	defer getManagerLock.Unlock()
	managerInstance = nil
}

func (m *manager) RegisterClassifierMethod(react ReactToNotification) {
	m.reactClassifier = react
}

func (m *manager) RegisterHealthCheckMethod(react ReactToNotification) {
	m.reactHealthCheck = react
}

func (m *manager) RegisterEventSourceMethod(react ReactToNotification) {
	m.reactEventSource = react
}

func (m *manager) RegisterReloaderMethod(react ReactToNotification) {
	m.reactReloader = react
}

func (m *manager) ReEvaluateResourceToWatch() {
	atomic.StoreUint32(&m.rebuildResourceToWatch, 1)
}

// EvaluateClassifier queues a Classifier instance for evaluation
func (m *manager) EvaluateClassifier(classifierName string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	logger := m.log
	logger = logger.WithValues("classifier", classifierName)

	logger.V(logs.LogDebug).Info("queue classifier for evaluation")

	m.classifierJobQueue[classifierName] = true
}

// EvaluateHealthCheck queues a HealthCheck instance for evaluation
func (m *manager) EvaluateHealthCheck(healthCheckName string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	logger := m.log
	logger = logger.WithValues("healthCheck", healthCheckName)

	logger.V(logs.LogDebug).Info("queue healthCheck for evaluation")

	m.healthCheckJobQueue[healthCheckName] = true
}

// EvaluateEventSource queues a EventSource instance for evaluation
func (m *manager) EvaluateEventSource(eventSourceName string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	logger := m.log
	logger = logger.WithValues("eventSource", eventSourceName)

	logger.V(logs.LogDebug).Info("queue eventSource for evaluation")

	m.eventSourceJobQueue[eventSourceName] = true
}

// EvaluateReloader queues a Reloader instance for evaluation
func (m *manager) EvaluateReloader(reloaderName string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	logger := m.log
	logger = logger.WithValues("reloader", reloaderName)

	logger.V(logs.LogDebug).Info("queue reloader for evaluation")

	m.reloaderJobQueue[fmt.Sprintf("%s:%s", libsveltosv1beta1.ReloaderKind, reloaderName)] = true
}

// EvaluateConfigMap queues a ConfigMap instance for evaluation
func (m *manager) EvaluateConfigMap(configMapNamespace, configMapName string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	logger := m.log
	logger = logger.WithValues("configMap", fmt.Sprintf("%s/%s", configMapNamespace, configMapName))

	logger.V(logs.LogDebug).Info("queue configMap for evaluation")

	m.reloaderJobQueue[fmt.Sprintf("ConfigMap:%s/%s", configMapNamespace, configMapName)] = true
}

// EvaluateSecret queues a Secret instance for evaluation
func (m *manager) EvaluateSecret(secretNamespace, secretName string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	logger := m.log
	logger = logger.WithValues("secret", fmt.Sprintf("%s/%s", secretNamespace, secretName))

	logger.V(logs.LogDebug).Info("queue secret for evaluation")

	m.reloaderJobQueue[fmt.Sprintf("Secret:%s/%s", secretNamespace, secretName)] = true
}

// If there is any classifier/healthCheck using this GVK, restart agent
// On restart, agent will be able to start a watcher (a watcher
// cannot be started on api-resources not present in the cluster)
func restartIfNeeded(gvk *schema.GroupVersionKind) {
	manager := GetManager()
	if manager == nil {
		return
	}

	logger := textlogger.NewLogger(textlogger.NewConfig(textlogger.Verbosity(1)))
	logger.V(logs.LogDebug).Info(fmt.Sprintf("react to CustomResourceDefinition %s change",
		gvk.String()))

	for i := range manager.unknownResourcesToWatch {
		tmpGVK := manager.unknownResourcesToWatch[i]
		if reflect.DeepEqual(*gvk, tmpGVK) {
			if killErr := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); killErr != nil {
				panic("kill -TERM failed")
			}
		}
	}
}

func (m *manager) getKubeconfig(ctx context.Context) ([]byte, error) {
	secret := &corev1.Secret{}
	key := client.ObjectKey{
		Namespace: libsveltosv1beta1.ClassifierSecretNamespace,
		Name:      libsveltosv1beta1.ClassifierSecretName,
	}

	if err := m.Get(ctx, key, secret); err != nil {
		return nil, errors.Wrap(err,
			fmt.Sprintf("Failed to get secret %s", key))
	}

	for _, contents := range secret.Data {
		return contents, nil
	}

	return nil, nil
}

// getServiceAccountInfo returns the namespace/name of the ServiceAccount
// representing the admin that created this object
func (m *manager) getServiceAccountInfo(obj client.Object) (namespace, name string) {
	labels := obj.GetLabels()
	if labels == nil {
		return "", ""
	}

	return labels[libsveltosv1beta1.ServiceAccountNamespaceLabel],
		labels[libsveltosv1beta1.ServiceAccountNameLabel]
}

func (m *manager) storeVersionForCompatibilityChecks(ctx context.Context, version string, wg *sync.WaitGroup) {
	wg.Wait() // Wait for sveltos-agent to evaluate classifers, healthChecks, eventoSources and reloaders once

	m.log.V(logs.LogInfo).Info(fmt.Sprintf("store version %s for compatibility checks", version))
	for {
		err := sveltos_upgrade.StoreSveltosAgentVersion(ctx, m.Client, version)
		if err == nil {
			break
		}
		m.log.V(logs.LogInfo).Info(fmt.Sprintf("failed to store version %s for compatibility checks: %v", version, err))
		time.Sleep(time.Second)
	}
}
