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

// EventSourceScopeParams defines the input parameters used to create a new EventSource Scope.
type EventSourceScopeParams struct {
	Client      client.Client
	Logger      logr.Logger
	EventSource *libsveltosv1alpha1.EventSource
}

// NewEventSourceScope creates a new EventSource Scope from the supplied parameters.
// This is meant to be called for each reconcile iteration.
func NewEventSourceScope(params EventSourceScopeParams) (*EventSourceScope, error) {
	if params.Client == nil {
		return nil, errors.New("client is required when creating a EventSourceScope")
	}
	if params.EventSource == nil {
		return nil, errors.New("failed to generate new scope from nil EventSource")
	}

	helper, err := patch.NewHelper(params.EventSource, params.Client)
	if err != nil {
		return nil, errors.Wrap(err, "failed to init patch helper")
	}
	return &EventSourceScope{
		Logger:      params.Logger,
		client:      params.Client,
		EventSource: params.EventSource,
		patchHelper: helper,
	}, nil
}

// EventSourceScope defines the basic context for an actuator to operate upon.
type EventSourceScope struct {
	logr.Logger
	client      client.Client
	patchHelper *patch.Helper
	EventSource *libsveltosv1alpha1.EventSource
}

// PatchObject persists the feature configuration and status.
func (s *EventSourceScope) PatchObject(ctx context.Context) error {
	return s.patchHelper.Patch(
		ctx,
		s.EventSource,
	)
}

// Close closes the current scope persisting the EventSource configuration and status.
func (s *EventSourceScope) Close(ctx context.Context) error {
	return s.PatchObject(ctx)
}

// Name returns the EventSource name.
func (s *EventSourceScope) Name() string {
	return s.EventSource.Name
}
