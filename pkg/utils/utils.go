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
	"k8s.io/apimachinery/pkg/version"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"
)

const (
	// Namespace where reports will be generated
	ReportNamespace = "projectsveltos"
)

func GetKubernetesVersion(ctx context.Context, cfg *rest.Config, logger logr.Logger) (string, error) {
	discClient, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		logger.V(logs.LogInfo).Info(fmt.Sprintf("failed to get discovery client: %v", err))
		return "", err
	}

	var k8sVersion *version.Info
	k8sVersion, err = discClient.ServerVersion()
	if err != nil {
		logger.V(logs.LogInfo).Info(fmt.Sprintf("failed to get version from discovery client: %v", err))
		return "", err
	}

	logger.V(logs.LogDebug).Info(fmt.Sprintf("cluster version: %s", k8sVersion.String()))
	return k8sVersion.String(), nil
}
