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
	"time"

	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	libsveltosv1alpha1 "github.com/projectsveltos/libsveltos/api/v1alpha1"
	logs "github.com/projectsveltos/libsveltos/lib/logsettings"
	libsveltosset "github.com/projectsveltos/libsveltos/lib/set"
	"github.com/projectsveltos/sveltos-agent/pkg/evaluation"
	"github.com/projectsveltos/sveltos-agent/pkg/scope"
	"github.com/projectsveltos/sveltos-agent/pkg/utils"
)

const (
	// deleteRequeueAfter is how long to wait before checking again during healthCheck delete reconciliation
	deleteRequeueAfter = 20 * time.Second
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

	ReferenceMap   map[corev1.ObjectReference]*libsveltosset.Set // key: Referenced object; value: set of all HealthChecks referencing the resource
	HealthCheckMap map[types.NamespacedName]*libsveltosset.Set   // key: HealthCheck name; value: set of referenced resources
}

//+kubebuilder:rbac:groups=lib.projectsveltos.io,resources=healthchecks,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=lib.projectsveltos.io,resources=healthchecks/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=lib.projectsveltos.io,resources=healthchecks/finalizers,verbs=update
//+kubebuilder:rbac:groups=lib.projectsveltos.io,resources=healthcheckreports,verbs=get;list;create;update;delete;patch
//+kubebuilder:rbac:groups=lib.projectsveltos.io,resources=healthcheckreports/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=*,resources=*,verbs=get;list;watch

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
		return r.reconcileDelete(ctx, healthCheckScope, logger)
	}

	// Handle non-deleted healthCheck
	return r.reconcileNormal(ctx, healthCheckScope, logger)
}

func (r *HealthCheckReconciler) reconcileDelete(ctx context.Context,
	healthCheckScope *scope.HealthCheckScope,
	logger logr.Logger,
) (reconcile.Result, error) {

	logger.V(logs.LogDebug).Info("reconcile delete")

	logger.V(logs.LogDebug).Info("remove healthCheck from maps")
	r.cleanMaps(healthCheckScope.HealthCheck)

	// Queue HealthCheck for evaluation
	manager := evaluation.GetManager()
	manager.EvaluateHealthCheck(healthCheckScope.Name())

	canRemove, err := r.canRemoveFinalizer(ctx, healthCheckScope)
	if err != nil {
		return reconcile.Result{Requeue: true, RequeueAfter: deleteRequeueAfter}, nil
	}

	if canRemove {
		err = r.removeHealthCheckReportFinalizer(ctx, healthCheckScope)
		if err != nil {
			return reconcile.Result{Requeue: true, RequeueAfter: deleteRequeueAfter}, nil
		}

		if controllerutil.ContainsFinalizer(healthCheckScope.HealthCheck, libsveltosv1alpha1.HealthCheckFinalizer) {
			controllerutil.RemoveFinalizer(healthCheckScope.HealthCheck, libsveltosv1alpha1.HealthCheckFinalizer)
		}
	} else {
		return reconcile.Result{Requeue: true, RequeueAfter: deleteRequeueAfter}, nil
	}

	logger.V(logs.LogInfo).Info("reconciliation succeeded")
	return reconcile.Result{}, nil
}

func (r *HealthCheckReconciler) reconcileNormal(ctx context.Context,
	healthCheckScope *scope.HealthCheckScope,
	logger logr.Logger,
) (reconcile.Result, error) {

	logger.V(logs.LogDebug).Info("reconcile")

	if !controllerutil.ContainsFinalizer(healthCheckScope.HealthCheck, libsveltosv1alpha1.HealthCheckFinalizer) {
		if err := addFinalizer(ctx, r.Client, healthCheckScope.HealthCheck, libsveltosv1alpha1.HealthCheckFinalizer,
			logger); err != nil {
			logger.V(logs.LogDebug).Info("failed to update finalizer")
			return reconcile.Result{}, err
		}
	}

	logger.V(logs.LogDebug).Info("update maps")
	r.updateMaps(healthCheckScope.HealthCheck)

	// Queue HealthCheck for evaluation
	manager := evaluation.GetManager()
	manager.EvaluateHealthCheck(healthCheckScope.Name())

	logger.V(logs.LogInfo).Info("reconciliation succeeded")
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *HealthCheckReconciler) SetupWithManager(mgr ctrl.Manager) error {
	c, err := ctrl.NewControllerManagedBy(mgr).
		For(&libsveltosv1alpha1.HealthCheck{}).
		Build(r)
	if err != nil {
		return errors.Wrap(err, "error creating controller")
	}

	// When ConfigMap changes, according to ConfigMapPredicates,
	// one or more HealthChecks need to be reconciled.
	err = c.Watch(&source.Kind{Type: &corev1.ConfigMap{}},
		handler.EnqueueRequestsFromMapFunc(r.requeueHealthCheckForReference),
		ConfigMapPredicates(mgr.GetLogger().WithValues("predicate", "configmappredicate")),
	)
	if err != nil {
		return err
	}

	// When Secret changes, according to SecretPredicates,
	// one or more HEalthChecks need to be reconciled.
	err = c.Watch(&source.Kind{Type: &corev1.Secret{}},
		handler.EnqueueRequestsFromMapFunc(r.requeueHealthCheckForReference),
		SecretPredicates(mgr.GetLogger().WithValues("predicate", "secretpredicate")),
	)
	if err != nil {
		return err
	}

	evaluation.GetManager().RegisterHealthCheckMethod(r.react)

	return nil
}

