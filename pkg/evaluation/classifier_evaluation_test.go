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
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2/textlogger"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	libsveltosv1beta1 "github.com/projectsveltos/libsveltos/api/v1beta1"
	"github.com/projectsveltos/libsveltos/lib/k8s_utils"
	"github.com/projectsveltos/sveltos-agent/pkg/evaluation"
	"github.com/projectsveltos/sveltos-agent/pkg/utils"
)

const (
	higherVersion  = "v1.32.1"
	currentVersion = "v1.32.0" // taken from KUBEBUILDER_ENVTEST_KUBERNETES_VERSION in Makefile
	lowerVersion   = "v1.31.0"
)

var (
	podTemplate = `apiVersion: v1
kind: Pod
metadata:
  namespace: %s
  name: %s
spec:
  containers:
  - name: nginx
    image: nginx:1.14.2`
)

var _ = Describe("Manager: classifier evaluation", func() {
	var classifier *libsveltosv1beta1.Classifier
	var clusterNamespace string
	var clusterName string
	var clusterType libsveltosv1beta1.ClusterType

	BeforeEach(func() {
		evaluation.Reset()
		clusterNamespace = utils.ReportNamespace
		clusterName = randomString()
		clusterType = libsveltosv1beta1.ClusterTypeCapi
	})

	AfterEach(func() {
		if classifier != nil {
			err := testEnv.Client.Delete(context.TODO(), classifier)
			if err != nil {
				Expect(apierrors.IsNotFound(err)).To(BeTrue())
			}
		}
	})

	It("IsVersionAMatch: comparisonEqual returns true when version matches", func() {
		classifier = getClassifierWithKubernetesConstraints(currentVersion, libsveltosv1beta1.ComparisonEqual)

		Expect(testEnv.Create(context.TODO(), classifier)).To(Succeed())
		waitForObject(context.TODO(), testEnv.Client, classifier)

		evaluation.InitializeManagerWithSkip(context.TODO(), textlogger.NewLogger(textlogger.NewConfig(textlogger.Verbosity(1))),
			testEnv.Config, testEnv.Client, clusterNamespace, clusterName, clusterType, 10)

		manager := evaluation.GetManager()
		Expect(manager).ToNot(BeNil())

		match, err := evaluation.IsVersionAMatch(manager, context.TODO(),
			classifier)
		Expect(err).To(BeNil())
		Expect(match).To(BeTrue())
	})

	It("IsVersionAMatch: comparisonEqual returns false when version does not match", func() {
		classifier = getClassifierWithKubernetesConstraints("v1.25.2", libsveltosv1beta1.ComparisonEqual)
		Expect(testEnv.Create(context.TODO(), classifier)).To(Succeed())
		waitForObject(context.TODO(), testEnv.Client, classifier)

		evaluation.InitializeManagerWithSkip(context.TODO(), textlogger.NewLogger(textlogger.NewConfig(textlogger.Verbosity(1))), testEnv.Config, testEnv.Client,
			clusterNamespace, clusterName, clusterType, 10)

		manager := evaluation.GetManager()
		Expect(manager).ToNot(BeNil())

		match, err := evaluation.IsVersionAMatch(manager, context.TODO(),
			classifier)
		Expect(err).To(BeNil())
		Expect(match).To(BeFalse())
	})

	It("IsVersionAMatch: ComparisonNotEqual returns true when version doesn't match", func() {
		classifier = getClassifierWithKubernetesConstraints(lowerVersion, libsveltosv1beta1.ComparisonNotEqual)
		Expect(testEnv.Create(context.TODO(), classifier)).To(Succeed())
		waitForObject(context.TODO(), testEnv.Client, classifier)

		evaluation.InitializeManagerWithSkip(context.TODO(), textlogger.NewLogger(textlogger.NewConfig(textlogger.Verbosity(1))), testEnv.Config, testEnv.Client,
			clusterNamespace, clusterName, clusterType, 10)

		manager := evaluation.GetManager()
		Expect(manager).ToNot(BeNil())

		match, err := evaluation.IsVersionAMatch(manager, context.TODO(),
			classifier)
		Expect(err).To(BeNil())
		Expect(match).To(BeTrue())
	})

	It("IsVersionAMatch: ComparisonNotEqual returns false when version matches", func() {
		classifier = getClassifierWithKubernetesConstraints(currentVersion, libsveltosv1beta1.ComparisonNotEqual)
		Expect(testEnv.Create(context.TODO(), classifier)).To(Succeed())
		waitForObject(context.TODO(), testEnv.Client, classifier)

		evaluation.InitializeManagerWithSkip(context.TODO(), textlogger.NewLogger(textlogger.NewConfig(textlogger.Verbosity(1))), testEnv.Config, testEnv.Client,
			clusterNamespace, clusterName, clusterType, 10)

		manager := evaluation.GetManager()
		Expect(manager).ToNot(BeNil())

		match, err := evaluation.IsVersionAMatch(manager, context.TODO(),
			classifier)
		Expect(err).To(BeNil())
		Expect(match).To(BeFalse())
	})

	It("IsVersionAMatch: ComparisonGreaterThan returns true when version is strictly greater than specified one", func() {
		classifier = getClassifierWithKubernetesConstraints(lowerVersion, libsveltosv1beta1.ComparisonGreaterThan)
		Expect(testEnv.Create(context.TODO(), classifier)).To(Succeed())
		waitForObject(context.TODO(), testEnv.Client, classifier)

		evaluation.InitializeManagerWithSkip(context.TODO(), textlogger.NewLogger(textlogger.NewConfig(textlogger.Verbosity(1))), testEnv.Config, testEnv.Client,
			clusterNamespace, clusterName, clusterType, 10)

		manager := evaluation.GetManager()
		Expect(manager).ToNot(BeNil())

		match, err := evaluation.IsVersionAMatch(manager, context.TODO(),
			classifier)
		Expect(err).To(BeNil())
		Expect(match).To(BeTrue())
	})

	It("IsVersionAMatch: ComparisonGreaterThan returns false when version is not strictly greater than specified one", func() {
		classifier = getClassifierWithKubernetesConstraints(currentVersion, libsveltosv1beta1.ComparisonGreaterThan)
		Expect(testEnv.Create(context.TODO(), classifier)).To(Succeed())
		waitForObject(context.TODO(), testEnv.Client, classifier)

		evaluation.InitializeManagerWithSkip(context.TODO(), textlogger.NewLogger(textlogger.NewConfig(textlogger.Verbosity(1))), testEnv.Config, testEnv.Client,
			clusterNamespace, clusterName, clusterType, 10)

		manager := evaluation.GetManager()
		Expect(manager).ToNot(BeNil())

		match, err := evaluation.IsVersionAMatch(manager, context.TODO(),
			classifier)
		Expect(err).To(BeNil())
		Expect(match).To(BeFalse())
	})

	It("IsVersionAMatch: ComparisonGreaterThanOrEqualTo returns true when version is equal to specified one", func() {
		classifier = getClassifierWithKubernetesConstraints(currentVersion, libsveltosv1beta1.ComparisonGreaterThanOrEqualTo)
		Expect(testEnv.Create(context.TODO(), classifier)).To(Succeed())
		waitForObject(context.TODO(), testEnv.Client, classifier)

		evaluation.InitializeManagerWithSkip(context.TODO(), textlogger.NewLogger(textlogger.NewConfig(textlogger.Verbosity(1))), testEnv.Config, testEnv.Client,
			clusterNamespace, clusterName, clusterType, 10)

		manager := evaluation.GetManager()
		Expect(manager).ToNot(BeNil())

		match, err := evaluation.IsVersionAMatch(manager, context.TODO(),
			classifier)
		Expect(err).To(BeNil())
		Expect(match).To(BeTrue())
	})

	It("IsVersionAMatch: ComparisonGreaterThanOrEqualTo returns false when version is not equal/greater than specified one", func() {
		classifier = getClassifierWithKubernetesConstraints(higherVersion, libsveltosv1beta1.ComparisonGreaterThanOrEqualTo)
		Expect(testEnv.Create(context.TODO(), classifier)).To(Succeed())
		waitForObject(context.TODO(), testEnv.Client, classifier)

		evaluation.InitializeManagerWithSkip(context.TODO(), textlogger.NewLogger(textlogger.NewConfig(textlogger.Verbosity(1))), testEnv.Config, testEnv.Client,
			clusterNamespace, clusterName, clusterType, 10)

		manager := evaluation.GetManager()
		Expect(manager).ToNot(BeNil())

		match, err := evaluation.IsVersionAMatch(manager, context.TODO(),
			classifier)
		Expect(err).To(BeNil())
		Expect(match).To(BeFalse())
	})

	It("IsVersionAMatch: ComparisonLessThan returns true when version is strictly less than specified one", func() {
		classifier = getClassifierWithKubernetesConstraints(higherVersion, libsveltosv1beta1.ComparisonLessThan)
		Expect(testEnv.Create(context.TODO(), classifier)).To(Succeed())
		waitForObject(context.TODO(), testEnv.Client, classifier)

		evaluation.InitializeManagerWithSkip(context.TODO(), textlogger.NewLogger(textlogger.NewConfig(textlogger.Verbosity(1))), testEnv.Config, testEnv.Client,
			clusterNamespace, clusterName, clusterType, 10)

		manager := evaluation.GetManager()
		Expect(manager).ToNot(BeNil())

		match, err := evaluation.IsVersionAMatch(manager, context.TODO(),
			classifier)
		Expect(err).To(BeNil())
		Expect(match).To(BeTrue())
	})

	It("IsVersionAMatch: ComparisonLessThan returns false when version is not strictly less than specified one", func() {
		classifier = getClassifierWithKubernetesConstraints(currentVersion, libsveltosv1beta1.ComparisonLessThan)
		Expect(testEnv.Create(context.TODO(), classifier)).To(Succeed())
		waitForObject(context.TODO(), testEnv.Client, classifier)

		evaluation.InitializeManagerWithSkip(context.TODO(), textlogger.NewLogger(textlogger.NewConfig(textlogger.Verbosity(1))), testEnv.Config, testEnv.Client,
			clusterNamespace, clusterName, clusterType, 10)

		manager := evaluation.GetManager()
		Expect(manager).ToNot(BeNil())

		match, err := evaluation.IsVersionAMatch(manager, context.TODO(),
			classifier)
		Expect(err).To(BeNil())
		Expect(match).To(BeFalse())
	})

	It("IsVersionAMatch: ComparisonLessThanOrEqualTo returns true when version is equal to specified one", func() {
		classifier = getClassifierWithKubernetesConstraints(currentVersion, libsveltosv1beta1.ComparisonLessThanOrEqualTo)
		Expect(testEnv.Create(context.TODO(), classifier)).To(Succeed())
		waitForObject(context.TODO(), testEnv.Client, classifier)

		evaluation.InitializeManagerWithSkip(context.TODO(), textlogger.NewLogger(textlogger.NewConfig(textlogger.Verbosity(1))), testEnv.Config, testEnv.Client,
			clusterNamespace, clusterName, clusterType, 10)

		manager := evaluation.GetManager()
		Expect(manager).ToNot(BeNil())

		match, err := evaluation.IsVersionAMatch(manager, context.TODO(),
			classifier)
		Expect(err).To(BeNil())
		Expect(match).To(BeTrue())
	})

	It("IsVersionAMatch: ComparisonLessThanOrEqualTo returns false when version is not equal/less than specified one", func() {
		classifier = getClassifierWithKubernetesConstraints(lowerVersion, libsveltosv1beta1.ComparisonLessThanOrEqualTo)
		Expect(testEnv.Create(context.TODO(), classifier)).To(Succeed())
		waitForObject(context.TODO(), testEnv.Client, classifier)

		evaluation.InitializeManagerWithSkip(context.TODO(), textlogger.NewLogger(textlogger.NewConfig(textlogger.Verbosity(1))), testEnv.Config, testEnv.Client,
			clusterNamespace, clusterName, clusterType, 10)

		manager := evaluation.GetManager()
		Expect(manager).ToNot(BeNil())

		match, err := evaluation.IsVersionAMatch(manager, context.TODO(),
			classifier)
		Expect(err).To(BeNil())
		Expect(match).To(BeFalse())
	})

	It("IsVersionAMatch: multiple constraints returns true when all checks pass", func() {
		classifier = getClassifierWithKubernetesConstraints(lowerVersion, libsveltosv1beta1.ComparisonGreaterThanOrEqualTo)
		classifier.Spec.KubernetesVersionConstraints = append(classifier.Spec.KubernetesVersionConstraints,
			libsveltosv1beta1.KubernetesVersionConstraint{
				Comparison: string(libsveltosv1beta1.ComparisonLessThan),
				Version:    higherVersion,
			},
		)

		Expect(testEnv.Create(context.TODO(), classifier)).To(Succeed())
		waitForObject(context.TODO(), testEnv.Client, classifier)

		evaluation.InitializeManagerWithSkip(context.TODO(), textlogger.NewLogger(textlogger.NewConfig(textlogger.Verbosity(1))), testEnv.Config, testEnv.Client,
			clusterNamespace, clusterName, clusterType, 10)

		manager := evaluation.GetManager()
		Expect(manager).ToNot(BeNil())

		match, err := evaluation.IsVersionAMatch(manager, context.TODO(),
			classifier)
		Expect(err).To(BeNil())
		Expect(match).To(BeTrue())
	})

	It("IsVersionAMatch: multiple constraints returns false if at least one check fails", func() {
		classifier = getClassifierWithKubernetesConstraints(lowerVersion, libsveltosv1beta1.ComparisonGreaterThan)
		classifier.Spec.KubernetesVersionConstraints = append(classifier.Spec.KubernetesVersionConstraints,
			libsveltosv1beta1.KubernetesVersionConstraint{
				Comparison: string(libsveltosv1beta1.ComparisonLessThan),
				Version:    currentVersion,
			},
		)

		Expect(testEnv.Create(context.TODO(), classifier)).To(Succeed())
		waitForObject(context.TODO(), testEnv.Client, classifier)

		evaluation.InitializeManagerWithSkip(context.TODO(), textlogger.NewLogger(textlogger.NewConfig(textlogger.Verbosity(1))),
			testEnv.Config, testEnv.Client, clusterNamespace, clusterName, clusterType, 10)

		manager := evaluation.GetManager()
		Expect(manager).ToNot(BeNil())

		match, err := evaluation.IsVersionAMatch(manager, context.TODO(),
			classifier)
		Expect(err).To(BeNil())
		Expect(match).To(BeFalse())
	})

	It("createClassifierReport creates ClassifierReport", func() {
		classifier = getClassifierWithKubernetesConstraints(lowerVersion, libsveltosv1beta1.ComparisonGreaterThan)

		Expect(testEnv.Create(context.TODO(), classifier)).To(Succeed())
		waitForObject(context.TODO(), testEnv.Client, classifier)

		evaluation.InitializeManagerWithSkip(context.TODO(), textlogger.NewLogger(textlogger.NewConfig(textlogger.Verbosity(1))),
			nil, testEnv, clusterNamespace, clusterName, clusterType, 10)

		manager := evaluation.GetManager()
		Expect(manager).ToNot(BeNil())

		isMatch := true
		Expect(evaluation.CreateClassifierReport(manager, context.TODO(), classifier, isMatch)).To(Succeed())

		// Use Eventually so cache is in sync.
		Eventually(func() bool {
			classifierReport := &libsveltosv1beta1.ClassifierReport{}
			err := testEnv.Get(context.TODO(), types.NamespacedName{Namespace: utils.ReportNamespace, Name: classifier.Name},
				classifierReport)
			if err != nil {
				return false
			}
			return classifierReport.Status.Phase != nil
		}, timeout, pollingInterval).Should(BeTrue())

		verifyClassifierReport(testEnv, classifier, isMatch)
	})

	It("createClassifierReport updates ClassifierReport", func() {
		phase := libsveltosv1beta1.ReportProcessed
		isMatch := false
		classifier = getClassifierWithKubernetesConstraints(lowerVersion, libsveltosv1beta1.ComparisonGreaterThan)
		classifierReport := &libsveltosv1beta1.ClassifierReport{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: utils.ReportNamespace,
				Name:      classifier.Name,
			},
			Spec: libsveltosv1beta1.ClassifierReportSpec{
				ClassifierName: classifier.Name,
				Match:          !isMatch,
			},
			Status: libsveltosv1beta1.ClassifierReportStatus{
				Phase: &phase,
			},
		}
		initObjects := []client.Object{
			classifierReport,
			classifier,
		}

		c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(initObjects...).
			WithObjects(initObjects...).Build()

		evaluation.InitializeManagerWithSkip(context.TODO(), textlogger.NewLogger(textlogger.NewConfig(textlogger.Verbosity(1))),
			nil, c, clusterNamespace, clusterName, clusterType, 10)

		manager := evaluation.GetManager()
		Expect(manager).ToNot(BeNil())

		Expect(evaluation.CreateClassifierReport(manager, context.TODO(), classifier, isMatch)).To(Succeed())

		verifyClassifierReport(c, classifier, isMatch)
	})

	It("evaluateClassifierInstance creates ClassifierReport", func() {
		// Create node and classifier so cluster is a match
		isMatch := true
		classifier = getClassifierWithKubernetesConstraints(lowerVersion, libsveltosv1beta1.ComparisonGreaterThan)

		Expect(testEnv.Create(context.TODO(), classifier)).To(Succeed())
		waitForObject(context.TODO(), testEnv.Client, classifier)

		evaluation.InitializeManagerWithSkip(context.TODO(), textlogger.NewLogger(textlogger.NewConfig(textlogger.Verbosity(1))),
			testEnv.Config, testEnv.Client, clusterNamespace, clusterName, clusterType, 10)

		manager := evaluation.GetManager()
		Expect(manager).ToNot(BeNil())

		// Use Eventually so cache is in sync.
		// EvaluateClassifierInstance creates ClassifierReports and then fetches it to update status.
		// So use Eventually loop to make sure fetching (after creation) does not fail due to cache
		Eventually(func() error {
			return evaluation.EvaluateClassifierInstance(manager, context.TODO(), classifier.Name)
		}, timeout, pollingInterval).Should(BeNil())

		// EvaluateClassifierInstance has set Phase. Make sure we see Phase set before verifying its value
		// otherwise because of cache test might fail.
		Eventually(func() bool {
			classifierReport := &libsveltosv1beta1.ClassifierReport{}
			err := testEnv.Get(context.TODO(), types.NamespacedName{Namespace: utils.ReportNamespace, Name: classifier.Name},
				classifierReport)
			if err != nil {
				return false
			}
			return classifierReport.Status.Phase != nil
		}, timeout, pollingInterval).Should(BeTrue())

		verifyClassifierReport(testEnv.Client, classifier, isMatch)
	})

	It("getResourcesForResourceSelector returns resources matching a ResourceSelector", func() {
		namespace := randomString()
		classifier = &libsveltosv1beta1.Classifier{
			ObjectMeta: metav1.ObjectMeta{
				Name: randomString(),
			},
			Spec: libsveltosv1beta1.ClassifierSpec{
				ClassifierLabels: []libsveltosv1beta1.ClassifierLabel{
					{Key: randomString(), Value: randomString()},
				},
				DeployedResourceConstraint: &libsveltosv1beta1.DeployedResourceConstraint{
					ResourceSelectors: []libsveltosv1beta1.ResourceSelector{
						{
							Namespace: namespace,
							Group:     "",
							Version:   "v1",
							Kind:      "Pod",
						},
					},
				},
			},
		}

		Expect(testEnv.Create(context.TODO(), classifier)).To(Succeed())
		waitForObject(context.TODO(), testEnv.Client, classifier)

		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: namespace,
			},
		}
		Expect(testEnv.Create(context.TODO(), ns)).To(Succeed())
		waitForObject(context.TODO(), testEnv.Client, ns)

		count := 3
		for i := 0; i < count; i++ {
			pod := fmt.Sprintf(podTemplate, namespace, randomString())
			u, err := k8s_utils.GetUnstructured([]byte(pod))
			Expect(err).To(BeNil())
			Expect(testEnv.Create(context.TODO(), u)).To(Succeed())
			waitForObject(context.TODO(), testEnv.Client, u)
		}

		evaluation.InitializeManagerWithSkip(context.TODO(), textlogger.NewLogger(textlogger.NewConfig(textlogger.Verbosity(1))),
			testEnv.Config, testEnv.Client, randomString(), randomString(), libsveltosv1beta1.ClusterTypeCapi, 10)
		manager := evaluation.GetManager()

		logger := textlogger.NewLogger(textlogger.NewConfig(textlogger.Verbosity(1)))

		resources, err := evaluation.GetResourcesForResourceSelector(manager, context.TODO(),
			&classifier.Spec.DeployedResourceConstraint.ResourceSelectors[0], logger)
		Expect(err).To(BeNil())
		Expect(resources).ToNot(BeNil())
		Expect(len(resources)).To(Equal(count))

		// Add one more pod so now the number of pod
		pod := fmt.Sprintf(podTemplate, namespace, randomString())
		u, err := k8s_utils.GetUnstructured([]byte(pod))
		Expect(err).To(BeNil())
		Expect(testEnv.Create(context.TODO(), u)).To(Succeed())
		waitForObject(context.TODO(), testEnv.Client, u)

		resources, err = evaluation.GetResourcesForResourceSelector(manager, context.TODO(),
			&classifier.Spec.DeployedResourceConstraint.ResourceSelectors[0], logger)
		Expect(err).To(BeNil())
		Expect(resources).ToNot(BeNil())
		Expect(len(resources)).To(Equal(count + 1))
	})

	It("getResourcesForResourceSelector returns resources with labels matching the classifier", func() {
		namespace := randomString()
		key1 := randomString()
		value1 := randomString()
		key2 := randomString()
		value2 := randomString()
		classifier = &libsveltosv1beta1.Classifier{
			ObjectMeta: metav1.ObjectMeta{
				Name: randomString(),
			},
			Spec: libsveltosv1beta1.ClassifierSpec{
				ClassifierLabels: []libsveltosv1beta1.ClassifierLabel{
					{Key: randomString(), Value: randomString()},
				},
				DeployedResourceConstraint: &libsveltosv1beta1.DeployedResourceConstraint{
					ResourceSelectors: []libsveltosv1beta1.ResourceSelector{
						{
							Namespace: namespace,
							LabelFilters: []libsveltosv1beta1.LabelFilter{
								{Key: key1, Operation: libsveltosv1beta1.OperationEqual, Value: value1},
								{Key: key2, Operation: libsveltosv1beta1.OperationEqual, Value: value2},
							},
							Group:   "",
							Version: "v1",
							Kind:    "Pod",
						},
					},
				},
			},
		}

		Expect(testEnv.Create(context.TODO(), classifier)).To(Succeed())
		waitForObject(context.TODO(), testEnv.Client, classifier)

		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: namespace,
			},
		}
		Expect(testEnv.Create(context.TODO(), ns)).To(Succeed())
		waitForObject(context.TODO(), testEnv.Client, ns)

		// Create pod (labels are currently not a match for classifier)
		pod := fmt.Sprintf(podTemplate, namespace, randomString())
		u, err := k8s_utils.GetUnstructured([]byte(pod))
		u.SetLabels(map[string]string{key1: value1})
		Expect(err).To(BeNil())
		Expect(testEnv.Create(context.TODO(), u)).To(Succeed())
		waitForObject(context.TODO(), testEnv.Client, u)

		evaluation.InitializeManagerWithSkip(context.TODO(), textlogger.NewLogger(textlogger.NewConfig(textlogger.Verbosity(1))),
			testEnv.Config, testEnv.Client, randomString(), randomString(), libsveltosv1beta1.ClusterTypeSveltos, 10)
		manager := evaluation.GetManager()

		logger := textlogger.NewLogger(textlogger.NewConfig(textlogger.Verbosity(1)))

		resources, err := evaluation.GetResourcesForResourceSelector(manager, context.TODO(),
			&classifier.Spec.DeployedResourceConstraint.ResourceSelectors[0], logger)
		Expect(err).To(BeNil())
		Expect(len(resources)).To(BeZero())

		// Get all pods in namespace and add second label needed for pods to match classifier
		pods := &corev1.PodList{}
		listOptions := []client.ListOption{
			client.InNamespace(namespace),
		}
		Expect(testEnv.List(context.TODO(), pods, listOptions...)).To(Succeed())
		Expect(len(pods.Items)).To(Equal(1))

		for i := range pods.Items {
			pod := &pods.Items[i]
			pod.Labels[key2] = value2
			Expect(testEnv.Update(context.TODO(), pod)).To(Succeed())
		}

		// Use Eventually so cache is in sync
		Eventually(func() bool {
			resources, err := evaluation.GetResourcesForResourceSelector(manager, context.TODO(),
				&classifier.Spec.DeployedResourceConstraint.ResourceSelectors[0], logger)
			return err == nil && len(resources) == len(pods.Items)
		}, timeout, pollingInterval).Should(BeTrue())
	})

	It("IsMatchForResourceSelectorScript returns true when resource is match for evaluate", func() {
		podIP := "192.168.10.1"

		evaluate := `      function evaluate()
       hs = {}
       hs.matching = false
       hs.message = ""
       if obj.status ~= nil then
	       if obj.status.podIP ==  "192.168.10.1" then
		     hs.matching = true
         end
       end
       return hs
      end`

		namespace := randomString()
		u, err := k8s_utils.GetUnstructured([]byte(fmt.Sprintf(podTemplate, namespace, randomString())))
		Expect(err).To(BeNil())

		evaluation.InitializeManagerWithSkip(context.TODO(), textlogger.NewLogger(textlogger.NewConfig(textlogger.Verbosity(1))),
			testEnv.Config, testEnv.Client, randomString(), randomString(), libsveltosv1beta1.ClusterTypeSveltos, 10)
		manager := evaluation.GetManager()

		isMatch, err := evaluation.IsMatchForResourceSelectorScript(manager, u, evaluate,
			textlogger.NewLogger(textlogger.NewConfig(textlogger.Verbosity(1))))
		Expect(err).To(BeNil())
		Expect(isMatch).To(BeFalse())

		// Get all pods in namespace and add second label needed for pods to match classifier
		podList := &corev1.PodList{}
		listOptions := []client.ListOption{
			client.InNamespace(namespace),
		}
		Expect(testEnv.List(context.TODO(), podList, listOptions...)).To(Succeed())

		// Convert unstructured to Pod and set Status.PodIP
		pod := &corev1.Pod{}
		err = runtime.DefaultUnstructuredConverter.FromUnstructured(u.UnstructuredContent(), pod)
		Expect(err).To(BeNil())
		pod.Status.PodIP = podIP

		// Convert pod back to unstructured
		var content map[string]interface{}
		content, err = runtime.DefaultUnstructuredConverter.ToUnstructured(pod)
		Expect(err).To(BeNil())
		Expect(content).ToNot(BeNil())
		u.SetUnstructuredContent(content)

		// Use Eventually so cache is in sync
		isMatch, err = evaluation.IsMatchForResourceSelectorScript(manager, u, evaluate,
			textlogger.NewLogger(textlogger.NewConfig(textlogger.Verbosity(1))))
		Expect(err).To(BeNil())
		Expect(isMatch).To(BeTrue())
	})

	It("cleanClassifierReport removes classifierReport", func() {
		classifier = getClassifierWithKubernetesConstraints(lowerVersion, libsveltosv1beta1.ComparisonGreaterThan)
		classifierReport := &libsveltosv1beta1.ClassifierReport{
			ObjectMeta: metav1.ObjectMeta{
				Name:      classifier.Name,
				Namespace: utils.ReportNamespace,
			},
		}

		initObjects := []client.Object{
			classifierReport,
		}

		c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(initObjects...).
			WithObjects(initObjects...).Build()

		evaluation.InitializeManagerWithSkip(context.TODO(), textlogger.NewLogger(textlogger.NewConfig(textlogger.Verbosity(1))),
			nil, c, clusterNamespace, clusterName, clusterType, 10)

		manager := evaluation.GetManager()
		Expect(manager).ToNot(BeNil())

		Expect(evaluation.CleanClassifierReport(manager, context.TODO(), classifier.Name)).To(Succeed())

		err := c.Get(context.TODO(), types.NamespacedName{Name: classifier.Name, Namespace: utils.ReportNamespace}, classifierReport)
		Expect(err).ToNot(BeNil())
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
	})

	It("getManamegentClusterClient returns client to access management cluster", func() {
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: "projectsveltos",
			},
		}
		err := testEnv.Create(context.TODO(), ns)
		if err != nil {
			Expect(apierrors.IsAlreadyExists(err)).To(BeTrue())
		}
		waitForObject(context.TODO(), testEnv.Client, ns)

		evaluation.InitializeManagerWithSkip(context.TODO(), textlogger.NewLogger(textlogger.NewConfig(textlogger.Verbosity(1))),
			testEnv.Config, testEnv.Client, randomString(), randomString(), libsveltosv1beta1.ClusterTypeCapi, 10)
		manager := evaluation.GetManager()

		c, err := evaluation.GetManamegentClusterClient(manager, context.TODO(),
			textlogger.NewLogger(textlogger.NewConfig(textlogger.Verbosity(1))))
		Expect(err).To(BeNil())
		Expect(c).ToNot(BeNil())
	})

	It("sendClassifierReport sends classifierReport to management cluster", func() {
		classifier = getClassifierWithKubernetesConstraints(lowerVersion, libsveltosv1beta1.ComparisonGreaterThan)
		classifier.Spec.ClassifierLabels = []libsveltosv1beta1.ClassifierLabel{
			{Key: randomString(), Value: randomString()},
		}
		Expect(testEnv.Create(context.TODO(), classifier)).To(Succeed())
		waitForObject(context.TODO(), testEnv.Client, classifier)

		isMatch := true
		phase := libsveltosv1beta1.ReportDelivering
		classifierReport := &libsveltosv1beta1.ClassifierReport{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: utils.ReportNamespace,
				Name:      classifier.Name,
			},
			Spec: libsveltosv1beta1.ClassifierReportSpec{
				ClassifierName: classifier.Name,
				Match:          isMatch,
			},
			Status: libsveltosv1beta1.ClassifierReportStatus{
				Phase: &phase,
			},
		}

		Expect(testEnv.Create(context.TODO(), classifierReport)).To(Succeed())
		waitForObject(context.TODO(), testEnv.Client, classifierReport)

		clusterNamespace := utils.ReportNamespace
		clusterName := randomString()
		clusterType := libsveltosv1beta1.ClusterTypeCapi

		evaluation.InitializeManagerWithSkip(context.TODO(), textlogger.NewLogger(textlogger.NewConfig(textlogger.Verbosity(1))),
			testEnv.Config, testEnv.Client, clusterNamespace, clusterName, clusterType, 10)
		manager := evaluation.GetManager()

		Expect(evaluation.SendClassifierReport(manager, context.TODO(), classifier)).To(Succeed())

		// sendClassifierReport creates classifierReport in manager.clusterNamespace

		// Use Eventually so cache is in sync
		classifierReportName := libsveltosv1beta1.GetClassifierReportName(classifier.Name, clusterName, &clusterType)
		Eventually(func() error {
			currentClassifierReport := &libsveltosv1beta1.ClassifierReport{}
			return testEnv.Get(context.TODO(),
				types.NamespacedName{Namespace: clusterNamespace, Name: classifierReportName}, currentClassifierReport)
		}, timeout, pollingInterval).Should(BeNil())

		currentClassifierReport := &libsveltosv1beta1.ClassifierReport{}
		Expect(testEnv.Get(context.TODO(),
			types.NamespacedName{Namespace: clusterNamespace, Name: classifierReportName}, currentClassifierReport)).To(Succeed())
		Expect(currentClassifierReport.Spec.ClusterName).To(Equal(clusterName))
		Expect(currentClassifierReport.Spec.ClusterNamespace).To(Equal(clusterNamespace))
		Expect(currentClassifierReport.Spec.ClassifierName).To(Equal(classifier.Name))
		Expect(currentClassifierReport.Spec.ClusterType).To(Equal(clusterType))
		Expect(currentClassifierReport.Spec.Match).To(Equal(isMatch))
		v, ok := currentClassifierReport.Labels[libsveltosv1beta1.ClassifierReportClusterNameLabel]
		Expect(ok).To(BeTrue())
		Expect(v).To(Equal(clusterName))

		v, ok = currentClassifierReport.Labels[libsveltosv1beta1.ClassifierReportClusterTypeLabel]
		Expect(ok).To(BeTrue())
		Expect(v).To(Equal(strings.ToLower(string(libsveltosv1beta1.ClusterTypeCapi))))

		v, ok = currentClassifierReport.Labels[libsveltosv1beta1.ClassifierlNameLabel]
		Expect(ok).To(BeTrue())
		Expect(v).To(Equal(classifier.Name))
	})
})

func verifyClassifierReport(c client.Client, classifier *libsveltosv1beta1.Classifier, isMatch bool) {
	classifierReport := &libsveltosv1beta1.ClassifierReport{}
	Expect(c.Get(context.TODO(), types.NamespacedName{Namespace: utils.ReportNamespace, Name: classifier.Name},
		classifierReport)).To(Succeed())
	Expect(classifierReport.Labels).ToNot(BeNil())
	v, ok := classifierReport.Labels[libsveltosv1beta1.ClassifierlNameLabel]
	Expect(ok).To(BeTrue())
	Expect(v).To(Equal(classifier.Name))
	Expect(classifierReport.Spec.Match).To(Equal(isMatch))
	Expect(classifierReport.Spec.ClassifierName).To(Equal(classifier.Name))
	Expect(classifierReport.Status.Phase).ToNot(BeNil())
	Expect(*classifierReport.Status.Phase).To(Equal(libsveltosv1beta1.ReportWaitingForDelivery))
}
