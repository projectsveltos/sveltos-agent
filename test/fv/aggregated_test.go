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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	libsveltosv1beta1 "github.com/projectsveltos/libsveltos/api/v1beta1"
	"github.com/projectsveltos/sveltos-agent/pkg/utils"
)

const (
	luaNotMountedConfigMaps = `      function getKey(namespace, name)
        return namespace .. ":" .. name
      end 

      function configMapsUsedByPods(pods)
        local podConfigMaps = {}

        for _, pod in ipairs(pods) do
          if pod.spec.containers ~= nil then
            for _, container in ipairs(pod.spec.containers) do
              
              if container.env ~= nil then
                for _, env in ipairs(container.env) do
                  if env.valueFrom ~= nil and env.valueFrom.configMapKeyRef ~= nil then
                    key = getKey(pod.metadata.namespace, env.valueFrom.configMapKeyRef.name)
                    podConfigMaps[key] = true
                  end
                end
              end

              if container.envFrom ~= nil then
                for _, envFrom in ipairs(container.envFrom) do
                  if envFrom.configMapRef ~= nil then
                    key = getKey(pod.metadata.namespace, envFrom.configMapRef.name)
                    podConfigMaps[key] = true
                  end
                end
              end  
            end
          end

          if pod.spec.initContainers ~= nil then
            for _, initContainer in ipairs(pod.spec.initContainers) do

              if initContainer.env ~= nil then
                for _, env in ipairs(initContainer.env) do
                  if env.valueFrom ~= nil and env.valueFrom.configMapKeyRef ~= nil then
                    key = getKey(pod.metadata.namespace, env.valueFrom.configMapKeyRef.name)
                    podConfigMaps[key] = true
                  end
                end
              end

              if initContainer.envFrom ~= nil then
                for _, envFrom in ipairs(initContainer.envFrom) do
                  if envFrom.configMapRef ~= nil then
                    key = getKey(pod.metadata.namespace,envFrom.configMapRef.name)
                    podConfigMaps[key] = true
                  end
                end
              end  

            end
          end    

          if pod.spec.volumes ~= nil then  
            for _, volume in ipairs(pod.spec.volumes) do
              if volume.configMap ~= nil then
                key = getKey(pod.metadata.namespace,volume.configMap.name)
                podConfigMaps[key] = true
              end

              if volume.projected ~= nil and volume.projected.sources ~= nil then
                for _, projectedResource in ipairs(volume.projected.sources) do
                  if projectedResource.configMap ~= nil then
                    key = getKey(pod.metadata.namespace,projectedResource.configMap.name)
                    podConfigMaps[key] = true
                  end
                end
              end
            end
          end
        end  

        return podConfigMaps
      end

      function evaluate()
        local hs = {}
        hs.message = ""

        local pods = {}
        local configMaps = {}
        local unusedConfigMaps = {}

        -- Separate configMaps and podsfrom the resources
        for _, resource in ipairs(resources) do
            local kind = resource.kind
            if kind == "ConfigMap" then
              table.insert(configMaps, resource)
            elseif kind == "Pod" then
              table.insert(pods, resource)
            end
        end

        podConfigMaps = configMapsUsedByPods(pods)

        for _, configMap in ipairs(configMaps) do
          key = getKey(configMap.metadata.namespace,configMap.metadata.name)
          if not podConfigMaps[key] then
            table.insert(unusedConfigMaps, configMap)
          end
        end

        if #unusedConfigMaps > 0 then
          hs.resources = unusedConfigMaps
        end
        return hs
      end`
)

var _ = Describe("Aggregated Events", func() {
	var ns string
	const (
		namePrefix = "aggregated-"
	)

	BeforeEach(func() {
		ns = randomString()

		namespace := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: ns,
			},
		}
		Expect(k8sClient.Create(context.TODO(), namespace)).To(Succeed())
	})

	AfterEach(func() {
		namespace := &corev1.Namespace{}
		Expect(k8sClient.Get(context.TODO(), types.NamespacedName{Name: ns}, namespace)).To(Succeed())

		Expect(k8sClient.Delete(context.TODO(), namespace)).To(Succeed())
	})

	It("Evaluate eventSource with AggregatedSelection", Label("FV"), func() {
		By("Creating a ConfigMap")
		configMap := corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "default",
				Name:      randomString(),
			},
			Data: map[string]string{
				randomString(): randomString(),
			},
		}
		Expect(k8sClient.Create(context.TODO(), &configMap)).To(Succeed())

		eventSource := libsveltosv1beta1.EventSource{
			ObjectMeta: metav1.ObjectMeta{
				Name: namePrefix + randomString(),
				Annotations: map[string]string{
					"projectsveltos.io/deployed-by-sveltos": "ok",
				},
			},
			Spec: libsveltosv1beta1.EventSourceSpec{
				ResourceSelectors: []libsveltosv1beta1.ResourceSelector{
					{
						Group:   "",
						Version: "v1",
						Kind:    "Pod",
					},
					{
						Group:   "",
						Version: "v1",
						Kind:    "ConfigMap",
					},
				},
				AggregatedSelection: luaNotMountedConfigMaps,
			},
		}

		By(fmt.Sprintf("Creating eventSource %s. ConfigMaps not mounted by Pods are a match", eventSource.Name))
		Expect(k8sClient.Create(context.TODO(), &eventSource)).To(Succeed())

		By(fmt.Sprintf("Verifying EventReport has match ConfigMap %s/%s", configMap.Namespace, configMap.Name))
		verifyEventReport("ConfigMap", configMap.Namespace, configMap.Name, eventSource.Name)

		By(fmt.Sprintf("Deleting EventSource %s", eventSource.Name))
		Expect(k8sClient.Get(context.TODO(), types.NamespacedName{Name: eventSource.Name}, &eventSource))
		Expect(k8sClient.Delete(context.TODO(), &eventSource)).To(Succeed())

		By("Verifying EventReport is marked for deletion")
		Eventually(func() bool {
			eventReport := &libsveltosv1beta1.EventReport{}
			err := k8sClient.Get(context.TODO(),
				types.NamespacedName{Namespace: utils.ReportNamespace, Name: eventSource.Name}, eventReport)
			if err != nil {
				return apierrors.IsNotFound(err)
			}
			return !eventReport.DeletionTimestamp.IsZero()
		}, timeout, pollingInterval).Should(BeTrue())
	})
})
