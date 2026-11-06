package machine //nolint:testpackage

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	. "github.com/onsi/gomega" //nolint:revive // one day we will remove gomega
	tinkv2 "github.com/tinkerbell/tinkerbell/api/v1alpha2/tinkerbell"
	rufiov2 "github.com/tinkerbell/tinkerbell/api/v1alpha2/tinkerbell/bmc"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	infrastructurev1 "github.com/tinkerbell/cluster-api-provider-tinkerbell/api/v1beta2"
)

const bmcTestNamespace = "tinkerbell"

func bmcTestScope(g Gomega) *machineReconcileScope {
	machine := &infrastructurev1.TinkerbellMachine{
		ObjectMeta: metav1.ObjectMeta{Name: "machine-1", Namespace: "capi", UID: "machine-1-uid"},
		Status:     infrastructurev1.TinkerbellMachineStatus{TargetNamespace: bmcTestNamespace},
	}

	return &machineReconcileScope{
		ctx:               context.Background(),
		log:               logr.Discard(),
		scheme:            hardwareTestScheme(g),
		tinkerbellClient:  newHardwareTestClient(g),
		tinkerbellMachine: machine,
	}
}

func bmcTestHardware() *tinkv2.Hardware {
	return &tinkv2.Hardware{
		ObjectMeta: metav1.ObjectMeta{Name: "hw-1", Namespace: bmcTestNamespace},
		Spec: tinkv2.HardwareSpec{BMC: &tinkv2.BMC{Connection: &rufiov2.Connection{
			Host:           "10.0.0.10",
			CredentialsRef: rufiov2.SimpleReference{Name: "hw-1-bmc", Namespace: bmcTestNamespace},
		}}},
	}
}

// Deleting a machine powers its Hardware off through a v1alpha2 bmc Job that references the
// Hardware, and waits for Rufio to report the Job's outcome in its conditions.
func Test_ensureBMCJobCompletionForDelete(t *testing.T) {
	t.Parallel()

	key := types.NamespacedName{Name: "machine-1-poweroff", Namespace: bmcTestNamespace}

	t.Run("creates_a_power_off_job_for_the_hardware", func(t *testing.T) {
		t.Parallel()
		g := NewWithT(t)

		scope := bmcTestScope(g)

		err := scope.ensureBMCJobCompletionForDelete(bmcTestHardware())
		g.Expect(err).To(MatchError(ContainSubstring("requeue to wait for job.bmc completion")))

		job := &rufiov2.Job{}
		g.Expect(scope.tinkerbellClient.Get(scope.ctx, key, job)).To(Succeed())
		g.Expect(job.Spec).To(Equal(rufiov2.JobSpec{
			HardwareRef: &rufiov2.SimpleReference{Name: "hw-1", Namespace: bmcTestNamespace},
			Operations:  []rufiov2.Operations{{PowerAction: ptr.To(rufiov2.PowerActionHardOff)}},
		}))
		g.Expect(job.Labels).To(HaveKeyWithValue(LabelMachineName, "machine-1"))
		g.Expect(job.Labels).To(HaveKeyWithValue(LabelMachineNamespace, "capi"))
		g.Expect(job.OwnerReferences).To(BeEmpty(), "an owner reference cannot cross namespaces")
	})

	cases := map[string]struct {
		conditions []rufiov2.Condition
		wantErr    string
	}{
		"waits_while_the_job_runs": {
			conditions: []rufiov2.Condition{{Type: rufiov2.ConditionTypeRunning, Status: rufiov2.ConditionStatusTrue}},
			wantErr:    "is not completed",
		},
		"reports_a_failed_job": {
			conditions: []rufiov2.Condition{{Type: rufiov2.ConditionTypeFailed, Status: rufiov2.ConditionStatusTrue}},
			wantErr:    "failed",
		},
		"succeeds_once_the_job_completed": {
			conditions: []rufiov2.Condition{{Type: rufiov2.ConditionTypeCompleted, Status: rufiov2.ConditionStatusTrue}},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			g := NewWithT(t)

			scope := bmcTestScope(g)
			job := &rufiov2.Job{
				ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
				Spec:       rufiov2.JobSpec{Operations: []rufiov2.Operations{{PowerAction: ptr.To(rufiov2.PowerActionHardOff)}}},
				Status:     rufiov2.JobStatus{Conditions: tc.conditions},
			}
			g.Expect(scope.tinkerbellClient.Create(scope.ctx, job)).To(Succeed())

			err := scope.ensureBMCJobCompletionForDelete(bmcTestHardware())
			if tc.wantErr == "" {
				g.Expect(err).NotTo(HaveOccurred())

				return
			}

			g.Expect(err).To(MatchError(ContainSubstring(tc.wantErr)))
		})
	}
}
