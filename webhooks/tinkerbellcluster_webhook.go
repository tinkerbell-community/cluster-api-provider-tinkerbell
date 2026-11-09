/*
Copyright 2022 The Tinkerbell Authors.

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

package webhooks

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	infrastructurev1 "github.com/tinkerbell/cluster-api-provider-tinkerbell/api/v1beta2"
)

// TinkerbellCluster implements webhook interfaces for the TinkerbellCluster API type.
type TinkerbellCluster struct{}

var _ admission.Validator[*infrastructurev1.TinkerbellCluster] = &TinkerbellCluster{}

// SetupWebhookWithManager sets up and registers the webhook with the manager.
func (w *TinkerbellCluster) SetupWebhookWithManager(mgr ctrl.Manager) error {
	if err := ctrl.NewWebhookManagedBy(mgr, &infrastructurev1.TinkerbellCluster{}).
		WithValidator(w).
		Complete(); err != nil {
		return fmt.Errorf("setting up TinkerbellCluster webhook: %w", err)
	}

	return nil
}

// +kubebuilder:webhook:verbs=create;update,path=/validate-infrastructure-cluster-x-k8s-io-v1beta2-tinkerbellcluster,mutating=false,failurePolicy=fail,matchPolicy=Equivalent,groups=infrastructure.cluster.x-k8s.io,resources=tinkerbellclusters,versions=v1beta2,name=validation.tinkerbellcluster.infrastructure.cluster.x-k8s.io,sideEffects=None,admissionReviewVersions=v1;v1beta1

// ValidateCreate implements admission.Validator.
func (w *TinkerbellCluster) ValidateCreate(_ context.Context, obj *infrastructurev1.TinkerbellCluster) (admission.Warnings, error) {
	return validateClusterSource(obj)
}

// ValidateUpdate implements admission.Validator.
func (w *TinkerbellCluster) ValidateUpdate(_ context.Context, _, newTC *infrastructurev1.TinkerbellCluster) (admission.Warnings, error) {
	return validateClusterSource(newTC)
}

// validateClusterSource checks what the cluster's machines provision from by default, and warns
// about deprecated template fields.
func validateClusterSource(tc *infrastructurev1.TinkerbellCluster) (admission.Warnings, error) {
	path := field.NewPath("spec")

	return deprecationWarnings(path, tc.Spec.TemplateInline, tc.Spec.TemplateRef),
		aggregateObjErrors(tc.GroupVersionKind().GroupKind(), tc.Name, validateSource(path, tc.Spec.Tasks, tc.Spec.TemplateInline, tc.Spec.TemplateRef))
}

// ValidateDelete implements admission.Validator.
func (w *TinkerbellCluster) ValidateDelete(_ context.Context, _ *infrastructurev1.TinkerbellCluster) (admission.Warnings, error) {
	return nil, nil
}
