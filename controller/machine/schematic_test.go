package machine

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	tinkv1 "github.com/tinkerbell/tinkerbell/api/v1alpha1/tinkerbell"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util/patch"
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
// TalosConfig bootstrap object carrying the given talosVersion (which may be a
// full version, a bare minor, empty, or "latest"). provisioned controls the
// Hardware's provisioned annotation.
func newSchematicScope(t *testing.T, factoryURL, talosVersion string, provisioned bool) (*machineReconcileScope, *tinkv1.Hardware) {
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
		WithStatusSubresource(&infrastructurev1.TinkerbellMachine{}).
		WithObjects(machine, tinkerbellMachine, hw,
			talosConfigObject("machine-1", "default", talosVersion)).Build()

	return &machineReconcileScope{
		log:                ctrl.Log.WithName("test"),
		ctx:                context.Background(),
		client:             c,
		tinkerbellClient:   c,
		scheme:             scheme,
		machine:            machine,
		tinkerbellMachine:  tinkerbellMachine,
		schematicRegistrar: schematic.NewRegistrar(factoryURL),
		versionResolver:    schematic.NewVersionResolver(factoryURL),
		factoryURL:         factoryURL,
	}, hw
}

// fakeFactory serves the two Image Factory routes the schematic pipeline uses: GET /versions
// returns the given version list, and any other request is treated as a schematic upload.
func fakeFactory(t *testing.T, versions []string) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodGet && strings.HasPrefix(req.URL.Path, "/versions") {
			_ = json.NewEncoder(w).Encode(versions)

			return
		}

		_ = json.NewEncoder(w).Encode(map[string]string{"id": "deadbeef"})
	}))
}

// TestReconcileSchematicRunsForProvisionedMachine locks in the P3 fix: the
// schematic is resolved and status.installerImage recorded even for an
// already provisioned machine, so a later talosVersion bump can drive an
// in-place upgrade. Before the fix, reconcile short-circuited on the
// provisioned annotation and never resolved.
func TestReconcileSchematicRunsForProvisionedMachine(t *testing.T) {
	t.Parallel()

	factory := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "deadbeef"})
	}))
	defer factory.Close()

	scope, hw := newSchematicScope(t, factory.URL, "v1.14.0", true /* provisioned */)

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
	t.Parallel()

	factory := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "cafef00d"})
	}))
	defer factory.Close()

	scope, hw := newSchematicScope(t, factory.URL, "v1.14.0", false /* not provisioned */)

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

// factoryVersions is a representative /versions listing: GA releases with the newest minor's
// pre-releases preceding its GA, matching factory.talos.dev.
func factoryVersions() []string {
	return []string{
		"v1.12.12",
		"v1.13.0", "v1.13.9", "v1.13.10",
		"v1.14.0-rc.2", "v1.14.0",
	}
}

// A full version is an explicit pin and must be used exactly, never bumped to a newer patch:
// operators pin precisely to avoid surprise upgrades.
func TestResolveTalosVersionRespectsFullPin(t *testing.T) {
	t.Parallel()

	factory := fakeFactory(t, factoryVersions())
	defer factory.Close()

	scope, _ := newSchematicScope(t, factory.URL, "v1.13.9", true)

	if got := scope.resolveTalosVersion(true); got != "v1.13.9" {
		t.Errorf("resolveTalosVersion() = %q, want v1.13.9 (a full pin must not be bumped)", got)
	}
}

// A bare minor tracks the newest GA patch within that minor, which is what drives an in-place
// patch upgrade once a new patch ships.
func TestResolveTalosVersionTracksLatestPatchForBareMinor(t *testing.T) {
	t.Parallel()

	factory := fakeFactory(t, factoryVersions())
	defer factory.Close()

	scope, _ := newSchematicScope(t, factory.URL, "v1.13", true)

	if got := scope.resolveTalosVersion(true); got != "v1.13.10" {
		t.Errorf("resolveTalosVersion() = %q, want v1.13.10", got)
	}
}

// An unset version on a fresh (not yet provisioned) machine pins the newest GA minor and resolves
// its newest patch, so the machine is installed at the current OS and stays contract-bounded
// rather than following new minors forever.
func TestResolveTalosVersionPinsNewestMinorWhenUnset(t *testing.T) {
	t.Parallel()

	factory := fakeFactory(t, factoryVersions())
	defer factory.Close()

	scope, _ := newSchematicScope(t, factory.URL, "", false /* not provisioned */)

	if got := scope.resolveTalosVersion(false); got != "v1.14.0" {
		t.Errorf("resolveTalosVersion() = %q, want v1.14.0", got)
	}

	if pin := scope.tinkerbellMachine.GetAnnotations()[schematic.ContractAnnotation]; pin != "v1.14" {
		t.Errorf("contract annotation = %q, want v1.14 (the resolved minor must be pinned)", pin)
	}
}