func (r *HealthCheckReconciler) cleanMaps(healthCheck *libsveltosv1alpha1.HealthCheck) {
	r.Mux.Lock()
	defer r.Mux.Unlock()

	policyRef := getKeyFromObject(r.Scheme, healthCheck)

	delete(r.HealthCheckMap, types.NamespacedName{Name: healthCheck.Name})

	for i := range r.GVKHealthChecks {
		r.GVKHealthChecks[i].Erase(policyRef)
	}

	healthCheckInfo := getKeyFromObject(r.Scheme, healthCheck)
	for i := range r.ReferenceMap {
		healthCheckSet := r.ReferenceMap[i]
		healthCheckSet.Erase(healthCheckInfo)
	}
}

func (r *HealthCheckReconciler) updateMaps(healthCheck *libsveltosv1alpha1.HealthCheck) {
	gvk := schema.GroupVersionKind{
		Group:   healthCheck.Spec.Group,
		Version: healthCheck.Spec.Version,
		Kind:    healthCheck.Spec.Kind,
	}

	policyRef := getKeyFromObject(r.Scheme, healthCheck)

	r.Mux.Lock()
	defer r.Mux.Unlock()

	_, ok := r.GVKHealthChecks[gvk]
	if !ok {
		r.GVKHealthChecks[gvk] = &libsveltosset.Set{}
	}
	r.GVKHealthChecks[gvk].Insert(policyRef)

	currentReferences := &libsveltosset.Set{}
	if healthCheck.Spec.PolicyRef != nil {
		currentReferences.Insert(&corev1.ObjectReference{
			APIVersion: corev1.SchemeGroupVersion.String(), // the only resources that can be referenced are Secret and ConfigMap
			Kind:       healthCheck.Spec.PolicyRef.Kind,
			Namespace:  healthCheck.Spec.PolicyRef.Namespace,
			Name:       healthCheck.Spec.PolicyRef.Name,
		})
	}

	// Get list of References not referenced anymore by ClusterSummary
	var toBeRemoved []corev1.ObjectReference
	healthCheckName := types.NamespacedName{Name: healthCheck.Name}
	if v, ok := r.HealthCheckMap[healthCheckName]; ok {
		toBeRemoved = v.Difference(currentReferences)
	}

	// For each currently referenced instance, add HealthCheck as consumer
	for _, referencedResource := range currentReferences.Items() {
		tmpResource := referencedResource
		r.getReferenceMapForEntry(&tmpResource).Insert(
			&corev1.ObjectReference{
				APIVersion: libsveltosv1alpha1.GroupVersion.String(),
				Kind:       libsveltosv1alpha1.HealthCheckKind,
				Name:       healthCheck.Name,
			},
		)
	}

	// For each resource not reference anymore, remove ClusterSummary as consumer
	for i := range toBeRemoved {
		referencedResource := toBeRemoved[i]
		r.getReferenceMapForEntry(&referencedResource).Erase(
			&corev1.ObjectReference{
				APIVersion: libsveltosv1alpha1.GroupVersion.String(),
				Kind:       libsveltosv1alpha1.HealthCheckKind,
				Name:       healthCheck.Name,
			},
		)
	}

	// Update list of WorklaodRoles currently referenced by ClusterSummary
	r.HealthCheckMap[healthCheckName] = currentReferences
}

func (r *HealthCheckReconciler) getReferenceMapForEntry(entry *corev1.ObjectReference) *libsveltosset.Set {
	s := r.ReferenceMap[*entry]
	if s == nil {
		s = &libsveltosset.Set{}
		r.ReferenceMap[*entry] = s
	}
	return s
}

// react gets called when an instance of passed in gvk has been modified.
// This method queues all Classifier currently using that gvk to be evaluated.
func (r *HealthCheckReconciler) react(gvk *schema.GroupVersionKind) {
	r.Mux.RLock()
	defer r.Mux.RUnlock()

	if gvk == nil {
		return
	}

	manager := evaluation.GetManager()

	if v, ok := r.GVKHealthChecks[*gvk]; ok {
		classifiers := v.Items()
		for i := range classifiers {
			classifier := classifiers[i]
			manager.EvaluateHealthCheck(classifier.Name)
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

// removeHealthCheckReportFinalizer removes finalizer from HealthCheckReport for
// given HealthCheck instance
func (r *HealthCheckReconciler) removeHealthCheckReportFinalizer(ctx context.Context,
	healthCheckScope *scope.HealthCheckScope) error {

	healthCheckReport := &libsveltosv1alpha1.HealthCheckReport{}
	err := r.Get(ctx,
		types.NamespacedName{Namespace: utils.ReportNamespace, Name: healthCheckScope.Name()}, healthCheckReport)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}

	if controllerutil.ContainsFinalizer(healthCheckReport, libsveltosv1alpha1.HealthCheckReportFinalizer) {
		controllerutil.RemoveFinalizer(healthCheckReport, libsveltosv1alpha1.HealthCheckReportFinalizer)
	}
	return nil
}
