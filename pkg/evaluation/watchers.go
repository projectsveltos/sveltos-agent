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
	"sort"
	"strings"
	"sync/atomic"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/cache"

	"github.com/go-logr/logr"

	libsveltosv1beta1 "github.com/projectsveltos/libsveltos/api/v1beta1"
	"github.com/projectsveltos/libsveltos/lib/logsettings"
)

func (m *manager) buildResourceToWatch(ctx context.Context) {
	for {
		request := atomic.LoadUint32(&m.rebuildResourceToWatch)
		if request != 0 {
			atomic.StoreUint32(&m.rebuildResourceToWatch, 0)
			gvksMap, err := m.buildList(ctx)
			if err != nil {
				m.log.Error(err, "failed to rebuild list of resources to watch")
				atomic.StoreUint32(&m.rebuildResourceToWatch, 1)
				continue
			}

			tmpResourceToWatch := m.buildSortedList(gvksMap)

			m.mu.Lock()
			if reflect.DeepEqual(tmpResourceToWatch, m.resourcesToWatch) {
				m.log.V(logsettings.LogInfo).Info("list of resources to watch has not changed")
			} else {
				m.log.V(logsettings.LogInfo).Info("list of resources to watch has changed")
				// Updates watchers
				err = m.updateWatchers(ctx, tmpResourceToWatch)
				if err != nil {
					m.log.Error(err, "failed to update watchers")
					atomic.StoreUint32(&m.rebuildResourceToWatch, 1)
				} else {
					copy(m.resourcesToWatch, tmpResourceToWatch)
				}
			}
			m.mu.Unlock()
		}

		// Sleep before next evaluation
		time.Sleep(m.interval)
	}
}

func (m *manager) buildList(ctx context.Context) (map[schema.GroupVersionKind]bool, error) {
	resources, err := m.buildListForClassifiers(ctx)
	if err != nil {
		return nil, err
	}

	tmpResources, err := m.buildListForHealthChecks(ctx)
	if err != nil {
		return nil, err
	}
	for k := range tmpResources {
		resources[k] = true
	}

	tmpResources, err = m.buildListForEventSources(ctx)
	if err != nil {
		return nil, err
	}
	for k := range tmpResources {
		resources[k] = true
	}

	tmpResources, err = m.buildListForReloaders(ctx)
	if err != nil {
		return nil, err
	}
	for k := range tmpResources {
		resources[k] = true
	}

	// Always watch for Secrets since this is where NATS configuration is
	resources[*getSecretGVK()] = true

	return resources, nil
}

func (m *manager) buildListForClassifiers(ctx context.Context) (map[schema.GroupVersionKind]bool, error) {
	resources := make(map[schema.GroupVersionKind]bool)

	classifiers := &libsveltosv1beta1.ClassifierList{}
	err := m.List(ctx, classifiers)
	if err != nil {
		if meta.IsNoMatchError(err) {
			return resources, nil
		}
		return nil, err
	}

	for i := range classifiers.Items {
		classifier := &classifiers.Items[i]
		if !classifier.DeletionTimestamp.IsZero() {
			continue
		}
		resources = m.addGVKsForClassifier(classifier, resources)
	}

	return resources, nil
}

func (m *manager) buildListForHealthChecks(ctx context.Context) (map[schema.GroupVersionKind]bool, error) {
	resources := make(map[schema.GroupVersionKind]bool)

	healthChecks := &libsveltosv1beta1.HealthCheckList{}
	err := m.List(ctx, healthChecks)
	if err != nil {
		if meta.IsNoMatchError(err) {
			return resources, nil
		}
		return nil, err
	}

	for i := range healthChecks.Items {
		healthCheck := &healthChecks.Items[i]
		if !healthCheck.DeletionTimestamp.IsZero() {
			continue
		}
		resources = m.addGVKsForHealthCheck(healthCheck, resources)
	}

	return resources, nil
}

func (m *manager) buildListForEventSources(ctx context.Context) (map[schema.GroupVersionKind]bool, error) {
	resources := make(map[schema.GroupVersionKind]bool)

	eventSources := &libsveltosv1beta1.EventSourceList{}
	err := m.List(ctx, eventSources)
	if err != nil {
		if meta.IsNoMatchError(err) {
			return resources, nil
		}
		return nil, err
	}

	for i := range eventSources.Items {
		eventSource := &eventSources.Items[i]
		if !eventSource.DeletionTimestamp.IsZero() {
			continue
		}
		resources = m.addGVKsForEventSource(eventSource, resources)
	}

	return resources, nil
}

func (m *manager) buildListForReloaders(ctx context.Context) (map[schema.GroupVersionKind]bool, error) {
	resources := make(map[schema.GroupVersionKind]bool)

	reloaders := &libsveltosv1beta1.ReloaderList{}
	err := m.List(ctx, reloaders)
	if err != nil {
		if meta.IsNoMatchError(err) {
			return resources, nil
		}
		return nil, err
	}

	for i := range reloaders.Items {
		reloader := &reloaders.Items[i]
		if !reloader.DeletionTimestamp.IsZero() {
			continue
		}
		resources = m.addGVKsForReloader(reloader, resources)
	}

	return resources, nil
}