// An unset version on an already-provisioned machine that carries no pin must NOT establish one:
// the running OS minor is unknown here, and publishing the factory's newest minor could ask the
// bootstrap provider to skip a minor, which Talos does not support. Resolution is skipped instead,
// preserving the pre-feature behavior, and no annotation is stamped.
func TestResolveTalosVersionSkipsUnsetOnProvisionedMachine(t *testing.T) {
	t.Parallel()

	factory := fakeFactory(t, factoryVersions())
	defer factory.Close()

	scope, _ := newSchematicScope(t, factory.URL, "", true /* provisioned */)

	if got := scope.resolveTalosVersion(true); got != "" {
		t.Errorf("resolveTalosVersion() = %q, want empty (a provisioned, unpinned machine must not jump minors)", got)
	}

	if pin, ok := scope.tinkerbellMachine.GetAnnotations()[schematic.ContractAnnotation]; ok {
		t.Errorf("contract annotation = %q, want none (no pin may be established for a running machine)", pin)
	}
}

// Once a contract minor is pinned, resolution stays within it even on a provisioned machine and
// even though a newer minor is GA: crossing a minor must be a deliberate act, not an unattended
// jump.
func TestResolveTalosVersionHonorsExistingContractPin(t *testing.T) {
	t.Parallel()

	factory := fakeFactory(t, factoryVersions())
	defer factory.Close()

	scope, _ := newSchematicScope(t, factory.URL, "", true /* provisioned */)
	scope.tinkerbellMachine.SetAnnotations(map[string]string{schematic.ContractAnnotation: "v1.13"})

	if got := scope.resolveTalosVersion(true); got != "v1.13.10" {
		t.Errorf("resolveTalosVersion() = %q, want v1.13.10 (must stay on the pinned minor)", got)
	}
}

// The contract pin established for an unset version must actually be persisted through the patch
// helper the way Reconcile persists it, and a second reconcile must not rewrite it: an in-memory
// pin that never reaches the API server, or one re-stamped every reconcile, would either lose the
// contract or churn the object forever.
func TestContractPinPersistsAndIsIdempotent(t *testing.T) {
	t.Parallel()

	factory := fakeFactory(t, factoryVersions())
	defer factory.Close()

	scope, hw := newSchematicScope(t, factory.URL, "", false /* not provisioned */)
	key := types.NamespacedName{Namespace: "default", Name: "machine-1"}

	// First reconcile: resolve, stamp the pin, and persist it the way Reconcile does.
	runReconcileSchematic(t, scope, hw)

	persisted := &infrastructurev1.TinkerbellMachine{}
	if err := scope.client.Get(scope.ctx, key, persisted); err != nil {
		t.Fatalf("re-getting machine: %v", err)
	}

	if pin := persisted.GetAnnotations()[schematic.ContractAnnotation]; pin != "v1.14" {
		t.Fatalf("contract annotation = %q, want v1.14 (the pin must survive the patch helper)", pin)
	}

	settledVersion := persisted.ResourceVersion

	// Second reconcile over the persisted object: the pin already exists and the resolved values
	// are unchanged, so nothing must be written.
	scope.tinkerbellMachine = persisted
	runReconcileSchematic(t, scope, hw)

	after := &infrastructurev1.TinkerbellMachine{}
	if err := scope.client.Get(scope.ctx, key, after); err != nil {
		t.Fatalf("re-getting machine: %v", err)
	}

	if after.ResourceVersion != settledVersion {
		t.Errorf("second reconcile rewrote the machine (resourceVersion %s -> %s); resolution must be idempotent",
			settledVersion, after.ResourceVersion)
	}
}

// runReconcileSchematic runs reconcileSchematic under a fresh patch helper and persists the result,
// mirroring how Reconcile snapshots the machine and patches it at the end of a reconcile.
func runReconcileSchematic(t *testing.T, scope *machineReconcileScope, hw *tinkv1.Hardware) {
	t.Helper()

	helper, err := patch.NewHelper(scope.tinkerbellMachine, scope.client)
	if err != nil {
		t.Fatalf("new patch helper: %v", err)
	}

	if err := scope.reconcileSchematic(hw); err != nil {
		t.Fatalf("reconcileSchematic: %v", err)
	}

	if err := helper.Patch(scope.ctx, scope.tinkerbellMachine); err != nil {
		t.Fatalf("patch: %v", err)
	}
}
