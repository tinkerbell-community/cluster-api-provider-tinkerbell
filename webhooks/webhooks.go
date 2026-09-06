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

// Package webhooks implements admission webhooks for CAPT API types.
package webhooks

import (
	"context"
	"fmt"
	"reflect"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/cluster-api/util/topology"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func aggregateObjErrors(gk schema.GroupKind, name string, allErrs field.ErrorList) error {
	if len(allErrs) == 0 {
		return nil
	}

	return apierrors.NewInvalid(
		gk,
		name,
		allErrs,
	)
}

// validateTemplateSpecImmutable enforces that the spec.template.spec of a Cluster API template
// object is immutable. spec.template.metadata is deliberately left mutable: it holds the labels
// and annotations propagated to the objects cloned from the template, which the upstream
// infrastructure providers allow to be updated in place.
//
// The CAPI topology controller detects template changes by dry-run applying the desired template
// and treats a webhook rejection as a hard error, which would permanently wedge ClusterClass-driven
// template rotation, so its dry-run requests are exempted from the check. A plain
// `kubectl apply --dry-run=server` carries no topology annotation and still gets the error.
func validateTemplateSpecImmutable[S any](
	ctx context.Context,
	kind string,
	newObj metav1.Object,
	oldSpec, newSpec S,
) error {
	req, err := admission.RequestFromContext(ctx)
	if err != nil {
		return apierrors.NewBadRequest(fmt.Sprintf("expected an admission.Request inside context: %v", err))
	}

	if topology.IsDryRunRequest(req, newObj) {
		return nil
	}

	if !reflect.DeepEqual(oldSpec, newSpec) {
		return apierrors.NewBadRequest(fmt.Sprintf("%s spec.template.spec is immutable", kind))
	}

	return nil
}
