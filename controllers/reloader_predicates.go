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

package controllers

import (
	"reflect"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	logs "github.com/projectsveltos/libsveltos/lib/logsettings"
	"github.com/projectsveltos/sveltos-agent/pkg/evaluation"
)

// ConfigMapPredicates predicates for ConfigMaps.
// Reloader contains list of Deployments/StatefulSets/DaemonSets instances deployed
// by Sveltos which needs to be reloaded when mounted ConfigMap changes.
// ReloaderReconciler starts a watcher on ConfigMap. When ConfigMap Data/BinaryData changes
// no reloader instance is queued for reconciliation. Instead ConfigMap itself is queued to
// be examined by evaluation manager. Manager has info on which Deployments/StatefulSets/DaemonSets
// are currently mounting this ConfigMap.
func ConfigMapPredicates(logger logr.Logger) predicate.Funcs {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			manager := evaluation.GetManager()

			newConfigMap := e.ObjectNew.(*corev1.ConfigMap)
			oldConfigMap := e.ObjectOld.(*corev1.ConfigMap)
			log := logger.WithValues("predicate", "updateEvent",
				"configmap", newConfigMap.Name,
			)

			if oldConfigMap == nil {
				log.V(logs.LogVerbose).Info("Old ConfigMap is nil. Re-evaluate configMap.")
				manager.EvaluateConfigMap(newConfigMap.Namespace, newConfigMap.Name)
				return false
			}

			if !reflect.DeepEqual(oldConfigMap.Data, newConfigMap.Data) {
				log.V(logs.LogVerbose).Info("ConfigMap Data changed. Re-evaluate configMap.")
				manager.EvaluateConfigMap(newConfigMap.Namespace, newConfigMap.Name)
			}

			if !reflect.DeepEqual(oldConfigMap.BinaryData, newConfigMap.BinaryData) {
				log.V(logs.LogVerbose).Info("ConfigMap BinaryData changed. Re-evaluate configMap.")
				manager.EvaluateConfigMap(newConfigMap.Namespace, newConfigMap.Name)
			}

			return false
		},
		CreateFunc: func(e event.CreateEvent) bool {
			return false
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			return false
		},
		GenericFunc: func(e event.GenericEvent) bool {
			return false
		},
	}
}

// SecretMapPredicates predicates for Secrets
// Reloader contains list of Deployments/StatefulSets/DaemonSets instances deployed
// by Sveltos which needs to be reloaded when mounted Secret changes.
// ReloaderReconciler starts a watcher on Secret. When Secret Data/StringData changes
// no reloader instance is queued for reconciliation. Instead Secret itself is queued to
// be examined by evaluation manager. Manager has info on which Deployments/StatefulSets/DaemonSets
// are currently mounting this Secret.
func SecretPredicates(logger logr.Logger) predicate.Funcs {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			manager := evaluation.GetManager()

			newSecret := e.ObjectNew.(*corev1.Secret)
			oldSecret := e.ObjectOld.(*corev1.Secret)
			log := logger.WithValues("predicate", "updateEvent",
				"secret", newSecret.Name,
			)

			if oldSecret == nil {
				log.V(logs.LogVerbose).Info("Old Secret is nil. Re-evaluate secret.")
				manager.EvaluateConfigMap(newSecret.Namespace, newSecret.Name)
				return false
			}

			if !reflect.DeepEqual(oldSecret.Data, newSecret.Data) {
				log.V(logs.LogVerbose).Info("Secret Data changed. Re-evaluate secret.")
				manager.EvaluateConfigMap(newSecret.Namespace, newSecret.Name)
			}

			if !reflect.DeepEqual(oldSecret.StringData, newSecret.StringData) {
				log.V(logs.LogVerbose).Info("Secret StringData changed. Re-evaluate secret.")
				manager.EvaluateConfigMap(newSecret.Namespace, newSecret.Name)
			}

			return false
		},
		CreateFunc: func(e event.CreateEvent) bool {
			return false
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			return false
		},
		GenericFunc: func(e event.GenericEvent) bool {
			return false
		},
	}
}
