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

	libsveltosv1alpha1 "github.com/projectsveltos/libsveltos/api/v1alpha1"
	logs "github.com/projectsveltos/libsveltos/lib/logsettings"
	libsveltosset "github.com/projectsveltos/libsveltos/lib/set"
	"github.com/projectsveltos/sveltos-agent/pkg/evaluation"
	"github.com/projectsveltos/sveltos-agent/pkg/scope"
	"github.com/projectsveltos/sveltos-agent/pkg/utils"
)

// HealthCheckReconciler reconciles a HealthCheck object
type HealthCheckReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	RunMode          Mode
	ClusterNamespace string
	ClusterName      string
	ClusterType      libsveltosv1alpha1.ClusterType
	// Used to update internal maps and sets
	Mux sync.RWMutex
	// key: GVK, Value: list of HealthChecks based on that GVK
	GVKHealthChecks map[schema.GroupVersionKind]*libsveltosset.Set
}

//+kubebuilder:rbac:groups=lib.projectsveltos.io,resources=healthchecks,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups=lib.projectsveltos.io,resources=healthchecks/finalizers,verbs=update
//+kubebuilder:rbac:groups=lib.projectsveltos.io,resources=healthcheckreports,verbs=get;list;create;update;delete;patch
//+kubebuilder:rbac:groups=lib.projectsveltos.io,resources=healthcheckreports/status,verbs=get;update;patch

func (r *HealthCheckReconciler) Reconcile(ctx context.Context, req ctrl.Request) (_ ctrl.Result, reterr error) {
	logger := ctrl.LoggerFrom(ctx)
	logger.V(logs.LogInfo).Info("Reconciling")

	// Fecth the HealthCheck instance
	healthCheck := &libsveltosv1alpha1.HealthCheck{}
	err := r.Get(ctx, req.NamespacedName, healthCheck)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}
		logger.Error(err, "Failed to fetch HealthCheck")
		return reconcile.Result{}, errors.Wrapf(
			err,
			"Failed to fetch HealthCheck %s",
			req.NamespacedName,
		)
	}

	if shouldIgnore(healthCheck) {
		return reconcile.Result{}, nil
	}

	logger.V(logs.LogDebug).Info("request to re-evaluate resource to watch")
	manager := evaluation.GetManager()
	manager.ReEvaluateResourceToWatch()

	healthCheckScope, err := scope.NewHealthCheckScope(scope.HealthCheckScopeParams{
		Client:      r.Client,
		Logger:      logger,
		HealthCheck: healthCheck,
	})
	if err != nil {
		logger.Error(err, "Failed to create healthCheckScope")
		return reconcile.Result{}, errors.Wrapf(
			err,
			"unable to create healthCheck scope for %s",
			req.NamespacedName,
		)
	}

	// Always close the scope when exiting this function so we can persist any HealthCheck
	// changes.
	defer func() {
		if err := healthCheckScope.Close(ctx); err != nil {
			reterr = err
		}
	}()

	// Handle deleted healthCheck
	if !healthCheck.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, healthCheckScope, logger), nil
	}

	// Handle non-deleted healthCheck
	return ctrl.Result{}, r.reconcileNormal(ctx, healthCheckScope, logger)
}

func (r *HealthCheckReconciler) reconcileDelete(ctx context.Context,
	healthCheckScope *scope.HealthCheckScope,
	logger logr.Logger,
) reconcile.Result {

	logger.V(logs.LogDebug).Info("reconcile delete")

	logger.V(logs.LogDebug).Info("remove healthCheck from maps")
	r.cleanMaps(healthCheckScope.HealthCheck)

	// Queue HealthCheck for evaluation
	manager := evaluation.GetManager()
	manager.EvaluateHealthCheck(healthCheckScope.Name())

	canRemove, err := r.canRemoveFinalizer(ctx, healthCheckScope)
	if err != nil {
		return reconcile.Result{Requeue: true, RequeueAfter: deleteRequeueAfter}
	}

	if canRemove {
		if controllerutil.ContainsFinalizer(healthCheckScope.HealthCheck, libsveltosv1alpha1.HealthCheckFinalizer) {
			controllerutil.RemoveFinalizer(healthCheckScope.HealthCheck, libsveltosv1alpha1.HealthCheckFinalizer)
		}
	} else {
		return reconcile.Result{Requeue: true, RequeueAfter: deleteRequeueAfter}
	}

	logger.V(logs.LogInfo).Info("reconciliation succeeded")
	return reconcile.Result{}
}

