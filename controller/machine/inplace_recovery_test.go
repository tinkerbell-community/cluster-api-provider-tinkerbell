package machine

import (
	"context"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/tinkerbell/tinkerbell/api/conversion"
	tinkv1 "github.com/tinkerbell/tinkerbell/api/v1alpha1/tinkerbell"
	tinkv2 "github.com/tinkerbell/tinkerbell/api/v1alpha2/tinkerbell"
	rufiov2 "github.com/tinkerbell/tinkerbell/api/v1alpha2/tinkerbell/bmc"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/tinkerbell/cluster-api-provider-tinkerbell/controller"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	infrastructurev1 "github.com/tinkerbell/cluster-api-provider-tinkerbell/api/v1beta2"
)

const recoveryJobName = "machine-1-inplace-recovery"

// newRecoveryScope builds a scope whose TinkerbellMachine is owned by a Machine that has been
// not-up-to-date for staleFor, optionally already mid in-place update.
func newRecoveryScope(t *testing.T, updating bool, staleFor time.Duration, attempted bool) (*machineReconcileScope, *tinkv2.Hardware) {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := clusterv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	if err := infrastructurev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	if err := controller.AddToSchemeBMC(scheme); err != nil {
		t.Fatal(err)
	}

	if err := controller.AddToSchemeTinkerbell(scheme); err != nil {
		t.Fatal(err)
	}

	machineAnnotations := map[string]string{}
	if updating {
		machineAnnotations[clusterv1.UpdateInProgressAnnotation] = ""
	}

	machine := &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "machine-1",
			Namespace:   "default",
			Annotations: machineAnnotations,
		},
		Status: clusterv1.MachineStatus{
			Conditions: []metav1.Condition{
				{
					Type:               clusterv1.MachineUpToDateCondition,
					Status:             metav1.ConditionFalse,
					Reason:             clusterv1.MachineNotUpToDateReason,
					LastTransitionTime: metav1.NewTime(time.Now().Add(-staleFor)),
				},
			},
		},
	}

	tinkerbellMachineAnnotations := map[string]string{}
	if attempted {
		tinkerbellMachineAnnotations[InPlaceRecoveryAttemptedAnnotation] = time.Now().UTC().Format(time.RFC3339)
	}

	tinkerbellMachine := &infrastructurev1.TinkerbellMachine{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "machine-1",
			Namespace:   "default",
			Annotations: tinkerbellMachineAnnotations,
			OwnerReferences: []metav1.OwnerReference{
				{APIVersion: clusterv1.GroupVersion.String(), Kind: "Machine", Name: "machine-1"},
			},
		},
		// Tinkerbell resources are created in the target namespace, which the controller
		// records on status during hardware selection.
		Status: infrastructurev1.TinkerbellMachineStatus{TargetNamespace: "default"},
	}

	hw := &tinkv2.Hardware{
		ObjectMeta: metav1.ObjectMeta{Name: "hw-1", Namespace: "default"},
		Spec: tinkv2.HardwareSpec{BMC: &tinkv2.BMC{Connection: &rufiov2.Connection{
			Host:           "10.0.0.10",
			CredentialsRef: rufiov2.SimpleReference{Name: "bmc-1-auth", Namespace: "default"},
		}}},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(machine, tinkerbellMachine).Build()

	return &machineReconcileScope{
		log:               ctrl.Log.WithName("test"),
		ctx:               context.Background(),
		client:            c,
		tinkerbellClient:  c,
		scheme:            scheme,
		tinkerbellMachine: tinkerbellMachine,
	}, hw
}

func getRecoveryJob(t *testing.T, scope *machineReconcileScope) *rufiov2.Job {
	t.Helper()

	job := &rufiov2.Job{}

	err := scope.tinkerbellClient.Get(scope.ctx,
		types.NamespacedName{Namespace: "default", Name: recoveryJobName}, job)
	if err != nil {
		return nil
	}

	return job
}

// A node that is merely slow to return must not be power cycled.
func TestInPlaceRecovery_NoJobBeforeTimeout(t *testing.T) {
	t.Parallel()

	scope, hw := newRecoveryScope(t, true, 2*time.Minute, false)

	if err := scope.reconcileInPlaceRecovery(hw, 20*time.Minute); err != nil {
		t.Fatal(err)
	}

	if job := getRecoveryJob(t, scope); job != nil {
		t.Fatal("power cycled a machine that was still within the timeout")
	}
}

// The same machine that is within a 20 minute budget is stalled against a shorter one, so the
// timeout is a real policy input rather than a hardcoded constant.
func TestInPlaceRecovery_TimeoutIsHonored(t *testing.T) {
	t.Parallel()

	scope, hw := newRecoveryScope(t, true, 2*time.Minute, false)

	if err := scope.reconcileInPlaceRecovery(hw, time.Minute); err != nil {
		t.Fatal(err)
	}

	if job := getRecoveryJob(t, scope); job == nil {
		t.Fatal("expected a power cycle job once the shorter timeout elapsed")
	}
}

func TestInPlaceRecovery_NoJobWhenNotUpdating(t *testing.T) {
	t.Parallel()

	scope, hw := newRecoveryScope(t, false, time.Hour, false)

	if err := scope.reconcileInPlaceRecovery(hw, 20*time.Minute); err != nil {
		t.Fatal(err)
	}

	if job := getRecoveryJob(t, scope); job != nil {
		t.Fatal("power cycled a machine that was not being updated")
	}
}

