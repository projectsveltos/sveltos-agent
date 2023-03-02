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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/pkg/errors"

	libsveltosv1alpha1 "github.com/projectsveltos/libsveltos/api/v1alpha1"
	logs "github.com/projectsveltos/libsveltos/lib/logsettings"
)

// HealthCheckReportReconciler reconciles a HealthCheckReport object
type HealthCheckReportReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=lib.projectsveltos.io,resources=healthcheckreports,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=lib.projectsveltos.io,resources=healthcheckreports/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=lib.projectsveltos.io,resources=healthcheckreports/finalizers,verbs=update

func (r *HealthCheckReportReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := ctrl.LoggerFrom(ctx)
	logger.V(logs.LogInfo).Info("Reconciling")

	// Fecth the HealthCheckReport instance
	healthCheckReport := &libsveltosv1alpha1.HealthCheckReport{}
	err := r.Get(ctx, req.NamespacedName, healthCheckReport)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}
		logger.Error(err, "Failed to fetch HealthCheckReport")
		return reconcile.Result{}, errors.Wrapf(
			err,
			"Failed to fetch HealthCheckReport %s",
			req.NamespacedName,
		)
	}

	logger.V(logs.LogDebug).Info("request to re-evaluate resource to watch")

	// Handle deleted healthCheck
	if !healthCheckReport.DeletionTimestamp.IsZero() {
		if healthCheckReport.Status.Phase != nil &&
			*healthCheckReport.Status.Phase == libsveltosv1alpha1.ReportProcessed {

			if controllerutil.ContainsFinalizer(healthCheckReport, libsveltosv1alpha1.HealthCheckReportFinalizer) {
				err = removeFinalizer(ctx, r.Client, healthCheckReport, libsveltosv1alpha1.HealthCheckReportFinalizer,
					logger)
			}
		}
	} else {
		if !controllerutil.ContainsFinalizer(healthCheckReport, libsveltosv1alpha1.HealthCheckReportFinalizer) {
			err = addFinalizer(ctx, r.Client, healthCheckReport, libsveltosv1alpha1.HealthCheckReportFinalizer,
				logger)
		}
	}

	if err != nil {
		return reconcile.Result{Requeue: true, RequeueAfter: deleteRequeueAfter}, nil
	}

	return reconcile.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *HealthCheckReportReconciler) SetupWithManager(mgr ctrl.Manager) error {
	_, err := ctrl.NewControllerManagedBy(mgr).
		For(&libsveltosv1alpha1.HealthCheckReport{}).
		Build(r)
	if err != nil {
		return errors.Wrap(err, "error creating controller")
	}

	return nil
}