func (r *HealthCheckReconciler) reconcileNormal(ctx context.Context,
	healthCheckScope *scope.HealthCheckScope,
	logger logr.Logger,
) error {

	logger.V(logs.LogDebug).Info("reconcile")

	if !controllerutil.ContainsFinalizer(healthCheckScope.HealthCheck, libsveltosv1alpha1.HealthCheckFinalizer) {
		if err := addFinalizer(ctx, r.Client, healthCheckScope.HealthCheck, libsveltosv1alpha1.HealthCheckFinalizer,
			logger); err != nil {
			logger.V(logs.LogDebug).Info("failed to update finalizer")
			return err
		}
	}

	logger.V(logs.LogDebug).Info("update maps")
	r.updateMaps(healthCheckScope.HealthCheck)

	// Queue HealthCheck for evaluation
	manager := evaluation.GetManager()
	manager.EvaluateHealthCheck(healthCheckScope.Name())

	logger.V(logs.LogInfo).Info("reconciliation succeeded")
	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *HealthCheckReconciler) SetupWithManager(mgr ctrl.Manager) error {
	_, err := ctrl.NewControllerManagedBy(mgr).
		For(&libsveltosv1alpha1.HealthCheck{}).
		Build(r)
	if err != nil {
		return errors.Wrap(err, "error creating controller")
	}

	evaluation.GetManager().RegisterHealthCheckMethod(r.react)

	return nil
}

func (r *HealthCheckReconciler) cleanMaps(healthCheck *libsveltosv1alpha1.HealthCheck) {
	r.Mux.Lock()
	defer r.Mux.Unlock()

	policyRef := getKeyFromObject(r.Scheme, healthCheck)

	for i := range r.GVKHealthChecks {
		r.GVKHealthChecks[i].Erase(policyRef)
	}
}

func (r *HealthCheckReconciler) updateMaps(healthCheck *libsveltosv1alpha1.HealthCheck) {
	policyRef := getKeyFromObject(r.Scheme, healthCheck)

	r.Mux.Lock()
	defer r.Mux.Unlock()

	for i := range healthCheck.Spec.ResourceSelectors {
		rs := &healthCheck.Spec.ResourceSelectors[i]
		gvk := schema.GroupVersionKind{
			Group:   rs.Group,
			Version: rs.Version,
			Kind:    rs.Kind,
		}

		_, ok := r.GVKHealthChecks[gvk]
		if !ok {
			r.GVKHealthChecks[gvk] = &libsveltosset.Set{}
		}
		r.GVKHealthChecks[gvk].Insert(policyRef)
	}
}

// react gets called when an instance of passed in gvk has been modified.
// This method queues all HealthCheck currently using that gvk to be evaluated.
func (r *HealthCheckReconciler) react(gvk *schema.GroupVersionKind) {
	r.Mux.RLock()
	defer r.Mux.RUnlock()

	if gvk == nil {
		return
	}

	manager := evaluation.GetManager()

	if v, ok := r.GVKHealthChecks[*gvk]; ok {
		healtchChecks := v.Items()
		for i := range healtchChecks {
			healtchCheck := healtchChecks[i]
			manager.EvaluateHealthCheck(healtchCheck.Name)
		}
	}
}

// canRemoveFinalizer returns true if HealthCheck can be removed.
// HealthCheck can be removed only if corresponding HealthCheckReport is
// either not found or marked as deleted and Status is set to ReportProcessed.
func (r *HealthCheckReconciler) canRemoveFinalizer(ctx context.Context,
	healthCheckScope *scope.HealthCheckScope) (bool, error) {

	healthCheckReport := &libsveltosv1alpha1.HealthCheckReport{}
	err := r.Get(ctx,
		types.NamespacedName{Namespace: utils.ReportNamespace, Name: healthCheckScope.Name()}, healthCheckReport)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	}

	if healthCheckReport.DeletionTimestamp.IsZero() ||
		healthCheckReport.Status.Phase == nil ||
		*healthCheckReport.Status.Phase != libsveltosv1alpha1.ReportProcessed {

		return false, nil
	}

	return true, nil
}
