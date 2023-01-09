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

package classification

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/cache"

	"github.com/go-logr/logr"
	libsveltosv1alpha1 "github.com/projectsveltos/libsveltos/api/v1alpha1"
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
					continue
				}
				copy(m.resourcesToWatch, tmpResourceToWatch)
			}
			m.mu.Unlock()
		}

		// Sleep before next evaluation
		time.Sleep(m.interval)
	}
}

func (m *manager) buildList(ctx context.Context) (map[schema.GroupVersionKind]bool, error) {
	classifiers := &libsveltosv1alpha1.ClassifierList{}
	err := m.List(ctx, classifiers)
	if err != nil {
		return nil, err
	}

	resources := make(map[schema.GroupVersionKind]bool)

	for i := range classifiers.Items {
		classifier := &classifiers.Items[i]
		if !classifier.DeletionTimestamp.IsZero() {
			continue
		}
		resources = m.addGVKsForClassifier(classifier, resources)
	}

	return resources, nil
}

func (m *manager) addGVKsForClassifier(classifier *libsveltosv1alpha1.Classifier,
	resources map[schema.GroupVersionKind]bool) map[schema.GroupVersionKind]bool {

	for i := range classifier.Spec.DeployedResourceConstraints {
		resource := &classifier.Spec.DeployedResourceConstraints[i]
		gvk := schema.GroupVersionKind{
			Group:   resource.Group,
			Kind:    resource.Kind,
			Version: resource.Version,
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
			err = m.startWatcher(ctx, gvk, m.react)
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
			//nolint: gomnd // groupVersion is group/version
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

func (m *manager) startWatcher(ctx context.Context, gvk *schema.GroupVersionKind, react ReactToNotification) error {
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
	go m.runInformer(watcherCtx.Done(), dcinformer.Informer(), gvk, react, logger)
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
			react(gvk)
		},
		DeleteFunc: func(obj interface{}) {
			logger.V(logsettings.LogDebug).Info("got delete notification")
			react(gvk)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			logger.V(logsettings.LogDebug).Info("got update notification")
			react(gvk)
		},
	}
	s.AddEventHandler(handlers)
	s.Run(stopCh)
}

func remove(slice []schema.GroupVersionKind, s int) []schema.GroupVersionKind {
	return append(slice[:s], slice[s+1:]...)
}
