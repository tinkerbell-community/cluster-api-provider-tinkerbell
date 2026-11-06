// Package controller provides the controller-runtime scheme for Tinkerbell and BMC resources.
package controller

import (
	tinkv1 "github.com/tinkerbell/tinkerbell/api/v1alpha1/tinkerbell"
	tinkv2 "github.com/tinkerbell/tinkerbell/api/v1alpha2/tinkerbell"
	rufiov2 "github.com/tinkerbell/tinkerbell/api/v1alpha2/tinkerbell/bmc"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

//nolint:gochecknoglobals
var (
	// tinkerbellV1Alpha1 registers the deprecated tinkerbell.org/v1alpha1 kinds CAPT still uses.
	tinkerbellV1Alpha1 = &scheme.Builder{GroupVersion: tinkv1.GroupVersion}

	// SchemeBuilderTinkerbell adds the tinkerbell.org kinds CAPT uses to a scheme: the v1alpha2
	// kinds, and the v1alpha1 kinds that have no v1alpha2 replacement CAPT can use instead.
	SchemeBuilderTinkerbell = runtime.NewSchemeBuilder(tinkv2.AddToScheme, tinkerbellV1Alpha1.AddToScheme)

	// AddToSchemeTinkerbell adds the tinkerbell.org kinds CAPT uses to the given scheme.
	AddToSchemeTinkerbell = SchemeBuilderTinkerbell.AddToScheme

	// SchemeBuilderBMC adds the bmc.tinkerbell.org/v1alpha2 kinds to a scheme.
	SchemeBuilderBMC = runtime.NewSchemeBuilder(rufiov2.AddToScheme)

	// AddToSchemeBMC adds the bmc.tinkerbell.org kinds CAPT uses to the given scheme.
	AddToSchemeBMC = SchemeBuilderBMC.AddToScheme
)

//nolint:gochecknoinits
func init() {
	tinkerbellV1Alpha1.Register(&tinkv1.Template{}, &tinkv1.TemplateList{})
	tinkerbellV1Alpha1.Register(&tinkv1.Workflow{}, &tinkv1.WorkflowList{})
}
