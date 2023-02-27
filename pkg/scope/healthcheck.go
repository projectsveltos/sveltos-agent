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

package scope

import (
	"context"

	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	"sigs.k8s.io/cluster-api/util/patch"
	"sigs.k8s.io/controller-runtime/pkg/client"

	libsveltosv1alpha1 "github.com/projectsveltos/libsveltos/api/v1alpha1"
)

// HealthCheckScopeParams defines the input parameters used to create a new HealthCheck Scope.
type HealthCheckScopeParams struct {
	Client      client.Client
	Logger      logr.Logger
	HealthCheck *libsveltosv1alpha1.HealthCheck
}

// NewHealthCheckScope creates a new HealthCheck Scope from the supplied parameters.
// This is meant to be called for each reconcile iteration.
func NewHealthCheckScope(params HealthCheckScopeParams) (*HealthCheckScope, error) {
	if params.Client == nil {
		return nil, errors.New("client is required when creating a HealthCheckScope")
	}
	if params.HealthCheck == nil {
		return nil, errors.New("failed to generate new scope from nil HealthCheck")
	}

	helper, err := patch.NewHelper(params.HealthCheck, params.Client)
	if err != nil {
		return nil, errors.Wrap(err, "failed to init patch helper")
	}
	return &HealthCheckScope{
		Logger:      params.Logger,
		client:      params.Client,
		HealthCheck: params.HealthCheck,
		patchHelper: helper,
	}, nil
}

// HealthCheckScope defines the basic context for an actuator to operate upon.
type HealthCheckScope struct {
	logr.Logger
	client      client.Client
	patchHelper *patch.Helper
	HealthCheck *libsveltosv1alpha1.HealthCheck
}

// PatchObject persists the feature configuration and status.
func (s *HealthCheckScope) PatchObject(ctx context.Context) error {
	return s.patchHelper.Patch(
		ctx,
		s.HealthCheck,
	)
}

// Close closes the current scope persisting the HealthCheck configuration and status.
func (s *HealthCheckScope) Close(ctx context.Context) error {
	return s.PatchObject(ctx)
}

// Name returns the HealthCheck name.
func (s *HealthCheckScope) Name() string {
	return s.HealthCheck.Name
}
