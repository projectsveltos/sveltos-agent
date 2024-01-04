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
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	libsveltosv1alpha1 "github.com/projectsveltos/libsveltos/api/v1alpha1"
	logs "github.com/projectsveltos/libsveltos/lib/logsettings"
	libsveltosset "github.com/projectsveltos/libsveltos/lib/set"
	"github.com/projectsveltos/sveltos-agent/pkg/evaluation"
	"github.com/projectsveltos/sveltos-agent/pkg/scope"
)

type Mode int64

const (
	SendReports Mode = iota
	DoNotSendReports
)

// ClassifierReconciler reconciles a Classifier object
type ClassifierReconciler struct {
	client.Client
	Scheme           *runtime.Scheme
	RunMode          Mode
	ClusterNamespace string
	ClusterName      string
	ClusterType      libsveltosv1alpha1.ClusterType
	// Used to update internal maps and sets
	Mux sync.RWMutex
	// key: GVK, Value: list of Classifiers based on that GVK
	GVKClassifiers map[schema.GroupVersionKind]*libsveltosset.Set
	// List of Classifier instances based on Kubernetes version
	VersionClassifiers libsveltosset.Set
}

//+kubebuilder:rbac:groups=lib.projectsveltos.io,resources=classifiers,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups=lib.projectsveltos.io,resources=classifiers/finalizers,verbs=update
//+kubebuilder:rbac:groups=lib.projectsveltos.io,resources=classifierreports,verbs=get;list;create;update;delete;patch
//+kubebuilder:rbac:groups=lib.projectsveltos.io,resources=classifierreports/status,verbs=get;update;patch

func (r *ClassifierReconciler) Reconcile(ctx context.Context, req ctrl.Request) (_ ctrl.Result, reterr error) {
	logger := ctrl.LoggerFrom(ctx)
	logger.V(logs.LogInfo).Info("Reconciling")

	// Fecth the Classifier instance
	classifier := &libsveltosv1alpha1.Classifier{}
	err := r.Get(ctx, req.NamespacedName, classifier)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}
		logger.Error(err, "Failed to fetch Classifier")
		return reconcile.Result{}, errors.Wrapf(
			err,
			"Failed to fetch Classifier %s",
			req.NamespacedName,
		)
	}

	if shouldIgnore(classifier) {
		return reconcile.Result{}, nil
	}

	logger.V(logs.LogDebug).Info("request to re-evaluate resource to watch")
	manager := evaluation.GetManager()
	manager.ReEvaluateResourceToWatch()

	classifierScope, err := scope.NewClassifierScope(scope.ClassifierScopeParams{
		Client:     r.Client,
		Logger:     logger,
		Classifier: classifier,
	})
	if err != nil {
		logger.Error(err, "Failed to create classifierScope")
		return reconcile.Result{}, errors.Wrapf(
			err,
			"unable to create classifier scope for %s",
			req.NamespacedName,
		)
	}

	// Always close the scope when exiting this function so we can persist any Classifier
	// changes.
	defer func() {
		if err := classifierScope.Close(ctx); err != nil {
			reterr = err
		}
	}()

	// Handle deleted classifier
	if !classifier.DeletionTimestamp.IsZero() {
		r.reconcileDelete(classifierScope, logger)
		return ctrl.Result{}, nil
	}

	// Handle non-deleted classifier
	return ctrl.Result{}, r.reconcileNormal(ctx, classifierScope, logger)
}

func (r *ClassifierReconciler) reconcileDelete(
	classifierScope *scope.ClassifierScope,
	logger logr.Logger,
) {

	logger.V(logs.LogDebug).Info("reconcile delete")

	logger.V(logs.LogDebug).Info("remove classifier from maps")
	policyRef := getKeyFromObject(r.Scheme, classifierScope.Classifier)

	r.Mux.Lock()
	defer r.Mux.Unlock()

	r.VersionClassifiers.Erase(policyRef)

	for i := range r.GVKClassifiers {
		r.GVKClassifiers[i].Erase(policyRef)
	}

	// Queue Classifier for evaluation
	manager := evaluation.GetManager()
	manager.EvaluateClassifier(classifierScope.Name())

	if controllerutil.ContainsFinalizer(classifierScope.Classifier, libsveltosv1alpha1.ClassifierFinalizer) {
		controllerutil.RemoveFinalizer(classifierScope.Classifier, libsveltosv1alpha1.ClassifierFinalizer)
	}

	logger.V(logs.LogInfo).Info("reconciliation succeeded")
}

func (r *ClassifierReconciler) reconcileNormal(ctx context.Context,
	classifierScope *scope.ClassifierScope,
	logger logr.Logger,
) error {

	logger.V(logs.LogDebug).Info("reconcile")

	if !controllerutil.ContainsFinalizer(classifierScope.Classifier, libsveltosv1alpha1.ClassifierFinalizer) {
		if err := addFinalizer(ctx, r.Client, classifierScope.Classifier, libsveltosv1alpha1.ClassifierFinalizer,
			logger); err != nil {
			logger.V(logs.LogDebug).Info("failed to update finalizer")
			return err
		}
	}

	logger.V(logs.LogDebug).Info("update maps")
	r.updateMaps(classifierScope.Classifier)

	// Queue Classifier for evaluation
	manager := evaluation.GetManager()
	manager.EvaluateClassifier(classifierScope.Name())

	logger.V(logs.LogInfo).Info("reconciliation succeeded")
	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ClassifierReconciler) SetupWithManager(mgr ctrl.Manager) error {
	_, err := ctrl.NewControllerManagedBy(mgr).
		For(&libsveltosv1alpha1.Classifier{}).
		Build(r)
	if err != nil {
		return errors.Wrap(err, "error creating controller")
	}

	evaluation.GetManager().RegisterClassifierMethod(r.react)

	return nil
}

func (r *ClassifierReconciler) updateMaps(classifier *libsveltosv1alpha1.Classifier) {
	r.Mux.Lock()
	defer r.Mux.Unlock()

	policyRef := getKeyFromObject(r.Scheme, classifier)

	if classifier.Spec.KubernetesVersionConstraints != nil {
		r.VersionClassifiers.Insert(policyRef)
	}

	if classifier.Spec.DeployedResourceConstraint == nil {
		return
	}

	gvks := make([]schema.GroupVersionKind, len(classifier.Spec.DeployedResourceConstraint.ResourceSelectors))
	for i := range classifier.Spec.DeployedResourceConstraint.ResourceSelectors {
		rs := &classifier.Spec.DeployedResourceConstraint.ResourceSelectors[i]
		gvk := schema.GroupVersionKind{
			Group:   rs.Group,
			Version: rs.Version,
			Kind:    rs.Kind,
		}
		gvks[i] = gvk
	}

	for i := range gvks {
		_, ok := r.GVKClassifiers[gvks[i]]
		if !ok {
			r.GVKClassifiers[gvks[i]] = &libsveltosset.Set{}
		}
		r.GVKClassifiers[gvks[i]].Insert(policyRef)
	}
}

// react gets called when an instance of passed in gvk has been modified.
// This method queues all Classifier currently using that gvk to be evaluated.
func (r *ClassifierReconciler) react(gvk *schema.GroupVersionKind) {
	r.Mux.RLock()
	defer r.Mux.RUnlock()

	if gvk == nil {
		return
	}

	manager := evaluation.GetManager()

	if v, ok := r.GVKClassifiers[*gvk]; ok {
		classifiers := v.Items()
		for i := range classifiers {
			classifier := classifiers[i]
			manager.EvaluateClassifier(classifier.Name)
		}
	}
}
