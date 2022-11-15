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

package utils

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	logs "github.com/projectsveltos/libsveltos/lib/logsettings"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	controlPlaneLabel = "node-role.kubernetes.io/control-plane"
	// Namespace where reports will be generated
	ReportNamespace = "projectsveltos"
)

func IsControlPlaneNode(node *corev1.Node) bool {
	labels := node.Labels
	if labels == nil {
		return false
	}
	_, ok := labels[controlPlaneLabel]
	return ok
}

func GetKubernetesVersion(ctx context.Context, c client.Client, logger logr.Logger) (string, error) {
	nodeList := &corev1.NodeList{}

	err := c.List(ctx, nodeList)
	if err != nil {
		return "", err
	}

	logger.V(logs.LogDebug).Info(fmt.Sprintf("found %d nodes", len(nodeList.Items)))
	currentVersions := make(map[string]int)
	for i := range nodeList.Items {
		node := &nodeList.Items[i]
		if IsControlPlaneNode(node) {
			logger.V(logs.LogDebug).Info(fmt.Sprintf("node %s is control plane", node.Name))
			version := node.Status.NodeInfo.KubeletVersion
			logger.V(logs.LogDebug).Info(fmt.Sprintf("node %s is control plane. Version: %s",
				node.Name, version))
			if v, ok := currentVersions[version]; ok {
				currentVersions[version] = v + 1
			} else {
				currentVersions[version] = 1
			}
		}
	}

	versionCount := 0
	version := ""
	for k := range currentVersions {
		if currentVersions[k] > versionCount {
			version = k
			versionCount = currentVersions[k]
		}
	}

	logger.V(logs.LogDebug).Info(fmt.Sprintf("cluster version: %s", version))
	return version, nil
}
