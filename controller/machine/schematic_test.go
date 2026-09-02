package machine

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	tinkv1 "github.com/tinkerbell/tinkerbell/api/v1alpha1/tinkerbell"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrastructurev1 "github.com/tinkerbell/cluster-api-provider-tinkerbell/api/v1beta2"
	controller "github.com/tinkerbell/cluster-api-provider-tinkerbell/controller"
	"github.com/tinkerbell/cluster-api-provider-tinkerbell/pkg/schematic"
)

// talosConfigObject builds a TalosConfig as an unstructured object carrying
// spec.talosVersion — the fake client serves it, and reconcileSchematic reads
// it via the same unstructured path production uses, avoiding a bootstrap
// provider import.
func talosConfigObject(name, namespace, talosVersion string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "bootstrap.cluster.x-k8s.io", Version: "v1beta1", Kind: "TalosConfig",
	})
	u.SetName(name)
	u.SetNamespace(namespace)
	_ = unstructured.SetNestedField(u.Object, talosVersion, "spec", "talosVersion")
	return u
}

// newSchematicScope builds a reconcile scope with a fake Image Factory and a
// TalosConfig bootstrap object carrying a full talosVersion. provisioned
// controls the Hardware's provisioned annotation.
func newSchematicScope(t *testing.T, factoryURL string, provisioned bool) (*machineReconcileScope, *tinkv1.Hardware) {
	t.Helper()

	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		clusterv1.AddToScheme, infrastructurev1.AddToScheme,
		controller.AddToSchemeBMC, controller.AddToSchemeTinkerbell,
	} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}

	machine := &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "machine-1", Namespace: "default"},
		Spec: clusterv1.MachineSpec{
			Bootstrap: clusterv1.Bootstrap{
				ConfigRef: clusterv1.ContractVersionedObjectReference{
					APIGroup: "bootstrap.cluster.x-k8s.io",
					Kind:     "TalosConfig",
					Name:     "machine-1",
				},
			},
		},
	}

	tinkerbellMachine := &infrastructurev1.TinkerbellMachine{
		ObjectMeta: metav1.ObjectMeta{
			Name: "machine-1", Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{
				{APIVersion: clusterv1.GroupVersion.String(), Kind: "Machine", Name: "machine-1"},
			},
		},
		Status: infrastructurev1.TinkerbellMachineStatus{TargetNamespace: "default"},
	}

	hwAnnotations := map[string]string{}
	if provisioned {
		hwAnnotations[HardwareProvisionedAnnotation] = "true"
	}
	hw := &tinkv1.Hardware{
		ObjectMeta: metav1.ObjectMeta{Name: "hw-1", Namespace: "default", Annotations: hwAnnotations},
		Spec: tinkv1.HardwareSpec{
			Interfaces: []tinkv1.Interface{{DHCP: &tinkv1.DHCP{Arch: "x86_64"}}},
			Disks:      []tinkv1.Disk{{Device: "/dev/nvme0n1"}},
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(machine, tinkerbellMachine, hw,
			talosConfigObject("machine-1", "default", "v1.14.0")).Build()

	return &machineReconcileScope{
		log:                ctrl.Log.WithName("test"),
		ctx:                context.Background(),
		client:             c,
		tinkerbellClient:   c,
		scheme:             scheme,
		machine:            machine,
		tinkerbellMachine:  tinkerbellMachine,
		schematicRegistrar: schematic.NewRegistrar(factoryURL),
		factoryURL:         factoryURL,
	}, hw
}

// TestReconcileSchematicRunsForProvisionedMachine locks in the P3 fix: the
// schematic is resolved and status.installerImage recorded even for an
// already provisioned machine, so a later talosVersion bump can drive an
// in-place upgrade. Before the fix, reconcile short-circuited on the
// provisioned annotation and never resolved.
func TestReconcileSchematicRunsForProvisionedMachine(t *testing.T) {
	factory := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "deadbeef"})
	}))
	defer factory.Close()

	scope, hw := newSchematicScope(t, factory.URL, true /* provisioned */)

	if err := scope.reconcile(hw); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if !scope.tinkerbellMachine.Status.Ready {
		t.Error("provisioned machine should be marked ready (short-circuit still applies)")
	}
	st := scope.tinkerbellMachine.Status
	if st.SchematicID != "deadbeef" {
		t.Errorf("SchematicID = %q, want deadbeef (schematic must resolve for provisioned machines)", st.SchematicID)
	}
	if st.InstallerImage == "" {
		t.Error("InstallerImage empty; the bootstrap provider needs it to trigger upgrades")
	}
	if st.DiskImageURL == "" {
		t.Error("DiskImageURL empty")
	}
}

// TestReconcileSchematicUnprovisionedStillResolves guards the pre-existing
// fresh-provision behavior after the hoist.
func TestReconcileSchematicUnprovisionedStillResolves(t *testing.T) {
	factory := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "cafef00d"})
	}))
	defer factory.Close()

	scope, hw := newSchematicScope(t, factory.URL, false /* not provisioned */)

	// Call reconcileSchematic directly: full reconcile would proceed into
	// Workflow/Template creation, which needs cluster scaffolding unrelated
	// to schematic resolution.
	if err := scope.reconcileSchematic(hw); err != nil {
		t.Fatalf("reconcileSchematic: %v", err)
	}

	if scope.tinkerbellMachine.Status.SchematicID != "cafef00d" {
		t.Errorf("SchematicID = %q, want cafef00d", scope.tinkerbellMachine.Status.SchematicID)
	}
}
