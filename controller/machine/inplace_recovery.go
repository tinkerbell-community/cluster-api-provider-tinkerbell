package machine

import (
	"fmt"
	"time"

	rufiov1 "github.com/tinkerbell/tinkerbell/api/v1alpha1/bmc"
	tinkv1 "github.com/tinkerbell/tinkerbell/api/v1alpha1/tinkerbell"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

const (
	// InPlaceRecoveryAttemptedAnnotation records that a BMC power cycle has already been
	// issued for the current in-place update, so recovery is attempted once rather than in a
	// loop. It is set on the TinkerbellMachine and cleared when the update finishes.
	InPlaceRecoveryAttemptedAnnotation = "infrastructure.cluster.x-k8s.io/in-place-recovery-attempted"

	// DefaultInPlaceRecoveryTimeout is how long a machine may be mid in-place update before
	// its failure to return is treated as a hung boot.
	//
	// Generous on purpose: a Talos upgrade cordons and drains the node, reboots, and rejoins,
	// which on real hardware can take several minutes. Power cycling a node that was merely
	// slow is worse than waiting.
	DefaultInPlaceRecoveryTimeout = 20 * time.Minute
)

// reconcileInPlaceRecovery power cycles a machine whose in-place update has stalled.
//
// Once Cluster API has stamped the in-place update annotation it is committed to that path:
// there is no fallback to replacing the Machine. If the node fails to come back from the
// reboot that a Talos upgrade performs, the update would otherwise hang indefinitely with no
// automated remedy.
//
// Tinkerbell is one of the few infrastructure providers that can actually do something here,
// because it manages the machine's BMC through Rufio. One hard power cycle is attempted; if
// that does not bring the node back the update is left failed for an operator to inspect,
// which is the honest outcome rather than an endless reboot loop.
func (scope *machineReconcileScope) reconcileInPlaceRecovery(hw *tinkv1.Hardware, timeout time.Duration) error {
	machine, err := scope.getOwningMachine()
	if err != nil || machine == nil {
		return err
	}

	_, updating := machine.Annotations[clusterv1.UpdateInProgressAnnotation]
	_, attempted := scope.tinkerbellMachine.GetAnnotations()[InPlaceRecoveryAttemptedAnnotation]

	if !updating {
		// Nothing in flight. Clear the marker so a later update starts with a fresh budget.
		if attempted {
			return scope.clearRecoveryAttempted()
		}

		return nil
	}

	stalledFor, stalled := inPlaceUpdateStalledFor(machine, timeout)
	if attempted || !stalled {
		return nil
	}

	if hw == nil || hw.Spec.BMCRef == nil {
		scope.log.Info("in-place update appears stalled but the hardware has no BMC reference, cannot recover",
			"machine", machine.Name)

		return nil
	}

	scope.log.Info("in-place update stalled, power cycling via BMC",
		"machine", machine.Name,
		"stalledFor", stalledFor.String())

	if err := scope.createPowerCycleJob(hw); err != nil {
		return err
	}

	return scope.markRecoveryAttempted()
}

// inPlaceUpdateStalledFor reports how long the in-place update has been running, and whether
// that has exceeded timeout. An update with no discernible start time is never treated as
// stalled: without a start time there is no evidence the reboot has taken too long.
func inPlaceUpdateStalledFor(machine *clusterv1.Machine, timeout time.Duration) (time.Duration, bool) {
	started := inPlaceUpdateStartedAt(machine)
	if started.IsZero() {
		return 0, false
	}

	elapsed := time.Since(started)

	return elapsed, elapsed >= timeout
}

// inPlaceUpdateStartedAt reports when the in-place update began.
//
// Cluster API does not stamp a start time, so the Machine's last transition into a
// not-up-to-date state is used as the closest available proxy, falling back to the
// annotation's own observation time via the Machine's lastUpdated condition.
func inPlaceUpdateStartedAt(machine *clusterv1.Machine) time.Time {
	for _, condition := range machine.Status.Conditions {
		if condition.Type == clusterv1.MachineUpToDateCondition && condition.Status == metav1.ConditionFalse {
			return condition.LastTransitionTime.Time
		}
	}

	return time.Time{}
}

// createPowerCycleJob issues a hard power cycle through Rufio.
func (scope *machineReconcileScope) createPowerCycleJob(hw *tinkv1.Hardware) error {
	job := &rufiov1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-inplace-recovery", scope.tinkerbellMachine.Name),
			Namespace: scope.tinkerbellNamespace(),
		},
		Spec: rufiov1.JobSpec{
			MachineRef: rufiov1.MachineRef{
				Name:      hw.Spec.BMCRef.Name,
				Namespace: scope.tinkerbellNamespace(),
			},
			Tasks: []rufiov1.Action{
				{PowerAction: ptr.To(rufiov1.PowerHardOff)},
				{PowerAction: ptr.To(rufiov1.PowerOn)},
			},
		},
	}

	if err := scope.setResourceOwnership(job); err != nil {
		return fmt.Errorf("setting BMCJob ownership: %w", err)
	}

	if err := scope.tinkerbellClient.Create(scope.ctx, job); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil
		}

		return fmt.Errorf("creating in-place recovery BMCJob: %w", err)
	}

	return nil
}

func (scope *machineReconcileScope) markRecoveryAttempted() error {
	patch := client.MergeFrom(scope.tinkerbellMachine.DeepCopy())

	annotations := scope.tinkerbellMachine.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}

	annotations[InPlaceRecoveryAttemptedAnnotation] = metav1.Now().UTC().Format(time.RFC3339)
	scope.tinkerbellMachine.SetAnnotations(annotations)

	if err := scope.client.Patch(scope.ctx, scope.tinkerbellMachine, patch); err != nil {
		return fmt.Errorf("recording in-place recovery attempt: %w", err)
	}

	return nil
}

func (scope *machineReconcileScope) clearRecoveryAttempted() error {
	patch := client.MergeFrom(scope.tinkerbellMachine.DeepCopy())

	annotations := scope.tinkerbellMachine.GetAnnotations()
	delete(annotations, InPlaceRecoveryAttemptedAnnotation)
	scope.tinkerbellMachine.SetAnnotations(annotations)

	if err := scope.client.Patch(scope.ctx, scope.tinkerbellMachine, patch); err != nil {
		return fmt.Errorf("clearing in-place recovery attempt: %w", err)
	}

	return nil
}

// getOwningMachine returns the Cluster API Machine that owns this TinkerbellMachine.
func (scope *machineReconcileScope) getOwningMachine() (*clusterv1.Machine, error) {
	for _, ref := range scope.tinkerbellMachine.OwnerReferences {
		if ref.Kind != "Machine" {
			continue
		}

		machine := &clusterv1.Machine{}

		key := types.NamespacedName{
			Namespace: scope.tinkerbellMachine.Namespace,
			Name:      ref.Name,
		}

		if err := scope.client.Get(scope.ctx, key, machine); err != nil {
			if apierrors.IsNotFound(err) {
				return nil, nil
			}

			return nil, fmt.Errorf("getting owning Machine: %w", err)
		}

		return machine, nil
	}

	return nil, nil
}
