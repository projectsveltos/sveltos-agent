/*
Copyright 2022.

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
	"sigs.k8s.io/cluster-api/util/patch"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/projectsveltos/classifier-agent/pkg/classification"
	"github.com/projectsveltos/classifier-agent/pkg/scope"
	libsveltosv1alpha1 "github.com/projectsveltos/libsveltos/api/v1alpha1"
	logs "github.com/projectsveltos/libsveltos/lib/logsettings"
	libsveltosset "github.com/projectsveltos/libsveltos/lib/set"
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

//+kubebuilder:rbac:groups=lib.projectsveltos.io,resources=classifiers,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=lib.projectsveltos.io,resources=classifiers/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=lib.projectsveltos.io,resources=classifiers/finalizers,verbs=update
//+kubebuilder:rbac:groups=lib.projectsveltos.io,resources=classifierreports,verbs=get;list;create;update;delete;patch
//+kubebuilder:rbac:groups=lib.projectsveltos.io,resources=classifierreports/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=*,resources=*,verbs=get;list;watch

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
			"Failed to fetch ClusterProfile %s",
			req.NamespacedName,
		)
	}

	logger.V(logs.LogDebug).Info("request to re-evaluate resource to watch")
	manager := classification.GetManager()
	manager.ReEvaluateResourceToWatch()

	classifierScope, err := scope.NewClassifierScope(scope.ClassifierScopeParams{
		Client:         r.Client,
		Logger:         logger,
		Classifier:     classifier,
		ControllerName: "classifier",
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
		return r.reconcileDelete(classifierScope, logger)
	}

	// Handle non-deleted classifier
	return r.reconcileNormal(ctx, classifierScope, logger)
}

func (r *ClassifierReconciler) reconcileDelete(
	classifierScope *scope.ClassifierScope,
	logger logr.Logger,
) (reconcile.Result, error) {

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
	manager := classification.GetManager()
	manager.EvaluateClassifier(classifierScope.Name())

	if controllerutil.ContainsFinalizer(classifierScope.Classifier, libsveltosv1alpha1.ClassifierFinalizer) {
		controllerutil.RemoveFinalizer(classifierScope.Classifier, libsveltosv1alpha1.ClassifierFinalizer)
	}

	logger.V(logs.LogInfo).Info("reconciliation succeeded")
	return ctrl.Result{}, nil
}

func (r *ClassifierReconciler) reconcileNormal(ctx context.Context,
	classifierScope *scope.ClassifierScope,
	logger logr.Logger,
) (reconcile.Result, error) {

	logger.V(logs.LogDebug).Info("reconcile")

	if !controllerutil.ContainsFinalizer(classifierScope.Classifier, libsveltosv1alpha1.ClassifierFinalizer) {
		if err := r.addFinalizer(ctx, classifierScope.Classifier, logger); err != nil {
			logger.V(logs.LogDebug).Info("failed to update finalizer")
			return reconcile.Result{}, err
		}
	}

	logger.V(logs.LogDebug).Info("update maps")
	r.updateMaps(classifierScope.Classifier)

	// Queue Classifier for evaluation
	manager := classification.GetManager()
	manager.EvaluateClassifier(classifierScope.Name())

	logger.V(logs.LogInfo).Info("reconciliation succeeded")
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ClassifierReconciler) SetupWithManager(mgr ctrl.Manager) error {
	_, err := ctrl.NewControllerManagedBy(mgr).
		For(&libsveltosv1alpha1.Classifier{}).
		Build(r)
	if err != nil {
		return errors.Wrap(err, "error creating controller")
	}

	sendReport := false
	if r.RunMode == SendReports {
		sendReport = true
	}
	const intervalInSecond = 10
	classification.InitializeManager(context.TODO(), mgr.GetLogger(),
		mgr.GetConfig(), mgr.GetClient(), r.ClusterNamespace, r.ClusterName, r.ClusterType,
		r.react, intervalInSecond, sendReport)

	return nil
}

func (r *ClassifierReconciler) updateMaps(classifier *libsveltosv1alpha1.Classifier) {
	gvks := make([]schema.GroupVersionKind, len(classifier.Spec.DeployedResourceConstraints))
	for i := range classifier.Spec.DeployedResourceConstraints {
		gvk := schema.GroupVersionKind{
			Group:   classifier.Spec.DeployedResourceConstraints[i].Group,
			Version: classifier.Spec.DeployedResourceConstraints[i].Version,
			Kind:    classifier.Spec.DeployedResourceConstraints[i].Kind,
		}
		gvks[i] = gvk
	}

	policyRef := getKeyFromObject(r.Scheme, classifier)

	r.Mux.Lock()
	defer r.Mux.Unlock()

	if classifier.Spec.KubernetesVersionConstraints != nil {
		r.VersionClassifiers.Insert(policyRef)
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

	manager := classification.GetManager()

	if v, ok := r.GVKClassifiers[*gvk]; ok {
		classifiers := v.Items()
		for i := range classifiers {
			classifier := classifiers[i]
			manager.EvaluateClassifier(classifier.Name)
		}
	}
}

func (r *ClassifierReconciler) addFinalizer(ctx context.Context, classifier *libsveltosv1alpha1.Classifier,
	logger logr.Logger) error {

	// If the SveltosCluster doesn't have our finalizer, add it.
	controllerutil.AddFinalizer(classifier, libsveltosv1alpha1.ClassifierFinalizer)

	helper, err := patch.NewHelper(classifier, r.Client)
	if err != nil {
		logger.Error(err, "failed to create patch Helper")
		return err
	}

	// Register the finalizer immediately to avoid orphaning clusterprofile resources on delete
	if err := helper.Patch(ctx, classifier); err != nil {
		return errors.Wrapf(
			err,
			"Failed to add finalizer for %s",
			classifier.Name,
		)
	}

	logger.V(logs.LogDebug).Info("added finalizer")
	return nil
}
