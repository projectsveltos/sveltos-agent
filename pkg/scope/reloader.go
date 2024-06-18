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

	libsveltosv1beta1 "github.com/projectsveltos/libsveltos/api/v1beta1"
)

// ReloaderScopeParams defines the input parameters used to create a new Reloader Scope.
type ReloaderScopeParams struct {
	Client   client.Client
	Logger   logr.Logger
	Reloader *libsveltosv1beta1.Reloader
}

// NewReloaderScope creates a new Reloader Scope from the supplied parameters.
// This is meant to be called for each reconcile iteration.
func NewReloaderScope(params ReloaderScopeParams) (*ReloaderScope, error) {
	if params.Client == nil {
		return nil, errors.New("client is required when creating a ReloaderScope")
	}
	if params.Reloader == nil {
		return nil, errors.New("failed to generate new scope from nil Reloader")
	}

	helper, err := patch.NewHelper(params.Reloader, params.Client)
	if err != nil {
		return nil, errors.Wrap(err, "failed to init patch helper")
	}
	return &ReloaderScope{
		Logger:      params.Logger,
		client:      params.Client,
		Reloader:    params.Reloader,
		patchHelper: helper,
	}, nil
}

// ReloaderScope defines the basic context for an actuator to operate upon.
type ReloaderScope struct {
	logr.Logger
	client      client.Client
	patchHelper *patch.Helper
	Reloader    *libsveltosv1beta1.Reloader
}

// PatchObject persists the feature configuration and status.
func (s *ReloaderScope) PatchObject(ctx context.Context) error {
	return s.patchHelper.Patch(
		ctx,
		s.Reloader,
	)
}

// Close closes the current scope persisting the Reloader configuration and status.
func (s *ReloaderScope) Close(ctx context.Context) error {
	return s.PatchObject(ctx)
}

// Name returns the Reloader name.
func (s *ReloaderScope) Name() string {
	return s.Reloader.Name
}
