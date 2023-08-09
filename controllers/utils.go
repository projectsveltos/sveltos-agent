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
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/cluster-api/util/patch"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/go-logr/logr"
	"github.com/pkg/errors"

	libsveltosv1alpha1 "github.com/projectsveltos/libsveltos/api/v1alpha1"
	logs "github.com/projectsveltos/libsveltos/lib/logsettings"
)

const (
	// deleteRequeueAfter is how long to wait before checking again during healthCheck delete reconciliation
	deleteRequeueAfter = 20 * time.Second
)

//+kubebuilder:rbac:groups=lib.projectsveltos.io,resources=debuggingconfigurations,verbs=get;list;watch
//+kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,verbs=get;list;watch
//+kubebuilder:rbac:groups=*,resources=*,verbs=get;list;watch;impersonate

func InitScheme() (*runtime.Scheme, error) {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		return nil, err
	}
	if err := libsveltosv1alpha1.AddToScheme(s); err != nil {
		return nil, err
	}
	return s, nil
}

// getKeyFromObject returns the Key that can be used in the internal reconciler maps.
func getKeyFromObject(scheme *runtime.Scheme, obj client.Object) *corev1.ObjectReference {
	addTypeInformationToObject(scheme, obj)

	return &corev1.ObjectReference{
		Namespace:  obj.GetNamespace(),
		Name:       obj.GetName(),
		Kind:       obj.GetObjectKind().GroupVersionKind().Kind,
		APIVersion: obj.GetObjectKind().GroupVersionKind().String(),
	}
}

func addTypeInformationToObject(scheme *runtime.Scheme, obj client.Object) {
	gvks, _, err := scheme.ObjectKinds(obj)
	if err != nil {
		panic(1)
	}

	for _, gvk := range gvks {
		if gvk.Kind == "" {
			continue
		}
		if gvk.Version == "" || gvk.Version == runtime.APIVersionInternal {
			continue
		}
		obj.GetObjectKind().SetGroupVersionKind(gvk)
		break
	}
}

func addFinalizer(ctx context.Context, c client.Client, object client.Object, finalizer string,
	logger logr.Logger) error {

	controllerutil.AddFinalizer(object, finalizer)

	return patchObject(ctx, c, object, logger)
}

func removeFinalizer(ctx context.Context, c client.Client, object client.Object, finalizer string,
	logger logr.Logger) error {

	controllerutil.RemoveFinalizer(object, finalizer)

	return patchObject(ctx, c, object, logger)
}

func patchObject(ctx context.Context, c client.Client, object client.Object, logger logr.Logger) error {
	helper, err := patch.NewHelper(object, c)
	if err != nil {
		logger.Error(err, "failed to create patch Helper")
		return err
	}

	if err := helper.Patch(ctx, object); err != nil {
		return errors.Wrapf(
			err,
			"Failed to add finalizer for %s",
			object.GetName(),
		)
	}

	logger.V(logs.LogDebug).Info("added finalizer")
	return nil
}

// Sveltos deploys EventSources,HealtchChecks,Classifiers,Reloaders in managed clusters.
// Sveltos-agent running in those managed clusters needs to process those.
//
// Sometimes the management cluster is in turn a managed cluster. In this case,
// there is another cluster with Sveltos. Management cluster is registered to
// be managed by this other cluster.
//
// |----------------|   |-------------------|   |--------------|
// | Managed cluster|<--| Management cluster|<--| Other cluster|
// |----------------|   |-------------------|   |--------------|
//
// In such a case, in the management cluster we will have two different types of
// EventSources,HealtchChecks,Classifiers/Reloaders:
// 1. Instances defined by platform/tenant admins in the management cluster that
// needs to be pushed and evaluated in the managed clusters;
// 2. Instances defined in the other cluster acting that are pushed to be evaluated
// in the management cluster.
// This means not all EventSources,HealtchChecks,Classifiers need to be evaluated
// by sveltos-agent. Sveltos-agent should evaluate *only* the instances pushed in the
// cluster by Sveltos.
// When Sveltos deploys EventSources,HealtchChecks,Classifiers a special annotation
// is added. If this annotation is present, then sveltos-agent need to process those
// instances. It can ignore those otherwise
func shouldIgnore(o client.Object) bool {
	annotation := o.GetAnnotations()
	if annotation == nil {
		return true
	}

	if _, ok := annotation[libsveltosv1alpha1.DeployedBySveltosAnnotation]; !ok {
		return true
	}

	return false
}