func (m *manager) addGVKsForClassifier(classifier *libsveltosv1beta1.Classifier,
	resources map[schema.GroupVersionKind]bool) map[schema.GroupVersionKind]bool {

	if classifier.Spec.DeployedResourceConstraint == nil {
		return resources
	}

	for i := range classifier.Spec.DeployedResourceConstraint.ResourceSelectors {
		rs := &classifier.Spec.DeployedResourceConstraint.ResourceSelectors[i]
		gvk := schema.GroupVersionKind{
			Group:   rs.Group,
			Kind:    rs.Kind,
			Version: rs.Version,
		}
		resources[gvk] = true
	}

	return resources
}

func (m *manager) addGVKsForHealthCheck(healthCheck *libsveltosv1beta1.HealthCheck,
	resources map[schema.GroupVersionKind]bool) map[schema.GroupVersionKind]bool {

	for i := range healthCheck.Spec.ResourceSelectors {
		gvk := schema.GroupVersionKind{
			Group:   healthCheck.Spec.ResourceSelectors[i].Group,
			Kind:    healthCheck.Spec.ResourceSelectors[i].Kind,
			Version: healthCheck.Spec.ResourceSelectors[i].Version,
		}
		resources[gvk] = true
	}
	return resources
}

func (m *manager) addGVKsForEventSource(eventSource *libsveltosv1beta1.EventSource,
	resources map[schema.GroupVersionKind]bool) map[schema.GroupVersionKind]bool {

	for i := range eventSource.Spec.ResourceSelectors {
		gvk := schema.GroupVersionKind{
			Group:   eventSource.Spec.ResourceSelectors[i].Group,
			Kind:    eventSource.Spec.ResourceSelectors[i].Kind,
			Version: eventSource.Spec.ResourceSelectors[i].Version,
		}
		resources[gvk] = true
	}
	return resources
}

func (m *manager) addGVKsForReloader(reloader *libsveltosv1beta1.Reloader,
	resources map[schema.GroupVersionKind]bool) map[schema.GroupVersionKind]bool {

	for i := range reloader.Spec.ReloaderInfo {
		resource := &reloader.Spec.ReloaderInfo[i]
		gvk := schema.GroupVersionKind{
			// Deployment/StatefulSet/DaemonSet are all appsv1
			Group:   appsv1.SchemeGroupVersion.Group,
			Version: appsv1.SchemeGroupVersion.Group,
			Kind:    resource.Kind,
		}
		resources[gvk] = true
	}

	return resources
}

func (m *manager) buildSortedList(gvksMap map[schema.GroupVersionKind]bool) []schema.GroupVersionKind {
	gvks := make([]schema.GroupVersionKind, len(gvksMap))
	i := 0
	for k := range gvksMap {
		gvks[i] = k
		i++
	}

	// Sort kinds to get deterministic test order
	sort.Slice(gvks, func(i, j int) bool {
		if gvks[i].Group != gvks[j].Group {
			return gvks[i].Group < gvks[j].Group
		}
		if gvks[i].Version != gvks[j].Version {
			return gvks[i].Version < gvks[j].Version
		}
		if gvks[i].Kind != gvks[j].Kind {
			return gvks[i].Kind < gvks[j].Kind
		}
		return false
	})

	return gvks
}

func (m *manager) updateWatchers(ctx context.Context, resourceToWatch []schema.GroupVersionKind) error {
	m.log.V(logsettings.LogDebug).Info("update watchers")

	apiResources, err := m.getInstalledResources()
	if err != nil {
		m.log.Error(err, "failed to get api-resources")
		return err
	}

	currentResourcesToWatch := make(map[schema.GroupVersionKind]bool)
	for i := range resourceToWatch {
		gvk := &resourceToWatch[i]
		currentResourcesToWatch[*gvk] = true
		if m.gvkInstalled(gvk, apiResources) {
			m.log.V(logsettings.LogDebug).Info(fmt.Sprintf("start watcher for %s", gvk.String()))
			// Start watcher and invoke the registered method to react when an instance of this
			// gvk is added/deleted/modified
			err = m.startWatcher(ctx, gvk)
			if err != nil {
				return err
			}
		} else {
			m.log.V(logsettings.LogDebug).Info(fmt.Sprintf("%s not installed yet", gvk.String()))
			m.unknownResourcesToWatch = append(m.unknownResourcesToWatch, *gvk)
		}
	}

	// Cancel all watchers we are not interested in anymore
	for i := range m.resourcesToWatch {
		gvk := &m.resourcesToWatch[i]
		if _, ok := currentResourcesToWatch[*gvk]; !ok {
			m.log.V(logsettings.LogInfo).Info(fmt.Sprintf("close watcher for %s", gvk.String()))
			cancel := m.watchers[*gvk]
			cancel()
			m.resourcesToWatch = remove(m.resourcesToWatch, i)
		}
	}

	return nil
}

