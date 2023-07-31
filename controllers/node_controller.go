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
	"fmt"

	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	libsveltosv1alpha1 "github.com/projectsveltos/libsveltos/api/v1alpha1"
	logs "github.com/projectsveltos/libsveltos/lib/logsettings"
	"github.com/projectsveltos/sveltos-agent/pkg/evaluation"
	"github.com/projectsveltos/sveltos-agent/pkg/utils"
)

// NodeReconciler reconciles a Node object
type NodeReconciler struct {
	client.Client
	*rest.Config
	Scheme *runtime.Scheme

	// kubernetesVersion is the current Kubernetes version
	kubernetesVersion string
}

// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
func (r *NodeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := ctrl.LoggerFrom(ctx)
	logger.V(logs.LogInfo).Info("Reconciling")

	// Fecth the Node instance
	node := &corev1.Node{}
	err := r.Get(ctx, req.NamespacedName, node)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}
		logger.Error(err, "Failed to fetch Node")
		return reconcile.Result{}, errors.Wrapf(
			err,
			"Failed to fetch Node %s",
			req.NamespacedName,
		)
	}

	logger = logger.WithValues("node", node.Name)

	version, err := utils.GetKubernetesVersion(ctx, r.Config, logger)
	if err != nil {
		return reconcile.Result{}, nil
	}
	if version != r.kubernetesVersion {
		// Re-evaluate all Classifiers using Kubernetes as one of the criteria
		var list []string
		list, err = r.findClassifierUsingKubernetesVersion(ctx, logger)
		if err != nil {
			return reconcile.Result{}, err
		}

		manager := evaluation.GetManager()
		for i := range list {
			logger.V(logs.LogDebug).Info(fmt.Sprintf("classifier %s needs re-evaluation",
				list[i]))
			manager.EvaluateClassifier(list[i])
		}

		r.kubernetesVersion = version
	}

	logger.V(logs.LogInfo).Info("reconciliation succeeded")
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *NodeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Node{}).
		Complete(r)
}

func (r *NodeReconciler) findClassifierUsingKubernetesVersion(ctx context.Context,
	logger logr.Logger) ([]string, error) {

	classifierList := &libsveltosv1alpha1.ClassifierList{}
	err := r.List(ctx, classifierList)
	if err != nil {
		return nil, err
	}

	result := make([]string, 0)

	for i := range classifierList.Items {
		classifier := &classifierList.Items[i]
		if len(classifier.Spec.KubernetesVersionConstraints) > 0 {
			result = append(result, classifier.Name)
		}
	}

	return result, nil
}
