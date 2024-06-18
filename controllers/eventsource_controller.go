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

import (
	"context"
	"sync"

	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	libsveltosv1beta1 "github.com/projectsveltos/libsveltos/api/v1beta1"
	logs "github.com/projectsveltos/libsveltos/lib/logsettings"
	libsveltosset "github.com/projectsveltos/libsveltos/lib/set"
	"github.com/projectsveltos/sveltos-agent/pkg/evaluation"
	"github.com/projectsveltos/sveltos-agent/pkg/scope"
	"github.com/projectsveltos/sveltos-agent/pkg/utils"
)

// EventSourceReconciler reconciles a EventSource object
type EventSourceReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	RunMode          Mode
	ClusterNamespace string
	ClusterName      string
	ClusterType      libsveltosv1beta1.ClusterType
	// Used to update internal maps and sets
	Mux sync.RWMutex
	// key: GVK, Value: list of EventSources based on that GVK
	GVKEventSources map[schema.GroupVersionKind]*libsveltosset.Set
}

// +kubebuilder:rbac:groups=lib.projectsveltos.io,resources=eventsources,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=lib.projectsveltos.io,resources=eventsources/finalizers,verbs=update
// +kubebuilder:rbac:groups=lib.projectsveltos.io,resources=eventreports,verbs=get;list;create;update;delete;patch
// +kubebuilder:rbac:groups=lib.projectsveltos.io,resources=eventreports/status,verbs=get;update;patch

func (r *EventSourceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (_ ctrl.Result, reterr error) {
	logger := ctrl.LoggerFrom(ctx)
	logger.V(logs.LogInfo).Info("Reconciling")

	// Fecth the EventSource instance
	eventSource := &libsveltosv1beta1.EventSource{}
	err := r.Get(ctx, req.NamespacedName, eventSource)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}
		logger.Error(err, "Failed to fetch EventSource")
		return reconcile.Result{}, errors.Wrapf(
			err,
			"Failed to fetch EventSource %s",
			req.NamespacedName,
		)
	}

	logger.V(logs.LogDebug).Info("request to re-evaluate resource to watch")
	manager := evaluation.GetManager()
	manager.ReEvaluateResourceToWatch()

	eventSourceScope, err := scope.NewEventSourceScope(scope.EventSourceScopeParams{
		Client:      r.Client,
		Logger:      logger,
		EventSource: eventSource,
	})
	if err != nil {
		logger.Error(err, "Failed to create eventSourceScope")
		return reconcile.Result{}, errors.Wrapf(
			err,
			"unable to create eventSource scope for %s",
			req.NamespacedName,
		)
	}

	// Always close the scope when exiting this function so we can persist any EventSource
	// changes.
	defer func() {
		if err := eventSourceScope.Close(ctx); err != nil {
			reterr = err
		}
	}()

	// Handle deleted eventSource
	if !eventSource.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, eventSourceScope, logger), nil
	}

	// Handle non-deleted eventSource
	return ctrl.Result{}, r.reconcileNormal(ctx, eventSourceScope, logger)
}

func (r *EventSourceReconciler) reconcileDelete(ctx context.Context,
	eventSourceScope *scope.EventSourceScope,
	logger logr.Logger,
) reconcile.Result {

	logger.V(logs.LogDebug).Info("reconcile delete")

	logger.V(logs.LogDebug).Info("remove eventSource from maps")
	r.cleanMaps(eventSourceScope.EventSource)

	// Queue EventSource for evaluation
	manager := evaluation.GetManager()
	manager.EvaluateEventSource(eventSourceScope.Name())

	canRemove, err := r.canRemoveFinalizer(ctx, eventSourceScope)
	if err != nil {
		return reconcile.Result{Requeue: true, RequeueAfter: deleteRequeueAfter}
	}

	if canRemove {
		if controllerutil.ContainsFinalizer(eventSourceScope.EventSource, libsveltosv1beta1.EventSourceFinalizer) {
			controllerutil.RemoveFinalizer(eventSourceScope.EventSource, libsveltosv1beta1.EventSourceFinalizer)
		}
	} else {
		return reconcile.Result{Requeue: true, RequeueAfter: deleteRequeueAfter}
	}

	logger.V(logs.LogInfo).Info("reconciliation succeeded")
	return reconcile.Result{}
}

