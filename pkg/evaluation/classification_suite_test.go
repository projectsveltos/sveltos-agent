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
	"fmt"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/klog/v2"
	"sigs.k8s.io/cluster-api/util"
	ctrl "sigs.k8s.io/controller-runtime"

	libsveltosv1alpha1 "github.com/projectsveltos/libsveltos/api/v1alpha1"
	"github.com/projectsveltos/libsveltos/lib/crd"
	"github.com/projectsveltos/libsveltos/lib/utils"
	"github.com/projectsveltos/sveltos-agent/internal/test/helpers"
)

var (
	testEnv *helpers.TestEnvironment
	cancel  context.CancelFunc
	ctx     context.Context
	scheme  *runtime.Scheme
)

const (
	timeout         = 80 * time.Second
	pollingInterval = 2 * time.Second
)

func TestControllers(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Classification Suite")
}

var _ = BeforeSuite(func() {
	By("bootstrapping test environment")

	ctrl.SetLogger(klog.Background())

	ctx, cancel = context.WithCancel(context.TODO())

	var err error
	scheme, err = setupScheme()
	Expect(err).To(BeNil())

	testEnvConfig := helpers.NewTestEnvironmentConfiguration([]string{}, scheme)
	testEnv, err = testEnvConfig.Build(scheme)
	if err != nil {
		panic(err)
	}

	go func() {
		By("Starting the manager")
		err = testEnv.StartManager(ctx)
		if err != nil {
			panic(fmt.Sprintf("Failed to start the envtest manager: %v", err))
		}
	}()

	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "projectsveltos",
		},
	}
	Expect(testEnv.Create(ctx, ns)).To(Succeed())
	waitForObject(ctx, testEnv.Client, ns)

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: libsveltosv1alpha1.ClassifierSecretNamespace,
			Name:      libsveltosv1alpha1.ClassifierSecretName,
		},
		Data: map[string][]byte{
			"data": testEnv.Kubeconfig,
		},
	}

	Expect(testEnv.Create(context.TODO(), secret)).To(Succeed())
	waitForObject(context.TODO(), testEnv.Client, secret)

	classifierCRD, err := utils.GetUnstructured(crd.GetClassifierCRDYAML())
	Expect(err).To(BeNil())
	Expect(testEnv.Create(ctx, classifierCRD)).To(Succeed())
	waitForObject(ctx, testEnv.Client, classifierCRD)

	classifierReportCRD, err := utils.GetUnstructured(crd.GetClassifierReportCRDYAML())
	Expect(err).To(BeNil())
	Expect(testEnv.Create(ctx, classifierReportCRD)).To(Succeed())
	waitForObject(ctx, testEnv.Client, classifierReportCRD)

	healthCheckCRD, err := utils.GetUnstructured(crd.GetHealthCheckCRDYAML())
	Expect(err).To(BeNil())
	Expect(testEnv.Create(ctx, healthCheckCRD)).To(Succeed())
	waitForObject(ctx, testEnv.Client, healthCheckCRD)

	healthCheckCReportRD, err := utils.GetUnstructured(crd.GetHealthCheckReportCRDYAML())
	Expect(err).To(BeNil())
	Expect(testEnv.Create(ctx, healthCheckCReportRD)).To(Succeed())
	waitForObject(ctx, testEnv.Client, healthCheckCReportRD)

	eventSourceCRD, err := utils.GetUnstructured(crd.GetEventSourceCRDYAML())
	Expect(err).To(BeNil())
	Expect(testEnv.Create(ctx, eventSourceCRD)).To(Succeed())
	waitForObject(ctx, testEnv.Client, eventSourceCRD)

	eventReportRD, err := utils.GetUnstructured(crd.GetEventReportCRDYAML())
	Expect(err).To(BeNil())
	Expect(testEnv.Create(ctx, eventReportRD)).To(Succeed())
	waitForObject(ctx, testEnv.Client, eventReportRD)

	reloaderCRD, err := utils.GetUnstructured(crd.GetReloaderCRDYAML())
	Expect(err).To(BeNil())
	Expect(testEnv.Create(ctx, reloaderCRD)).To(Succeed())
	waitForObject(ctx, testEnv.Client, reloaderCRD)

	reloaderReportRD, err := utils.GetUnstructured(crd.GetReloaderReportCRDYAML())
	Expect(err).To(BeNil())
	Expect(testEnv.Create(ctx, reloaderReportRD)).To(Succeed())
	waitForObject(ctx, testEnv.Client, reloaderReportRD)

	// add an extra second sleep. Otherwise randomly ut fails with
	// no matches for kind "EventSource" in version "lib.projectsveltos.io/v1alpha1"
	time.Sleep(time.Second)

	if synced := testEnv.GetCache().WaitForCacheSync(ctx); !synced {
		time.Sleep(time.Second)
	}
})

var _ = AfterSuite(func() {
	cancel()
	By("tearing down the test environment")
	err := testEnv.Stop()
	Expect(err).ToNot(HaveOccurred())
})

func randomString() string {
	const length = 10
	return "a-" + util.RandomString(length)
}

func setupScheme() (*runtime.Scheme, error) {
	s := runtime.NewScheme()
	if err := libsveltosv1alpha1.AddToScheme(s); err != nil {
		return nil, err
	}
	if err := clientgoscheme.AddToScheme(s); err != nil {
		return nil, err
	}
	if err := apiextensionsv1.AddToScheme(s); err != nil {
		return nil, err
	}
	return s, nil
}

func getClassifierWithKubernetesConstraints(k8sVersion string, comparison libsveltosv1alpha1.KubernetesComparison,
) *libsveltosv1alpha1.Classifier {

	return &libsveltosv1alpha1.Classifier{
		ObjectMeta: metav1.ObjectMeta{
			Name: randomString(),
		},
		Spec: libsveltosv1alpha1.ClassifierSpec{
			KubernetesVersionConstraints: []libsveltosv1alpha1.KubernetesVersionConstraint{
				{
					Comparison: string(comparison),
					Version:    k8sVersion,
				},
			},
			ClassifierLabels: []libsveltosv1alpha1.ClassifierLabel{
				{Key: randomString(), Value: randomString()},
			},
		},
	}
}
