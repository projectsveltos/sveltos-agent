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
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	libsveltosv1alpha1 "github.com/projectsveltos/libsveltos/api/v1alpha1"
	logs "github.com/projectsveltos/libsveltos/lib/logsettings"
	libsveltosset "github.com/projectsveltos/libsveltos/lib/set"
	"github.com/projectsveltos/sveltos-agent/pkg/evaluation"
	"github.com/projectsveltos/sveltos-agent/pkg/scope"
)

// ReloaderReconciler reconciles a Reloader object
type ReloaderReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// Used to update internal maps and sets
	Mux sync.RWMutex
	// key: GVK, Value: list of Reloaders based on that GVK
	// For instance, for a Deployment, this will contain list of Reloaders
	// listing at least one Deployment
	GVKReloaders map[schema.GroupVersionKind]*libsveltosset.Set

	RunMode          Mode
	ClusterNamespace string
	ClusterName      string
	ClusterType      libsveltosv1alpha1.ClusterType
}

//+kubebuilder:rbac:groups=lib.projectsveltos.io,resources=reloaders,verbs=get;list;watch;patch
//+kubebuilder:rbac:groups=lib.projectsveltos.io,resources=reloaders/finalizers,verbs=update;patch
//+kubebuilder:rbac:groups=lib.projectsveltos.io,resources=reloaderreports,verbs=get;list;watch;create;delete;patch;update
//+kubebuilder:rbac:groups=lib.projectsveltos.io,resources=reloaderreports/status,verbs=get;update;patch

func (r *ReloaderReconciler) Reconcile(ctx context.Context, req ctrl.Request) (_ ctrl.Result, reterr error) {
	logger := ctrl.LoggerFrom(ctx)
	logger.V(logs.LogInfo).Info("Reconciling")

	// Fecth the reloader instance
	reloader := &libsveltosv1alpha1.Reloader{}
	err := r.Get(ctx, req.NamespacedName, reloader)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}
		logger.Error(err, "Failed to fetch reloader")
		return reconcile.Result{}, errors.Wrapf(
			err,
			"Failed to fetch reloader %s",
			req.NamespacedName,
		)
	}

	if shouldIgnore(reloader) {
		logger.V(logs.LogDebug).Info("ignoring it")
		return reconcile.Result{}, nil
	}

	reloaderScope, err := scope.NewReloaderScope(scope.ReloaderScopeParams{
		Client:   r.Client,
		Logger:   logger,
		Reloader: reloader,
	})
	if err != nil {
		logger.Error(err, "Failed to create reloaderScope")
		return reconcile.Result{}, errors.Wrapf(
			err,
			"unable to create reloader scope for %s",
			req.NamespacedName,
		)
	}

	// Always close the scope when exiting this function so we can persist any Reloader
	// changes.
	defer func() {
		if err := reloaderScope.Close(ctx); err != nil {
			reterr = err
		}
	}()

	// Handle deleted reloader
	if !reloader.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(reloaderScope, logger), nil
	}

	// Handle non-deleted healthCheck
	return ctrl.Result{}, r.reconcileNormal(ctx, reloaderScope, logger)
}

func (r *ReloaderReconciler) reconcileDelete(reloaderScope *scope.ReloaderScope,
	logger logr.Logger,
) reconcile.Result {

	logger.V(logs.LogDebug).Info("reconcile delete")

	logger.V(logs.LogDebug).Info("remove reloader from maps")
	r.cleanMaps(reloaderScope.Reloader)

	// Queue Reloader for evaluation
	manager := evaluation.GetManager()
	manager.EvaluateReloader(reloaderScope.Name())

	// All ReloaderReport created because of this Reloader instance
	// are automatically deleted after being processed by management
	// cluster.

	if controllerutil.ContainsFinalizer(reloaderScope.Reloader, libsveltosv1alpha1.ReloaderFinalizer) {
		controllerutil.RemoveFinalizer(reloaderScope.Reloader, libsveltosv1alpha1.ReloaderFinalizer)
	}

	logger.V(logs.LogInfo).Info("reconciliation succeeded")
	return reconcile.Result{}
}