func TestInPlaceRecovery_PowerCyclesStalledMachine(t *testing.T) {
	t.Parallel()

	scope, hw := newRecoveryScope(t, true, time.Hour, false)

	if err := scope.reconcileInPlaceRecovery(hw, 20*time.Minute); err != nil {
		t.Fatal(err)
	}

	job := getRecoveryJob(t, scope)
	if job == nil {
		t.Fatal("expected a power cycle job for a stalled in-place update")
	}

	want := rufiov2.JobSpec{
		HardwareRef: &rufiov2.SimpleReference{Name: "hw-1", Namespace: "default"},
		Operations: []rufiov2.Operations{
			{PowerAction: ptr.To(rufiov2.PowerActionHardOff)},
			{PowerAction: ptr.To(rufiov2.PowerActionOn)},
		},
	}
	if diff := cmp.Diff(want, job.Spec); diff != "" {
		t.Errorf("expected a hard off then on against the Hardware's BMC (-want +got):\n%s", diff)
	}

	if job.Labels[LabelMachineName] != "machine-1" || job.Labels[LabelMachineNamespace] != "default" {
		t.Errorf("the Job must carry the machine's ownership labels, got %v", job.Labels)
	}

	updated := &infrastructurev1.TinkerbellMachine{}
	if err := scope.client.Get(scope.ctx,
		types.NamespacedName{Namespace: "default", Name: "machine-1"}, updated); err != nil {
		t.Fatal(err)
	}

	if _, ok := updated.Annotations[InPlaceRecoveryAttemptedAnnotation]; !ok {
		t.Error("the attempt must be recorded so recovery happens once, not in a loop")
	}
}

// One power cycle, not an endless reboot loop: if the machine still has not returned, that is
// for an operator to look at.
func TestInPlaceRecovery_OnlyAttemptsOnce(t *testing.T) {
	t.Parallel()

	scope, hw := newRecoveryScope(t, true, time.Hour, true)

	if err := scope.reconcileInPlaceRecovery(hw, 20*time.Minute); err != nil {
		t.Fatal(err)
	}

	if job := getRecoveryJob(t, scope); job != nil {
		t.Fatal("issued a second power cycle for the same in-place update")
	}
}

// Once the update finishes the marker is cleared, so a later update gets a fresh budget.
func TestInPlaceRecovery_ClearsMarkerWhenUpdateFinishes(t *testing.T) {
	t.Parallel()

	scope, hw := newRecoveryScope(t, false, time.Hour, true)

	if err := scope.reconcileInPlaceRecovery(hw, 20*time.Minute); err != nil {
		t.Fatal(err)
	}

	updated := &infrastructurev1.TinkerbellMachine{}
	if err := scope.client.Get(scope.ctx,
		types.NamespacedName{Namespace: "default", Name: "machine-1"}, updated); err != nil {
		t.Fatal(err)
	}

	if _, ok := updated.Annotations[InPlaceRecoveryAttemptedAnnotation]; ok {
		t.Error("the marker must be cleared once the update is no longer in progress")
	}
}

// Hardware created through v1alpha1 names its BMC with a bmcRef to a bmc Machine, which only the
// conversion annotation still holds. Rufio resolves it from the Hardware, so it is power cycled
// like Hardware with its own connection.
func TestInPlaceRecovery_PowerCyclesHardwareWithAV1Alpha1BMCRef(t *testing.T) {
	t.Parallel()

	scope, _ := newRecoveryScope(t, true, time.Hour, false)

	v1 := &tinkv1.Hardware{
		ObjectMeta: metav1.ObjectMeta{Name: "hw-1", Namespace: "default"},
		Spec: tinkv1.HardwareSpec{
			BMCRef:     &corev1.TypedLocalObjectReference{Kind: "Machine", Name: "bmc-1"},
			Interfaces: []tinkv1.Interface{{DHCP: &tinkv1.DHCP{MAC: "00:00:00:00:00:01"}}},
		},
	}
	hw := &tinkv2.Hardware{}
	if err := conversion.HardwareToV1Alpha2(v1, hw); err != nil {
		t.Fatal(err)
	}

	if err := scope.reconcileInPlaceRecovery(hw, 20*time.Minute); err != nil {
		t.Fatal(err)
	}

	job := getRecoveryJob(t, scope)
	if job == nil {
		t.Fatal("expected a power cycle job for Hardware whose BMC is a v1alpha1 bmc Machine")
	}

	if diff := cmp.Diff(&rufiov2.SimpleReference{Name: "hw-1", Namespace: "default"}, job.Spec.HardwareRef); diff != "" {
		t.Errorf("the Job must reference the Hardware (-want +got):\n%s", diff)
	}
}

// Without a BMC there is nothing to power cycle; that must be reported, not crash.
func TestInPlaceRecovery_NoBMC(t *testing.T) {
	t.Parallel()

	scope, hw := newRecoveryScope(t, true, time.Hour, false)
	hw.Spec.BMC = nil

	if err := scope.reconcileInPlaceRecovery(hw, 20*time.Minute); err != nil {
		t.Fatal(err)
	}

	if job := getRecoveryJob(t, scope); job != nil {
		t.Fatal("created a BMC job without a BMC reference")
	}
}
