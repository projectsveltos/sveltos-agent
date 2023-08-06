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

package evaluation_test

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2/klogr"

	libsveltosv1alpha1 "github.com/projectsveltos/libsveltos/api/v1alpha1"
	libsveltosset "github.com/projectsveltos/libsveltos/lib/set"
	"github.com/projectsveltos/sveltos-agent/pkg/evaluation"
	"github.com/projectsveltos/sveltos-agent/pkg/utils"
)

var _ = Describe("Manager: reloader evaluation", func() {
	var reloader *libsveltosv1alpha1.Reloader
	var clusterNamespace string
	var clusterName string
	var clusterType libsveltosv1alpha1.ClusterType

	BeforeEach(func() {
		evaluation.Reset()
		clusterNamespace = utils.ReportNamespace
		clusterName = randomString()
		clusterType = libsveltosv1alpha1.ClusterTypeCapi
	})

	BeforeEach(func() {
		evaluation.Reset()
	})

	It("getResourceVolumeMounts returns all mounted ConfigMaps/Secrets", func() {
		configMapName := randomString()
		secretName := randomString()

		depl := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      randomString(),
				Namespace: randomString(),
			},
			Spec: appsv1.DeploymentSpec{
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Volumes: []corev1.Volume{
							{
								Name: randomString(),
								VolumeSource: corev1.VolumeSource{
									ConfigMap: &corev1.ConfigMapVolumeSource{
										LocalObjectReference: corev1.LocalObjectReference{
											Name: configMapName,
										},
									},
								},
							},
							{
								Name: randomString(),
								VolumeSource: corev1.VolumeSource{
									Secret: &corev1.SecretVolumeSource{
										SecretName: secretName,
									},
								},
							},
						},
					},
				},
			},
		}

		reloader = &libsveltosv1alpha1.Reloader{
			ObjectMeta: metav1.ObjectMeta{
				Name: randomString(),
			},
			Spec: libsveltosv1alpha1.ReloaderSpec{
				ReloaderInfo: []libsveltosv1alpha1.ReloaderInfo{
					{
						Kind:      "Deployment",
						Namespace: depl.Namespace,
						Name:      depl.Name,
					},
				},
			},
		}

		initObjects := []client.Object{
			depl, reloader,
		}

		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(initObjects...).Build()

		evaluation.InitializeManagerWithSkip(context.TODO(), klogr.New(), nil, c,
			clusterNamespace, clusterName, clusterType, 10)

		manager := evaluation.GetManager()
		Expect(manager).ToNot(BeNil())

		result, err := evaluation.GetResourceVolumeMounts(manager, context.TODO(), &reloader.Spec.ReloaderInfo[0])
		Expect(err).To(BeNil())
		Expect(len(result)).To(Equal(2))
		Expect(result).To(ContainElement(
			corev1.ObjectReference{
				Kind:       "ConfigMap",
				APIVersion: corev1.SchemeGroupVersion.String(),
				Namespace:  depl.Namespace,
				Name:       configMapName,
			}))
		Expect(result).To(ContainElement(
			corev1.ObjectReference{
				Kind:       "Secret",
				APIVersion: corev1.SchemeGroupVersion.String(),
				Namespace:  depl.Namespace,
				Name:       secretName,
			}))
	})

	It("evaluateReloaderInstance updates maps correctly", func() {
		secretName1 := randomString()
		secretName2 := randomString()

		statefulSet := &appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      randomString(),
				Namespace: randomString(),
			},
			Spec: appsv1.StatefulSetSpec{
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Volumes: []corev1.Volume{
							{
								Name: randomString(),
								VolumeSource: corev1.VolumeSource{
									Secret: &corev1.SecretVolumeSource{
										SecretName: secretName1,
									},
								},
							},
							{
								Name: randomString(),
								VolumeSource: corev1.VolumeSource{
									Secret: &corev1.SecretVolumeSource{
										SecretName: secretName2,
									},
								},
							},
						},
					},
				},
			},
		}

		reloader = &libsveltosv1alpha1.Reloader{
			ObjectMeta: metav1.ObjectMeta{
				Name: randomString(),
			},
			Spec: libsveltosv1alpha1.ReloaderSpec{
				ReloaderInfo: []libsveltosv1alpha1.ReloaderInfo{
					{
						Kind:      "StatefulSet",
						Namespace: statefulSet.Namespace,
						Name:      statefulSet.Name,
					},
				},
			},
		}

		initObjects := []client.Object{
			reloader, statefulSet,
		}

		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(initObjects...).Build()

		evaluation.InitializeManagerWithSkip(context.TODO(), klogr.New(), nil, c,
			clusterNamespace, clusterName, clusterType, 10)

		manager := evaluation.GetManager()
		Expect(manager).ToNot(BeNil())

		Expect(evaluation.EvaluateReloaderInstance(manager, context.TODO(), reloader.Name)).To(BeNil())

		reloaderObjRef := corev1.ObjectReference{
			Kind:       libsveltosv1alpha1.ReloaderKind,
			APIVersion: libsveltosv1alpha1.GroupVersion.String(),
			Name:       reloader.Name,
		}

		statefulSetObjRef := corev1.ObjectReference{
			Kind:       "StatefulSet",
			APIVersion: appsv1.SchemeGroupVersion.String(),
			Name:       statefulSet.Name,
			Namespace:  statefulSet.Namespace,
		}

		reloaderMap := evaluation.GetReloaderMap()
		Expect(len(reloaderMap)).To(Equal(1))
		v, ok := reloaderMap[reloaderObjRef]
		Expect(ok).To(BeTrue())
		Expect(v.Len()).To(Equal(1))

		Expect(v.Has(&statefulSetObjRef)).To(BeTrue())

		resourceMap := evaluation.GetResourceMap()
		Expect(len(resourceMap)).To(Equal(1))
		v, ok = resourceMap[statefulSetObjRef]
		Expect(ok).To(BeTrue())
		Expect(v.Has(&reloaderObjRef)).To(BeTrue())

		volumeMap := evaluation.GetVolumeMap()
		Expect(len(volumeMap)).To(Equal(2))

		for i := range statefulSet.Spec.Template.Spec.Volumes {
			volume := statefulSet.Spec.Template.Spec.Volumes[i]
			if volume.Secret == nil {
				continue
			}
			objRef := corev1.ObjectReference{
				APIVersion: corev1.SchemeGroupVersion.String(),
				Name:       volume.Secret.SecretName,
				Namespace:  statefulSet.Namespace,
				Kind:       "Secret",
			}

			v, ok = volumeMap[objRef]
			Expect(ok).To(BeTrue())
			Expect(v.Has(&statefulSetObjRef)).To(BeTrue())
		}

		// Delete reloader. Maps need to be cleaned after that
		Expect(c.Delete(context.TODO(), reloader)).To(Succeed())
		Expect(evaluation.EvaluateReloaderInstance(manager, context.TODO(), reloader.Name)).To(BeNil())

		reloaderMap = evaluation.GetReloaderMap()
		Expect(len(reloaderMap)).To(Equal(0))
		resourceMap = evaluation.GetResourceMap()
		Expect(len(resourceMap)).To(Equal(0))
		volumeMap = evaluation.GetVolumeMap()
		Expect(len(volumeMap)).To(Equal(0))
	})

	It("updateReloaderReport updates ReloaderReport Spec and Status", func() {
		configMapRef := &corev1.ObjectReference{
			Namespace:  randomString(),
			Name:       randomString(),
			Kind:       "ConfigMap",
			APIVersion: corev1.SchemeGroupVersion.String(),
		}

		deplRef := &corev1.ObjectReference{
			Namespace:  randomString(),
			Name:       randomString(),
			Kind:       "Deployment",
			APIVersion: appsv1.SchemeGroupVersion.String(),
		}

		statefulSetRef := &corev1.ObjectReference{
			Namespace:  randomString(),
			Name:       randomString(),
			Kind:       "StatefulSet",
			APIVersion: appsv1.SchemeGroupVersion.String(),
		}

		// Create an empty ReloaderReport
		reloaderReport := &libsveltosv1alpha1.ReloaderReport{
			ObjectMeta: metav1.ObjectMeta{
				Name: libsveltosv1alpha1.GetReloaderReportName(configMapRef.Kind,
					configMapRef.Namespace, configMapRef.Name, clusterName, &clusterType),
				Namespace: utils.ReportNamespace,
			},
		}
		initObjects := []client.Object{
			reloaderReport,
		}

		c := fake.NewClientBuilder().WithScheme(scheme).
			WithStatusSubresource(initObjects...).WithObjects(initObjects...).Build()

		evaluation.InitializeManagerWithSkip(context.TODO(), klogr.New(), nil, c,
			clusterNamespace, clusterName, clusterType, 10)

		manager := evaluation.GetManager()
		Expect(manager).ToNot(BeNil())

		volumeMap := evaluation.GetVolumeMap()
		volumeMap[*configMapRef] = &libsveltosset.Set{}
		volumeMap[*configMapRef].Insert(deplRef)
		volumeMap[*configMapRef].Insert(statefulSetRef)

		Expect(evaluation.UpdateReloaderReport(manager, context.TODO(), configMapRef)).To(BeNil())

		currentReloaderReport := &libsveltosv1alpha1.ReloaderReport{}
		Expect(c.Get(context.TODO(),
			types.NamespacedName{Namespace: utils.ReportNamespace, Name: reloaderReport.Name},
			currentReloaderReport)).To(Succeed())
		Expect(len(currentReloaderReport.Spec.ResourcesToReload)).To(Equal(2))

		Expect(currentReloaderReport.Spec.ResourcesToReload).To(
			ContainElement(libsveltosv1alpha1.ReloaderInfo{
				Kind:      deplRef.Kind,
				Namespace: deplRef.Namespace,
				Name:      deplRef.Name,
			}))

		Expect(currentReloaderReport.Spec.ResourcesToReload).To(
			ContainElement(libsveltosv1alpha1.ReloaderInfo{
				Kind:      statefulSetRef.Kind,
				Namespace: statefulSetRef.Namespace,
				Name:      statefulSetRef.Name,
			}))
	})

	It("evaluateMountedResource updates ReloaderReport", func() {
		secretRef := &corev1.ObjectReference{
			Namespace:  randomString(),
			Name:       randomString(),
			Kind:       "Secret",
			APIVersion: corev1.SchemeGroupVersion.String(),
		}

		daemonSetRef := &corev1.ObjectReference{
			Namespace:  randomString(),
			Name:       randomString(),
			Kind:       "DaemonSet",
			APIVersion: appsv1.SchemeGroupVersion.String(),
		}

		// Create an empty ReloaderReport
		reloaderReport := &libsveltosv1alpha1.ReloaderReport{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: utils.ReportNamespace,
				Name: libsveltosv1alpha1.GetReloaderReportName(secretRef.Kind,
					secretRef.Namespace, secretRef.Name, clusterName, &clusterType),
			},
		}
		initObjects := []client.Object{
			reloaderReport,
		}

		c := fake.NewClientBuilder().WithScheme(scheme).
			WithStatusSubresource(initObjects...).WithObjects(initObjects...).Build()

		evaluation.InitializeManagerWithSkip(context.TODO(), klogr.New(), nil, c,
			clusterNamespace, clusterName, clusterType, 10)

		manager := evaluation.GetManager()
		Expect(manager).ToNot(BeNil())

		volumeMap := evaluation.GetVolumeMap()
		volumeMap[*secretRef] = &libsveltosset.Set{}
		volumeMap[*secretRef].Insert(daemonSetRef)

		resourceInfo := fmt.Sprintf("%s/%s", secretRef.Namespace, secretRef.Name)
		Expect(evaluation.EvaluateMountedResource(manager, context.TODO(),
			secretRef.Kind, resourceInfo)).To(Succeed())

		currentReloaderReport := &libsveltosv1alpha1.ReloaderReport{}
		Expect(c.Get(context.TODO(),
			types.NamespacedName{Namespace: utils.ReportNamespace, Name: reloaderReport.Name},
			currentReloaderReport)).To(Succeed())
		Expect(len(currentReloaderReport.Spec.ResourcesToReload)).To(Equal(1))

		Expect(currentReloaderReport.Spec.ResourcesToReload).To(
			ContainElement(libsveltosv1alpha1.ReloaderInfo{
				Kind:      daemonSetRef.Kind,
				Namespace: daemonSetRef.Namespace,
				Name:      daemonSetRef.Name,
			}))
	})

	It("createReloaderReport creates ReloaderReport", func() {
		configMapRef := &corev1.ObjectReference{
			Namespace:  randomString(),
			Name:       randomString(),
			Kind:       "ConfigMap",
			APIVersion: corev1.SchemeGroupVersion.String(),
		}

		c := fake.NewClientBuilder().WithScheme(scheme).Build()

		evaluation.InitializeManagerWithSkip(context.TODO(), klogr.New(), nil, c,
			clusterNamespace, clusterName, clusterType, 10)

		manager := evaluation.GetManager()
		Expect(manager).ToNot(BeNil())

		reloaderReport, err := evaluation.CreateReloaderReport(manager, context.TODO(), configMapRef)
		Expect(err).To(BeNil())
		Expect(len(reloaderReport.Spec.ResourcesToReload)).To(BeZero())

		Expect(reloaderReport.Labels).ToNot(BeNil())
		v, ok := reloaderReport.Labels[libsveltosv1alpha1.ReloaderReportClusterNameLabel]
		Expect(ok).To(BeTrue())
		Expect(v).To(Equal(clusterName))

		v, ok = reloaderReport.Labels[libsveltosv1alpha1.ReloaderReportClusterTypeLabel]
		Expect(ok).To(BeTrue())
		Expect(v).To(Equal(strings.ToLower(string(clusterType))))

		Expect(reloaderReport.Annotations).ToNot(BeNil())
		v, ok = reloaderReport.Annotations[libsveltosv1alpha1.ReloaderReportResourceKindAnnotation]
		Expect(ok).To(BeTrue())
		Expect(v).To(Equal(strings.ToLower(configMapRef.Kind)))

		v, ok = reloaderReport.Annotations[libsveltosv1alpha1.ReloaderReportResourceNameAnnotation]
		Expect(ok).To(BeTrue())
		Expect(v).To(Equal(configMapRef.Name))

		v, ok = reloaderReport.Annotations[libsveltosv1alpha1.ReloaderReportResourceNamespaceAnnotation]
		Expect(ok).To(BeTrue())
		Expect(v).To(Equal(configMapRef.Namespace))
	})

	It("sendReloaderReport sends reloaderReport to management cluster", func() {
		// Create namespace. This represents the cluster namespace in the management cluster
		// ReloaderReport will be created by sendReloaderReportToMgtmCluster in the cluster
		// namespace in the management cluster.
		// ReloaderReport is instead in the projectsveltos namespace in the managed cluster.
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: randomString(),
			},
		}
		Expect(testEnv.Create(context.TODO(), ns)).To(Succeed())
		waitForObject(context.TODO(), testEnv.Client, ns)

		configMapRef := &corev1.ObjectReference{
			Namespace:  randomString(),
			Name:       randomString(),
			Kind:       "ConfigMap",
			APIVersion: corev1.SchemeGroupVersion.String(),
		}

		name := libsveltosv1alpha1.GetReloaderReportName(configMapRef.Kind,
			configMapRef.Namespace, configMapRef.Name, clusterName, &clusterType)

		phase := libsveltosv1alpha1.ReportDelivering
		reloaderReport := &libsveltosv1alpha1.ReloaderReport{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: utils.ReportNamespace,
				Name:      name,
				Labels: libsveltosv1alpha1.GetReloaderReportLabels(
					clusterName, &clusterType),
				Annotations: libsveltosv1alpha1.GetReloaderReportAnnotations(
					configMapRef.Kind, configMapRef.Namespace, configMapRef.Name),
			},
			Spec: libsveltosv1alpha1.ReloaderReportSpec{
				ResourcesToReload: []libsveltosv1alpha1.ReloaderInfo{
					{Kind: "Deployment", Namespace: randomString(), Name: randomString()},
					{Kind: "DaemonSet", Namespace: randomString(), Name: randomString()},
				},
			},
			Status: libsveltosv1alpha1.ReloaderReportStatus{
				Phase: &phase,
			},
		}

		Expect(testEnv.Create(context.TODO(), reloaderReport)).To(Succeed())
		waitForObject(context.TODO(), testEnv.Client, reloaderReport)

		evaluation.InitializeManagerWithSkip(context.TODO(), klogr.New(), testEnv.Config, testEnv.Client,
			ns.Name, clusterName, clusterType, 10)
		manager := evaluation.GetManager()

		Expect(evaluation.SendReloaderReportToMgtmCluster(manager, context.TODO(), configMapRef)).To(Succeed())

		// Event though testEnv is used for both management and managed cluster, ReloaderReport
		// is present in:
		// - projectsveltos namespace in the managed cluster
		// - cluster namespace in the management cluster

		// Verify ReloaderReport was created in the management cluster
		Eventually(func() error {
			mgmtReloaderReport := &libsveltosv1alpha1.ReloaderReport{}
			return testEnv.Get(context.TODO(),
				types.NamespacedName{Namespace: ns.Name, Name: name},
				mgmtReloaderReport)
		}, timeout, pollingInterval).Should(BeNil())

		mgmtReloaderReport := &libsveltosv1alpha1.ReloaderReport{}
		Expect(testEnv.Get(context.TODO(),
			types.NamespacedName{Namespace: ns.Name, Name: name},
			mgmtReloaderReport)).To(Succeed())
		By("Verifying labels and annotations are set")
		Expect(mgmtReloaderReport.Labels).ToNot(BeNil())
		Expect(reflect.DeepEqual(mgmtReloaderReport.Labels,
			reloaderReport.Labels)).To(BeTrue())
		Expect(mgmtReloaderReport.Annotations).ToNot(BeNil())
		Expect(reflect.DeepEqual(mgmtReloaderReport.Annotations,
			reloaderReport.Annotations)).To(BeTrue())

		// After sending ReloaderReport to management cluster, it gets deleted in the
		// managed cluster
		Eventually(func() bool {
			managedReloaderReport := &libsveltosv1alpha1.ReloaderReport{}
			err := testEnv.Get(context.TODO(),
				types.NamespacedName{Namespace: utils.ReportNamespace, Name: name},
				managedReloaderReport)
			if err != nil {
				return apierrors.IsNotFound(err)
			}
			return false
		}, timeout, pollingInterval).Should(BeTrue())
	})
})
