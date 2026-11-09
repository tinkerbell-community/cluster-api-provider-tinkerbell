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

// TinkerbellClusterTemplate implements webhook interfaces for the TinkerbellClusterTemplate API type.
type TinkerbellClusterTemplate struct{}

var _ admission.Validator[*infrastructurev1.TinkerbellClusterTemplate] = &TinkerbellClusterTemplate{}

// SetupWebhookWithManager sets up and registers the webhook with the manager.
func (w *TinkerbellClusterTemplate) SetupWebhookWithManager(mgr ctrl.Manager) error {
	if err := ctrl.NewWebhookManagedBy(mgr, &infrastructurev1.TinkerbellClusterTemplate{}).
		WithValidator(w).
		Complete(); err != nil {
		return fmt.Errorf("setting up TinkerbellClusterTemplate webhook: %w", err)
	}

	return nil
}

// +kubebuilder:webhook:verbs=create;update,path=/validate-infrastructure-cluster-x-k8s-io-v1beta2-tinkerbellclustertemplate,mutating=false,failurePolicy=fail,matchPolicy=Equivalent,groups=infrastructure.cluster.x-k8s.io,resources=tinkerbellclustertemplates,versions=v1beta2,name=validation.tinkerbellclustertemplate.infrastructure.cluster.x-k8s.io,sideEffects=None,admissionReviewVersions=v1;v1beta1

// ValidateCreate implements admission.Validator.
func (w *TinkerbellClusterTemplate) ValidateCreate(_ context.Context, obj *infrastructurev1.TinkerbellClusterTemplate) (admission.Warnings, error) {
	return validateClusterTemplateSource(obj)
}

// ValidateUpdate implements admission.Validator. See validateTemplateSpecImmutable for what is
// frozen and why topology dry-run requests are exempted.
func (w *TinkerbellClusterTemplate) ValidateUpdate(
	ctx context.Context,
	oldTCT *infrastructurev1.TinkerbellClusterTemplate,
	newTCT *infrastructurev1.TinkerbellClusterTemplate,
) (admission.Warnings, error) {
	if err := validateTemplateSpecImmutable(
		ctx, "TinkerbellClusterTemplate", newTCT,
		oldTCT.Spec.Template.Spec, newTCT.Spec.Template.Spec,
	); err != nil {
		return nil, err
	}

	return validateClusterTemplateSource(newTCT)
}

// validateClusterTemplateSource checks what the machines of clusters stamped from tct provision
// from by default, and warns about deprecated template fields.
func validateClusterTemplateSource(tct *infrastructurev1.TinkerbellClusterTemplate) (admission.Warnings, error) {
	path := field.NewPath("spec", "template", "spec")
	spec := tct.Spec.Template.Spec

	return deprecationWarnings(path, spec.TemplateInline, spec.TemplateRef),
		aggregateObjErrors(tct.GroupVersionKind().GroupKind(), tct.Name, validateSource(path, spec.Tasks, spec.TemplateInline, spec.TemplateRef))
}

// ValidateDelete implements admission.Validator.
func (w *TinkerbellClusterTemplate) ValidateDelete(_ context.Context, _ *infrastructurev1.TinkerbellClusterTemplate) (admission.Warnings, error) {
	return nil, nil
}