func (r *EventSourceReconciler) reconcileNormal(ctx context.Context,
	eventSourceScope *scope.EventSourceScope,
	logger logr.Logger,
) error {

	logger.V(logs.LogDebug).Info("reconcile")

	if !controllerutil.ContainsFinalizer(eventSourceScope.EventSource, libsveltosv1beta1.EventSourceFinalizer) {
		if err := addFinalizer(ctx, r.Client, eventSourceScope.EventSource, libsveltosv1beta1.EventSourceFinalizer,
			logger); err != nil {
			logger.V(logs.LogDebug).Info("failed to update finalizer")
			return err
		}
	}

	logger.V(logs.LogDebug).Info("update maps")
	r.updateMaps(eventSourceScope.EventSource)

	// Queue EventSource for evaluation
	manager := evaluation.GetManager()
	manager.EvaluateEventSource(eventSourceScope.Name())

	logger.V(logs.LogInfo).Info("reconciliation succeeded")
	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *EventSourceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	_, err := ctrl.NewControllerManagedBy(mgr).
		For(&libsveltosv1beta1.EventSource{}).
		Build(r)
	if err != nil {
		return errors.Wrap(err, "error creating controller")
	}

	evaluation.GetManager().RegisterEventSourceMethod(r.react)

	return nil
}

func (r *EventSourceReconciler) cleanMaps(eventSource *libsveltosv1beta1.EventSource) {
	r.Mux.Lock()
	defer r.Mux.Unlock()

	policyRef := getKeyFromObject(r.Scheme, eventSource)

	for i := range r.GVKEventSources {
		r.GVKEventSources[i].Erase(policyRef)
	}
}

func (r *EventSourceReconciler) updateMaps(eventSource *libsveltosv1beta1.EventSource) {
	r.Mux.Lock()
	defer r.Mux.Unlock()

	for i := range eventSource.Spec.ResourceSelectors {
		rs := &eventSource.Spec.ResourceSelectors[i]
		gvk := schema.GroupVersionKind{
			Group:   rs.Group,
			Version: rs.Version,
			Kind:    rs.Kind,
		}

		policyRef := getKeyFromObject(r.Scheme, eventSource)

		_, ok := r.GVKEventSources[gvk]
		if !ok {
			r.GVKEventSources[gvk] = &libsveltosset.Set{}
		}
		r.GVKEventSources[gvk].Insert(policyRef)
	}
}

// react gets called when an instance of passed in gvk has been modified.
// This method queues all EventSource currently using that gvk to be evaluated.
func (r *EventSourceReconciler) react(gvk *schema.GroupVersionKind) {
	r.Mux.RLock()
	defer r.Mux.RUnlock()

	if gvk == nil {
		return
	}

	manager := evaluation.GetManager()

	if v, ok := r.GVKEventSources[*gvk]; ok {
		eventSources := v.Items()
		for i := range eventSources {
			eventSource := eventSources[i]
			manager.EvaluateEventSource(eventSource.Name)
		}
	}
}

// canRemoveFinalizer returns true if EventSource can be removed.
// EventSource can be removed only if corresponding EventReport is
// either not found or marked as deleted and Status is set to ReportProcessed.
func (r *EventSourceReconciler) canRemoveFinalizer(ctx context.Context,
	eventSourceScope *scope.EventSourceScope) (bool, error) {

	eventReport := &libsveltosv1beta1.EventReport{}
	err := r.Get(ctx,
		types.NamespacedName{Namespace: utils.ReportNamespace, Name: eventSourceScope.Name()}, eventReport)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	}

	if eventReport.DeletionTimestamp.IsZero() ||
		eventReport.Status.Phase == nil ||
		*eventReport.Status.Phase != libsveltosv1beta1.ReportProcessed {

		return false, nil
	}

	return true, nil
}
