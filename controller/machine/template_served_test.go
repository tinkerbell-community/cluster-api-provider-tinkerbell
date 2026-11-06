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

package machine //nolint:testpackage

import (
	"context"
	"os"
	"slices"
	"testing"

	. "github.com/onsi/gomega" //nolint:revive // one day we will remove gomega
	tinkv1 "github.com/tinkerbell/tinkerbell/api/v1alpha1/tinkerbell"
	tinkv2 "github.com/tinkerbell/tinkerbell/api/v1alpha2/tinkerbell"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	infrastructurev1 "github.com/tinkerbell/cluster-api-provider-tinkerbell/api/v1beta2"
)

// templatesNotServed makes the scope's Tinkerbell client answer like an API server whose Template
// CRD Tinkerbell removed (--enable-deprecated-crd-removal): every Template request fails with a
// no-match error.
func templatesNotServed(g Gomega, scope *machineReconcileScope) {
	inner, ok := scope.tinkerbellClient.(client.WithWatch)
	g.Expect(ok).To(BeTrue(), "the test client is a fake client")

	noMatch := func(obj any) error {
		switch obj.(type) {
		case *tinkv1.Template, *tinkv1.TemplateList:
			return &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: "tinkerbell.org", Kind: "Template"}, SearchedVersions: []string{"v1alpha1"}}
		}

		return nil
	}
	cl := interceptor.NewClient(inner, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if err := noMatch(obj); err != nil {
				return err
			}

			return c.Get(ctx, key, obj, opts...)
		},
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if err := noMatch(list); err != nil {
				return err
			}

			return c.List(ctx, list, opts...)
		},
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if err := noMatch(obj); err != nil {
				return err
			}

			return c.Create(ctx, obj, opts...)
		},
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if err := noMatch(obj); err != nil {
				return err
			}

			return c.Delete(ctx, obj, opts...)
		},
	})
	scope.client = cl
	scope.tinkerbellClient = cl
}

// Without Templates, a template that converts still runs as v1alpha2 Tasks.
func Test_createWorkflow_without_templates_runs_tasks(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	hw := workflowTestHardware(false)
	scope := workflowTestScope(g, talosTemplate, hw)
	templatesNotServed(g, scope)

	g.Expect(scope.createWorkflow(hw)).To(Succeed())

	wf := getWorkflow(g, scope)
	g.Expect(wf.Spec.Tasks).To(HaveLen(1))
	g.Expect(wf.Spec.Tasks[0].TaskRef.Name).To(Equal(wfMachine+"-talos"), "the Workflow runs the template's Task")
	g.Expect(listTasks(g, scope)).To(HaveLen(1))
}

// Without Templates, a template that does not convert cannot fall back to compatibility mode: the
// error says why, and no Workflow is created.
func Test_createWorkflow_without_templates_cannot_fall_back(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	hw := workflowTestHardware(false)
	scope := workflowTestScope(g, rangeTemplate, hw)
	templatesNotServed(g, scope)

	err := scope.createWorkflow(hw)
	g.Expect(err).To(MatchError(ErrTemplatesNotServed))
	g.Expect(err.Error()).To(ContainSubstring(`it uses "range"`), "the error says why the template does not run as Tasks")
	g.Expect(apierrors.IsNotFound(scope.tinkerbellClient.Get(scope.ctx, machineKey(), &tinkv2.Workflow{}))).To(BeTrue())
}

// Without Templates, a machine's dependencies are still removed, so it can be deleted.
func Test_removeDependencies_without_templates(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	labels := map[string]string{LabelMachineName: wfMachine, LabelMachineNamespace: wfNamespace}
	objects := []client.Object{
		&tinkv2.Task{ObjectMeta: metav1.ObjectMeta{Name: wfMachine + "-talos", Namespace: wfNamespace, Labels: labels}},
		&tinkv2.Workflow{ObjectMeta: metav1.ObjectMeta{Name: wfMachine, Namespace: wfNamespace, Labels: labels}},
	}
	scope := workflowTestScope(g, talosTemplate, objects...)
	templatesNotServed(g, scope)

	g.Expect(scope.removeDependencies()).To(Succeed())

	g.Expect(apierrors.IsNotFound(scope.tinkerbellClient.Get(scope.ctx, machineKey(), &tinkv2.Workflow{}))).To(BeTrue())
	g.Expect(listTasks(g, scope)).To(BeEmpty())
}

// A templateRef cannot be read without Templates; the error says to inline the template.
func Test_templateRef_without_templates(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	hw := workflowTestHardware(false)
	scope := workflowTestScope(g, "", hw)
	scope.tinkerbellMachine.Spec.TemplateRef = &infrastructurev1.ObjectRef{Name: "shared"}
	templatesNotServed(g, scope)

	_, err := scope.templateData(hw)
	g.Expect(err).To(MatchError(ErrTemplatesNotServed))
	g.Expect(err.Error()).To(ContainSubstring("templateInline"))
}

// TestTinkerbellWithoutTemplates runs CAPT against a real API server with Tinkerbell's CRDs but no
// Template CRD, as Tinkerbell leaves it once it removed the v1alpha1-only CRDs. It needs envtest's
// binaries (KUBEBUILDER_ASSETS) and Tinkerbell's CRDs (TINKERBELL_CRD_DIR), and is skipped without
// them.
func TestTinkerbellWithoutTemplates(t *testing.T) {
	t.Parallel()

	crdDir := os.Getenv(tinkerbellCRDDirEnv)
	if os.Getenv("KUBEBUILDER_ASSETS") == "" || crdDir == "" {
		t.Skipf("set KUBEBUILDER_ASSETS and %s (Tinkerbell's crd/bases) to run against the Tinkerbell CRDs", tinkerbellCRDDirEnv)
	}

	g := NewWithT(t)

	crds := slices.DeleteFunc(v1alpha2CRDs(g, crdDir), func(c *apiextensionsv1.CustomResourceDefinition) bool {
		return c.Spec.Names.Kind == "Template"
	})
	env := &envtest.Environment{CRDs: crds, ErrorIfCRDPathMissing: true}

	cfg, err := env.Start()
	g.Expect(err).NotTo(HaveOccurred())

	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Logf("stopping envtest: %v", err)
		}
	})

	tinkerbell, err := client.New(cfg, client.Options{Scheme: hardwareTestScheme(g)})
	g.Expect(err).NotTo(HaveOccurred())

	const namespace = "default"

	hw := envtestHardware(g, tinkerbell, namespace)

	// A template that converts runs as Tasks, and the machine's dependencies are removed.
	scope := envtestScope(g, tinkerbell, namespace, talosTemplate)
	g.Expect(scope.createWorkflow(hw)).To(Succeed())

	wf := &tinkv2.Workflow{}
	g.Expect(tinkerbell.Get(scope.ctx, namespacedMachineKey(namespace), wf)).To(Succeed())
	g.Expect(wf.Spec.Tasks[0].TaskRef.Name).NotTo(BeEmpty())
	g.Expect(scope.removeDependencies()).To(Succeed())
	g.Expect(apierrors.IsNotFound(tinkerbell.Get(scope.ctx, namespacedMachineKey(namespace), &tinkv2.Workflow{}))).To(BeTrue())

	// One that does not convert cannot fall back to compatibility mode.
	scope = envtestScope(g, tinkerbell, namespace, rangeTemplate)
	g.Expect(scope.createWorkflow(hw)).To(MatchError(ErrTemplatesNotServed))
}

func namespacedMachineKey(namespace string) client.ObjectKey {
	return client.ObjectKey{Name: wfMachine, Namespace: namespace}
}