func (r *ReloaderReconciler) reconcileNormal(ctx context.Context,
	reloaderScope *scope.ReloaderScope, logger logr.Logger,
) error {

	logger.V(logs.LogDebug).Info("reconcile")

	if !controllerutil.ContainsFinalizer(reloaderScope.Reloader, libsveltosv1alpha1.ReloaderFinalizer) {
		if err := addFinalizer(ctx, r.Client, reloaderScope.Reloader, libsveltosv1alpha1.ReloaderFinalizer,
			logger); err != nil {
			logger.V(logs.LogDebug).Info("failed to update finalizer")
			return err
		}
	}

	logger.V(logs.LogDebug).Info("update maps")
	r.updateMaps(reloaderScope.Reloader)

	// Queue EventSource for evaluation
	manager := evaluation.GetManager()
	manager.EvaluateReloader(reloaderScope.Name())

	logger.V(logs.LogInfo).Info("reconciliation succeeded")
	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ReloaderReconciler) SetupWithManager(mgr ctrl.Manager) error {
	c, err := ctrl.NewControllerManagedBy(mgr).
		For(&libsveltosv1alpha1.Reloader{}).
		Build(r)
	if err != nil {
		return errors.Wrap(err, "error creating controller")
	}

	// Reloader contains list of Deployment/StatefulSet/DaemonSet instances
	// to reload if mounted ConfigMap changes. A watcher on ConfigMap is started
	// here. When ConfigMap changes according to ConfigMapPredicates, ConfigMap
	// is queued to be evaluated by evaluation manager. Manager has full view
	// of which Deployment/StatefulSet/DaemonSet instances are mounting which ConfigMap
	// instances.
	err = c.Watch(source.Kind(mgr.GetCache(), &corev1.ConfigMap{}),
		nil,
		ConfigMapPredicates(mgr.GetLogger().WithValues("predicate", "configmappredicate")),
	)
	if err != nil {
		return err
	}

	// Reloader contains list of Deployment/StatefulSet/DaemonSet instances
	// to reload if mounted Secret changes. A watcher on Secret is started
	// here. When Secret changes according to SecretMapPredicates, Secret
	// is queued to be evaluated by evaluation manager. Manager has full view
	// of which Deployment/StatefulSet/DaemonSet instances are mounting which Secret
	// instances.
	err = c.Watch(source.Kind(mgr.GetCache(), &corev1.Secret{}),
		nil,
		SecretPredicates(mgr.GetLogger().WithValues("predicate", "secretpredicate")),
	)

	evaluation.GetManager().RegisterReloaderMethod(r.react)

	return err
}

func (r *ReloaderReconciler) cleanMaps(reloader *libsveltosv1alpha1.Reloader) {
	r.Mux.Lock()
	defer r.Mux.Unlock()

	policyRef := getKeyFromObject(r.Scheme, reloader)

	for gvk := range r.GVKReloaders {
		r.GVKReloaders[gvk].Erase(policyRef)
	}
}

func (r *ReloaderReconciler) updateMaps(reloader *libsveltosv1alpha1.Reloader) {
	currentGVKs := make(map[schema.GroupVersionKind]bool)

	for i := range reloader.Spec.ReloaderInfo {
		gvk := schema.GroupVersionKind{
			// Deployment/StatefulSet/DaemonSet are all appsv1
			Group:   appsv1.SchemeGroupVersion.Group,
			Version: appsv1.SchemeGroupVersion.Version,
			Kind:    reloader.Spec.ReloaderInfo[i].Kind,
		}

		currentGVKs[gvk] = true
	}

	policyRef := getKeyFromObject(r.Scheme, reloader)

	r.Mux.Lock()
	defer r.Mux.Unlock()

	for gvk := range r.GVKReloaders {
		if _, ok := currentGVKs[gvk]; !ok {
			r.GVKReloaders[gvk].Erase(policyRef)
		}
	}

	for gvk := range currentGVKs {
		_, ok := r.GVKReloaders[gvk]
		if !ok {
			r.GVKReloaders[gvk] = &libsveltosset.Set{}
		}
		r.GVKReloaders[gvk].Insert(policyRef)
	}
}

// react gets called when an instance of passed in gvk has been modified.
// This method queues all Reloader currently using that gvk to be evaluated.
func (r *ReloaderReconciler) react(gvk *schema.GroupVersionKind) {
	r.Mux.RLock()
	defer r.Mux.RUnlock()

	if gvk == nil {
		return
	}

	manager := evaluation.GetManager()

	if v, ok := r.GVKReloaders[*gvk]; ok {
		reloaders := v.Items()
		for i := range reloaders {
			eventSource := reloaders[i]
			manager.EvaluateReloader(eventSource.Name)
		}
	}
}
