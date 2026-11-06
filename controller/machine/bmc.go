package machine

import (
	"fmt"

	tinkv2 "github.com/tinkerbell/tinkerbell/api/v1alpha2/tinkerbell"
	rufiov2 "github.com/tinkerbell/tinkerbell/api/v1alpha2/tinkerbell/bmc"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// newBMCJob returns a bmc Job named name that runs operations against hw's BMC, in order.
//
// The Job references the Hardware rather than carrying a connection: Rufio resolves the BMC
// from it, whether the Hardware has its own spec.bmc.connection or, for Hardware created
// through v1alpha1, a bmcRef to a deprecated bmc Machine (see hasBMC).
func (scope *machineReconcileScope) newBMCJob(name string, hw *tinkv2.Hardware, operations ...rufiov2.Operations) *rufiov2.Job {
	return &rufiov2.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: scope.tinkerbellNamespace(),
		},
		Spec: rufiov2.JobSpec{
			HardwareRef: &rufiov2.SimpleReference{
				Name:      hw.Name,
				Namespace: hw.Namespace,
			},
			Operations: operations,
		},
	}
}

// powerAction returns the bmc operation that runs action.
func powerAction(action rufiov2.PowerAction) rufiov2.Operations {
	return rufiov2.Operations{PowerAction: &action}
}

// createPowerOffJob creates a BMCJob object with the required tasks for hardware power off.
func (scope *machineReconcileScope) createPowerOffJob(hw *tinkv2.Hardware) error {
	bmcJob := scope.newBMCJob(fmt.Sprintf("%s-poweroff", scope.tinkerbellMachine.Name), hw,
		powerAction(rufiov2.PowerActionHardOff))

	if err := scope.setResourceOwnership(bmcJob); err != nil {
		return fmt.Errorf("setting BMCJob ownership: %w", err)
	}

	if err := scope.tinkerbellClient.Create(scope.ctx, bmcJob); err != nil {
		return fmt.Errorf("creating BMCJob: %w", err)
	}

	scope.log.Info("Created BMCJob to power off hardware",
		"Name", bmcJob.Name,
		"Namespace", bmcJob.Namespace)

	return fmt.Errorf("requeue to wait for job.bmc completion: %s/%s", bmcJob.Namespace, bmcJob.Name)
}

// getJob fetches the Job by name.
func (scope *machineReconcileScope) getJob(name string, job *rufiov2.Job) error {
	namespacedName := types.NamespacedName{
		Name:      name,
		Namespace: scope.tinkerbellNamespace(),
	}

	if err := scope.tinkerbellClient.Get(scope.ctx, namespacedName, job); err != nil {
		return fmt.Errorf("getting BMCJob: %w", err)
	}

	return nil
}

// ensureBMCJobCompletionForDelete ensures the machine power off BMCJob is completed.
// Removes the machint finalizer to let machine delete.
func (scope *machineReconcileScope) ensureBMCJobCompletionForDelete(hardware *tinkv2.Hardware) error {
	// Fetch a poweroff BMCJob for the machine.
	// If Job not found, we remove dependencies and create job.
	bmcJob := &rufiov2.Job{}
	jobName := fmt.Sprintf("%s-poweroff", scope.tinkerbellMachine.Name)

	if err := scope.getJob(jobName, bmcJob); err != nil {
		if apierrors.IsNotFound(err) {
			return scope.createPowerOffJob(hardware)
		}

		return fmt.Errorf("getting BMC job for machine: %w", err)
	}

	// Check the Job conditions to ensure the power off job is complete.
	if rufiov2.HasConditionStatus(bmcJob.Status.Conditions, rufiov2.ConditionTypeCompleted, rufiov2.ConditionStatusTrue) {
		return nil
	}

	if rufiov2.HasConditionStatus(bmcJob.Status.Conditions, rufiov2.ConditionTypeFailed, rufiov2.ConditionStatusTrue) {
		return fmt.Errorf("bmc job %s/%s failed", bmcJob.Namespace, bmcJob.Name)
	}

	return fmt.Errorf("requeue, bmc job %s/%s is not completed", bmcJob.Namespace, bmcJob.Name)
}
