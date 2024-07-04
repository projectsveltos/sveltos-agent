/*
Copyright 2023. projectsveltos.io. All rights reserved.

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

package fv_test

import (
	"context"
	"fmt"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	libsveltosv1beta1 "github.com/projectsveltos/libsveltos/api/v1beta1"
	libsveltosutils "github.com/projectsveltos/libsveltos/lib/utils"
	"github.com/projectsveltos/sveltos-agent/pkg/utils"
)

const (
	deploymentWithConfigMap = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: nginx-deployment
  namespace: %s
spec:
  replicas: 3
  selector:
    matchLabels:
      app: nginx
  template:
    metadata:
      labels:
        app: nginx
    spec:
      containers:
        - name: nginx
          image: nginx:latest
          ports:
            - containerPort: 80
          volumeMounts:
            - name: nginx-config-volume
              mountPath: /etc/nginx/conf.d # Mount the configuration inside the container
      volumes:
        - name: nginx-config-volume
          configMap:
            name: nginx-configmap # ConfigMap name to be mounted
`

	mountedConfigMap = `apiVersion: v1
kind: ConfigMap
metadata:
  name: nginx-configmap
  namespace: %s
data:
  nginx-config.conf: |
    server {
      listen 80;
      server_name example.com;

      location / {
        root /usr/share/nginx/html;
        index index.html;
      }
    }
`
)

var _ = Describe("Reloader", func() {
	var key string
	var value string
	const (
		namePrefix = "reloader-"
	)

	BeforeEach(func() {
		key = randomString()
		value = randomString()
	})

	AfterEach(func() {
		listOptions := []client.ListOption{
			client.MatchingLabels{
				key: value,
			},
		}

		namespaceList := &corev1.NamespaceList{}
		Expect(k8sClient.List(context.TODO(), namespaceList, listOptions...)).To(Succeed())

		for i := range namespaceList.Items {
			Expect(k8sClient.Delete(context.TODO(), &namespaceList.Items[i])).To(Succeed())
		}
	})

	It("Evaluate reloaderReport", Label("FV"), func() {
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: randomString(),
			},
		}
		By(fmt.Sprintf("Create namespace %s for this test", ns.Name))
		Expect(k8sClient.Create(context.TODO(), ns)).To(Succeed())

		By("Creating a ConfigMap")
		configMap, err := libsveltosutils.GetUnstructured([]byte(fmt.Sprintf(mountedConfigMap, ns.Name)))
		Expect(err).To(BeNil())
		err = k8sClient.Create(context.TODO(), configMap)
		if err != nil {
			Expect(apierrors.IsAlreadyExists(err)).To(BeTrue())
		}

		By("Creating a deployment mounting a ConfigMap")
		var deployment *unstructured.Unstructured
		deployment, err = libsveltosutils.GetUnstructured([]byte(fmt.Sprintf(deploymentWithConfigMap, ns.Name)))
		Expect(err).To(BeNil())
		err = k8sClient.Create(context.TODO(), deployment)
		if err != nil {
			Expect(apierrors.IsAlreadyExists(err)).To(BeTrue())
		}

		reloader := libsveltosv1beta1.Reloader{
			ObjectMeta: metav1.ObjectMeta{
				Name: namePrefix + randomString(),
				Annotations: map[string]string{
					"projectsveltos.io/deployed-by-sveltos": "ok",
				},
			},
			Spec: libsveltosv1beta1.ReloaderSpec{
				ReloaderInfo: []libsveltosv1beta1.ReloaderInfo{
					{
						Kind:      "Deployment",
						Namespace: deployment.GetNamespace(),
						Name:      deployment.GetName(),
					},
				},
			},
		}

		By(fmt.Sprintf("Creating reloader %s", reloader.Name))
		Expect(k8sClient.Create(context.TODO(), &reloader)).To(Succeed())

		mountedResource := &corev1.ObjectReference{
			Kind:      "ConfigMap",
			Namespace: configMap.GetNamespace(),
			Name:      configMap.GetName(),
		}

		// Since ConfigMap has not changed, expect no ReloadReport
		// This test removes all existing ReloadReports when started
		By("Verifying ReloaderReport is not created yet")
		Consistently(func() bool {
			reloaderReports := libsveltosv1beta1.ReloaderReportList{}
			err := k8sClient.List(context.TODO(), &reloaderReports)
			if err != nil {
				return false
			}

			reloaderReport := getReloaderReportForMountedResource(mountedResource, &reloaderReports)

			return reloaderReport == nil
		}, timeout/2, pollingInterval).Should(BeTrue())

		By("Modify ConfigMap")
		currentConfigMap := &corev1.ConfigMap{}
		Expect(k8sClient.Get(context.TODO(),
			types.NamespacedName{Namespace: configMap.GetNamespace(), Name: configMap.GetName()},
			currentConfigMap)).To(Succeed())

		currentConfigMap.Data[randomString()] = randomString()
		Expect(k8sClient.Update(context.TODO(), currentConfigMap)).To(Succeed())

		By(fmt.Sprintf("Verifying ReloadReport is created for deployment %s/%s",
			deployment.GetNamespace(), deployment.GetName()))
		resourceToReload := &corev1.ObjectReference{
			Kind:      "Deployment",
			Namespace: deployment.GetNamespace(),
			Name:      deployment.GetName(),
		}
		verifyReloaderReport(mountedResource, resourceToReload)

		currentReloader := &libsveltosv1beta1.Reloader{}
		Expect(k8sClient.Get(context.TODO(),
			types.NamespacedName{Namespace: utils.ReportNamespace, Name: reloader.Name},
			currentReloader)).To(Succeed())
		Expect(k8sClient.Delete(context.TODO(), currentReloader)).To(Succeed())
	})
})

func verifyReloaderReport(mountedResource, resourceToReload *corev1.ObjectReference) {
	Eventually(func() bool {
		reloaderReports := libsveltosv1beta1.ReloaderReportList{}
		err := k8sClient.List(context.TODO(), &reloaderReports)
		if err != nil {
			return false
		}

		reloaderReport := getReloaderReportForMountedResource(mountedResource, &reloaderReports)

		if reloaderReport == nil {
			return false
		}

		if len(reloaderReport.Spec.ResourcesToReload) != 1 {
			return false
		}

		if reloaderReport.Spec.ResourcesToReload[0].Kind == resourceToReload.Kind &&
			reloaderReport.Spec.ResourcesToReload[0].Namespace == resourceToReload.Namespace &&
			reloaderReport.Spec.ResourcesToReload[0].Name == resourceToReload.Name {

			return true
		}

		return false
	}, timeout, pollingInterval).Should(BeTrue())
}

// getReloaderReportForMountedResource returns the ReloaderReport created for
// a given mounted resource (either ConfigMap or Secret)
func getReloaderReportForMountedResource(mountedResource *corev1.ObjectReference,
	reloaderReports *libsveltosv1beta1.ReloaderReportList) *libsveltosv1beta1.ReloaderReport {

	for i := range reloaderReports.Items {
		rr := &reloaderReports.Items[i]
		if rr.Annotations == nil {
			continue
		}
		kind, ok := rr.Annotations[libsveltosv1beta1.ReloaderReportResourceKindAnnotation]
		if !ok {
			continue
		}
		var namespace string
		namespace, ok = rr.Annotations[libsveltosv1beta1.ReloaderReportResourceNamespaceAnnotation]
		if !ok {
			continue
		}
		var name string
		name, ok = rr.Annotations[libsveltosv1beta1.ReloaderReportResourceNameAnnotation]
		if !ok {
			continue
		}
		if !strings.EqualFold(kind, mountedResource.Kind) {
			continue
		}
		if namespace != mountedResource.Namespace {
			continue
		}
		if name != mountedResource.Name {
			continue
		}
		return rr
	}

	return nil
}
