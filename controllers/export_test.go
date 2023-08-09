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

package controllers

var (
	FindClassifierUsingKubernetesVersion = (*NodeReconciler).findClassifierUsingKubernetesVersion
)

func GetKubernetesVersion(r *NodeReconciler) string {
	return r.kubernetesVersion
}

func SetKubernetesVersion(r *NodeReconciler, version string) {
	r.kubernetesVersion = version
}

var (
	ClassifierUpdateMaps      = (*ClassifierReconciler).updateMaps
	ClassifierReconcileDelete = (*ClassifierReconciler).reconcileDelete

	HealthCheckUpdateMaps      = (*HealthCheckReconciler).updateMaps
	HealthCheckReconcileDelete = (*HealthCheckReconciler).reconcileDelete

	EventSourceUpdateMaps      = (*EventSourceReconciler).updateMaps
	EventSourceReconcileDelete = (*EventSourceReconciler).reconcileDelete

	ReloaderUpdateMaps      = (*ReloaderReconciler).updateMaps
	ReloaderReconcileDelete = (*ReloaderReconciler).reconcileDelete
)

var (
	GetKeyFromObject = getKeyFromObject
)
