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

package utils_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"k8s.io/klog/v2/klogr"

	"github.com/projectsveltos/classifier-agent/pkg/utils"
)

var _ = Describe("Manager", func() {
	BeforeEach(func() {
		var err error
		scheme, err = setupScheme()
		Expect(err).ToNot(HaveOccurred())
	})

	It("GetKubernetesVersion returns cluster Kubernetes version", func() {
		version, err := utils.GetKubernetesVersion(context.TODO(), testEnv.Config, klogr.New())
		Expect(err).To(BeNil())
		Expect(version).ToNot(BeEmpty())
	})
})
