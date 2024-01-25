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

package evaluation_test

import (
	"context"

	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func getDeployment(replicas, availableReplicas int32) *appsv1.Deployment {
	labels := map[string]string{randomString(): randomString()}
	labelSelector := metav1.LabelSelector{
		MatchLabels: labels,
	}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: randomString(),
			Name:      randomString(),
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &labelSelector,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  randomString(),
							Image: randomString(),
						},
					},
				},
			},
		},
		Status: appsv1.DeploymentStatus{
			AvailableReplicas: availableReplicas,
		},
	}
}

func getService() *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: randomString(),
			Name:      randomString(),
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{
				{
					Name: randomString(),
					Port: 443,
				},
			},
		},
	}
}

func createNamespace(name string) {
	currentNamespace := &corev1.Namespace{}
	err := testEnv.Get(context.TODO(), types.NamespacedName{Name: name}, currentNamespace)
	if err == nil {
		return
	}

	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
	}

	Expect(testEnv.Create(context.TODO(), ns)).To(Succeed())
	waitForObject(context.TODO(), testEnv.Client, ns)
}

// waitForObject waits for the cache to be updated helps in preventing test flakes
// due to the cache sync delays.
func waitForObject(ctx context.Context, c client.Client, obj client.Object) {
	// Makes sure the cache is updated with the new object
	objCopy := obj.DeepCopyObject().(client.Object)
	key := client.ObjectKeyFromObject(obj)
	Eventually(func() error {
		return c.Get(ctx, key, objCopy)
	}, timeout, pollingInterval).Should(BeNil())
}

func verifyResult(result, matchingResources, nonMatchingResources []*unstructured.Unstructured) {
	for i := range matchingResources {
		Expect(result).To(ContainElement(matchingResources[i]))
	}

	for i := range nonMatchingResources {
		Expect(result).ToNot(ContainElement(nonMatchingResources[i]))
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
