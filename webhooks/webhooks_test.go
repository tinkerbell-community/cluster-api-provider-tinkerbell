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

package webhooks_test

import (
	"context"
	"testing"

	. "github.com/onsi/gomega" //nolint:revive // one day we will remove gomega
	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// admissionContext returns a context carrying an UPDATE admission.Request, the way
// controller-runtime's admission handler populates it before invoking a validator.
func admissionContext(dryRun bool) context.Context {
	return admission.NewContextWithRequest(context.Background(), admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			Operation: admissionv1.Update,
			DryRun:    &dryRun,
		},
	})
}

// templateObject is the constraint satisfied by the CAPT template kinds: an API object whose
// metadata can be read, which is what the topology dry-run detection needs.
type templateObject interface {
	runtime.Object
	metav1.Object
}

// assertTemplateSpecImmutability exercises the immutability contract shared by every CAPT
// template webhook. newTemplate must return a template whose spec.template.spec embeds marker,
// and setTemplateLabels must write to spec.template.metadata.labels.
func assertTemplateSpecImmutability[T templateObject](
	t *testing.T,
	validator admission.Validator[T],
	newTemplate func(marker string) T,
	setTemplateLabels func(obj T, labels map[string]string),
) {
	t.Helper()

	t.Run("rejects a spec.template.spec change", func(t *testing.T) {
		t.Parallel()
		g := NewWithT(t)

		_, err := validator.ValidateUpdate(admissionContext(false), newTemplate("original"), newTemplate("modified"))
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("spec.template.spec is immutable"))
	})

	t.Run("allows a spec.template.spec change for a topology dry-run", func(t *testing.T) {
		t.Parallel()
		g := NewWithT(t)

		updated := newTemplate("modified")
		updated.SetAnnotations(map[string]string{clusterv1.TopologyDryRunAnnotation: ""})

		_, err := validator.ValidateUpdate(admissionContext(true), newTemplate("original"), updated)
		g.Expect(err).ToNot(HaveOccurred())
	})

	t.Run("rejects a spec.template.spec change for a plain dry-run", func(t *testing.T) {
		t.Parallel()
		g := NewWithT(t)

		// `kubectl apply --dry-run=server` carries DryRun without the topology annotation and
		// must still see the immutability error, matching a non-dry-run apply.
		_, err := validator.ValidateUpdate(admissionContext(true), newTemplate("original"), newTemplate("modified"))
		g.Expect(err).To(HaveOccurred())
	})

	t.Run("allows a spec.template.metadata change", func(t *testing.T) {
		t.Parallel()
		g := NewWithT(t)

		updated := newTemplate("original")
		setTemplateLabels(updated, map[string]string{"example.com/pool": "worker"})

		_, err := validator.ValidateUpdate(admissionContext(false), newTemplate("original"), updated)
		g.Expect(err).ToNot(HaveOccurred())
	})

	t.Run("allows a no-op update", func(t *testing.T) {
		t.Parallel()
		g := NewWithT(t)

		_, err := validator.ValidateUpdate(admissionContext(false), newTemplate("original"), newTemplate("original"))
		g.Expect(err).ToNot(HaveOccurred())
	})

	t.Run("allows create and delete", func(t *testing.T) {
		t.Parallel()
		g := NewWithT(t)

		_, err := validator.ValidateCreate(admissionContext(false), newTemplate("original"))
		g.Expect(err).ToNot(HaveOccurred())

		_, err = validator.ValidateDelete(admissionContext(false), newTemplate("original"))
		g.Expect(err).ToNot(HaveOccurred())
	})
}