// getInstalledResources fetches all installed api resources
func (m *manager) getInstalledResources() (map[schema.GroupVersionKind]bool, error) {
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(m.config)
	if err != nil {
		return nil, err
	}

	_, resourceList, err := discoveryClient.ServerGroupsAndResources()
	if err != nil {
		return nil, err
	}

	gvks := make(map[schema.GroupVersionKind]bool)

	for i := range resourceList {
		for j := range resourceList[i].APIResources {
			resource := resourceList[i].APIResources[j]
			// resource.Group and resource.Version are empty
			// see: https://github.com/kubernetes/client-go/issues/1071
			// get those from resourceList[i].GroupVersion
			group := resource.Group
			version := resource.Version
			groupVersionInfo := strings.Split(resourceList[i].GroupVersion, "/")
			//nolint: mnd // groupVersion is group/version
			if len(groupVersionInfo) == 2 {
				group = groupVersionInfo[0]
				version = groupVersionInfo[1]
			} else if len(groupVersionInfo) == 1 {
				group = ""
				version = groupVersionInfo[0]
			}
			gvk := schema.GroupVersionKind{
				Group:   group,
				Version: version,
				Kind:    resource.Kind,
			}

			gvks[gvk] = true
		}
	}

	return gvks, nil
}

func (m *manager) gvkInstalled(gvk *schema.GroupVersionKind,
	apiResources map[schema.GroupVersionKind]bool) bool {

	if gvk == nil {
		return false
	}

	return apiResources[*gvk]
}

func (m *manager) react(gvk *schema.GroupVersionKind, obj interface{}) {
	if m.reactClassifier != nil {
		m.reactClassifier(gvk, obj)
	}

	if m.reactHealthCheck != nil {
		m.reactHealthCheck(gvk, obj)
	}

	if m.reactEventSource != nil {
		m.reactEventSource(gvk, obj)
	}

	if m.reactReloader != nil {
		m.reactReloader(gvk, obj)
	}

	m.reactToNATS(gvk, obj)
}

func (m *manager) startWatcher(ctx context.Context, gvk *schema.GroupVersionKind) error {
	logger := m.log.WithValues("gvk", gvk.String())

	if _, ok := m.watchers[*gvk]; ok {
		logger.V(logsettings.LogDebug).Info("watcher already present")
		return nil
	}

	logger.V(logsettings.LogInfo).Info("start watcher")
	// dynamic informer needs to be told which type to watch
	dcinformer, err := m.getDynamicInformer(gvk)
	if err != nil {
		logger.Error(err, "Failed to get informer")
		return err
	}

	watcherCtx, cancel := context.WithCancel(ctx)
	m.watchers[*gvk] = cancel
	go m.runInformer(watcherCtx.Done(), dcinformer.Informer(), gvk, m.react, logger)
	return nil
}

func (m *manager) getDynamicInformer(gvk *schema.GroupVersionKind) (informers.GenericInformer, error) {
	// Grab a dynamic interface that we can create informers from
	d, err := dynamic.NewForConfig(m.config)
	if err != nil {
		return nil, err
	}
	// Create a factory object that can generate informers for resource types
	factory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(
		d,
		0,
		corev1.NamespaceAll,
		nil,
	)

	dc := discovery.NewDiscoveryClientForConfigOrDie(m.config)
	groupResources, err := restmapper.GetAPIGroupResources(dc)
	if err != nil {
		return nil, err
	}
	mapper := restmapper.NewDiscoveryRESTMapper(groupResources)

	mapping, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		// getDynamicInformer is only called after verifying resource
		// is installed.
		return nil, err
	}

	resourceId := schema.GroupVersionResource{
		Group:    gvk.Group,
		Version:  gvk.Version,
		Resource: mapping.Resource.Resource,
	}

	informer := factory.ForResource(resourceId)
	return informer, nil
}

func (m *manager) runInformer(stopCh <-chan struct{}, s cache.SharedIndexInformer,
	gvk *schema.GroupVersionKind, react ReactToNotification, logger logr.Logger) {

	handlers := cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			logger.V(logsettings.LogDebug).Info("got add notification")
			react(gvk, obj)
		},
		DeleteFunc: func(obj interface{}) {
			logger.V(logsettings.LogDebug).Info("got delete notification")
			react(gvk, obj)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			logger.V(logsettings.LogDebug).Info("got update notification")
			react(gvk, newObj)
		},
	}
	_, err := s.AddEventHandler(handlers)
	if err != nil {
		panic(1)
	}
	s.Run(stopCh)
}

func remove(slice []schema.GroupVersionKind, s int) []schema.GroupVersionKind {
	return append(slice[:s], slice[s+1:]...)
}
