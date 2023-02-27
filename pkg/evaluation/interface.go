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

package evaluation

type ClassifierInterface interface {
	// EvaluateClassifier requests a classifier to be
	// evaluated.
	// This method is idempotent. Invoking multiple times
	// on same instance, will queue Classifier to be evaluated
	// only once.
	// After an evaluation, a Classifier is automatically removed
	// from queue containing all Classifiers which requires evaluation.
	EvaluateClassifier(classifierName string)

	// EvaluateHealthCheck requests a healthCheck to be
	// evaluated.
	// This method is idempotent. Invoking multiple times
	// on same instance, will queue HealthCheck to be evaluated
	// only once.
	// After an evaluation, a HealthCheck is automatically removed
	// from queue containing all HealthChecks which requires evaluation.
	EvaluateHealthCheck(healthCheckName string)

	// ReEvaluateResourceToWatch requests to rebuild the GVKs to watch.
	// This evaluation is done asynchronously when at least one request
	// to re-evaluate has been received
	ReEvaluateResourceToWatch()
}
