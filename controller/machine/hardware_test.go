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
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	. "github.com/onsi/gomega" //nolint:revive // one day we will remove gomega
	"github.com/tinkerbell/tinkerbell/api/conversion"
	tinkv1 "github.com/tinkerbell/tinkerbell/api/v1alpha1/tinkerbell"
	tinkv2 "github.com/tinkerbell/tinkerbell/api/v1alpha2/tinkerbell"
	rufiov2 "github.com/tinkerbell/tinkerbell/api/v1alpha2/tinkerbell/bmc"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ipamv1 "sigs.k8s.io/cluster-api/api/ipam/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	infrastructurev1 "github.com/tinkerbell/cluster-api-provider-tinkerbell/api/v1beta2"
	"github.com/tinkerbell/cluster-api-provider-tinkerbell/controller"
)

func hardwareTestScheme(g Gomega) *runtime.Scheme {
	s := runtime.NewScheme()
	g.Expect(infrastructurev1.AddToScheme(s)).To(Succeed())
	g.Expect(controller.AddToSchemeTinkerbell(s)).To(Succeed())
	g.Expect(controller.AddToSchemeBMC(s)).To(Succeed())
	g.Expect(ipamv1.AddToScheme(s)).To(Succeed())
	g.Expect(clusterv1.AddToScheme(s)).To(Succeed())

	return s
}

func newHardwareTestClient(g Gomega, objects ...client.Object) client.Client {
	s := hardwareTestScheme(g)

	return fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(objects...).
		WithStatusSubresource(&tinkv2.Hardware{}, &infrastructurev1.TinkerbellMachine{}).
		Build()
}

// blockingPatchClient pauses Patch calls until two claimers have reached the
// write boundary. This makes the read-modify-write race deterministic.
type blockingPatchClient struct {
	client.Client
	entered chan struct{}
	release chan struct{}
	patches atomic.Int32
	once    sync.Once
}

func (c *blockingPatchClient) Patch(
	ctx context.Context,
	obj client.Object,
	patch client.Patch,
	opts ...client.PatchOption,
) error {
	if c.patches.Add(1) == 2 {
		c.once.Do(func() {
			close(c.entered)
		})
	}

	<-c.release
	if err := c.Client.Patch(ctx, obj, patch, opts...); err != nil {
		return fmt.Errorf("patching object after claim barrier: %w", err)
	}

	return nil
}

// Test_takeHardwareOwnership_owned_by_other_machine asserts that takeHardwareOwnership
// refuses to claim Hardware already labelled as owned by a different Machine, returning
// an error and leaving the object untouched (no resourceVersion bump).
func Test_takeHardwareOwnership_owned_by_other_machine(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	const (
		machineName      = "myMachine"
		machineNamespace = "myNamespace"
		hardwareName     = "myHardware"
	)

	hw := &tinkv2.Hardware{
		ObjectMeta: metav1.ObjectMeta{
			Name:      hardwareName,
			Namespace: machineNamespace,
			Labels: map[string]string{
				HardwareOwnerNameLabel:      "otherMachine",
				HardwareOwnerNamespaceLabel: "otherNamespace",
			},
		},
	}

	cl := newHardwareTestClient(g, hw)

	scope := &machineReconcileScope{
		ctx:               context.Background(),
		tinkerbellClient:  cl,
		tinkerbellMachine: &infrastructurev1.TinkerbellMachine{ObjectMeta: metav1.ObjectMeta{Name: machineName, Namespace: machineNamespace}},
	}
	before := &tinkv2.Hardware{}
	g.Expect(cl.Get(context.Background(), types.NamespacedName{Name: hardwareName, Namespace: machineNamespace}, before)).To(Succeed())

	err := scope.takeHardwareOwnership(hw)
	g.Expect(err).To(HaveOccurred(), "expected error claiming Hardware owned by a different Machine")
	g.Expect(errors.Is(err, errHardwareClaimRequeue)).To(BeTrue(),
		"expected ownership contention to request a quiet requeue")
	g.Expect(err.Error()).To(ContainSubstring("already owned"), "expected 'already owned' guard error")

	// The object must not have been written: a fresh Get yields the same resourceVersion.
	persisted := &tinkv2.Hardware{}
	g.Expect(cl.Get(context.Background(), types.NamespacedName{Name: hardwareName, Namespace: machineNamespace}, persisted)).To(Succeed())
	g.Expect(persisted.ResourceVersion).To(Equal(before.ResourceVersion),
		"Hardware must not be written when it already belongs to another Machine")
	g.Expect(persisted.Labels[HardwareOwnerNameLabel]).To(Equal("otherMachine"),
		"Hardware must keep its original owner label, not be claimed by this Machine")
}

// Test_takeHardwareOwnership_already_claimed_is_noop asserts that when Hardware is already
// claimed by this Machine with the expected finalizer state, takeHardwareOwnership is a
// no-op: it returns nil without bumping resourceVersion (avoids avoidable conflicts).
func Test_takeHardwareOwnership_already_claimed_is_noop(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	const (
		machineName      = "myMachine"
		machineNamespace = "myNamespace"
		hardwareName     = "myHardware"
	)

	hw := &tinkv2.Hardware{
		ObjectMeta: metav1.ObjectMeta{
			Name:      hardwareName,
			Namespace: machineNamespace,
			Labels: map[string]string{
				HardwareOwnerNameLabel:      machineName,
				HardwareOwnerNamespaceLabel: machineNamespace,
			},
		},
	}
	controllerutil.AddFinalizer(hw, infrastructurev1.MachineFinalizer)

	cl := newHardwareTestClient(g, hw)

	scope := &machineReconcileScope{
		ctx:               context.Background(),
		tinkerbellClient:  cl,
		tinkerbellMachine: &infrastructurev1.TinkerbellMachine{ObjectMeta: metav1.ObjectMeta{Name: machineName, Namespace: machineNamespace}},
	}

	before := &tinkv2.Hardware{}
	g.Expect(cl.Get(context.Background(), types.NamespacedName{Name: hardwareName, Namespace: machineNamespace}, before)).To(Succeed())

	g.Expect(scope.takeHardwareOwnership(hw)).To(Succeed(), "already-claimed Hardware should be a no-op, not an error")

	after := &tinkv2.Hardware{}
	g.Expect(cl.Get(context.Background(), types.NamespacedName{Name: hardwareName, Namespace: machineNamespace}, after)).To(Succeed())
	g.Expect(after.ResourceVersion).To(Equal(before.ResourceVersion),
		"already-claimed Hardware must not be rewritten on subsequent reconciles")
}

// Test_takeHardwareOwnership_concurrent_claims verifies that two Machines which
// read the same Hardware resourceVersion cannot both claim it. The barrier
// forces both stale objects to reach Patch before either write is allowed.
func Test_takeHardwareOwnership_concurrent_claims(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	const (
		hardwareName      = "myHardware"
		machineNamespace  = "myNamespace"
		firstMachineName  = "firstMachine"
		secondMachineName = "secondMachine"
	)

	hw := &tinkv2.Hardware{
		ObjectMeta: metav1.ObjectMeta{
			Name:      hardwareName,
			Namespace: machineNamespace,
		},
	}

	baseClient := newHardwareTestClient(g, hw)
	claimClient := &blockingPatchClient{
		Client:  baseClient,
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}

	firstHardware := &tinkv2.Hardware{}
	secondHardware := &tinkv2.Hardware{}
	namespacedName := types.NamespacedName{Name: hardwareName, Namespace: machineNamespace}
	g.Expect(baseClient.Get(context.Background(), namespacedName, firstHardware)).To(Succeed())
	g.Expect(baseClient.Get(context.Background(), namespacedName, secondHardware)).To(Succeed())
	g.Expect(firstHardware.ResourceVersion).To(Equal(secondHardware.ResourceVersion))

	scope := func(machineName string) *machineReconcileScope {
		return &machineReconcileScope{
			ctx:              context.Background(),
			tinkerbellClient: claimClient,
			tinkerbellMachine: &infrastructurev1.TinkerbellMachine{
				ObjectMeta: metav1.ObjectMeta{
					Name:      machineName,
					Namespace: machineNamespace,
				},
			},
		}
	}

	results := make(chan error, 2)
	go func() {
		results <- scope(firstMachineName).takeHardwareOwnership(firstHardware)
	}()
	go func() {
		results <- scope(secondMachineName).takeHardwareOwnership(secondHardware)
	}()

	select {
	case <-claimClient.entered:
	case <-time.After(time.Second):
		close(claimClient.release)
		t.Fatal("timed out waiting for both concurrent claims to reach Patch")
	}
	close(claimClient.release)

	firstErr := <-results
	secondErr := <-results
	errs := []error{firstErr, secondErr}

	successes := 0
	conflicts := 0
	for _, err := range errs {
		switch {
		case err == nil:
			successes++
		case apierrors.IsConflict(err):
			g.Expect(errors.Is(err, errHardwareClaimRequeue)).To(BeTrue(),
				"expected concurrent claim conflict to request a quiet requeue")
			conflicts++
		default:
			t.Fatalf("unexpected claim error: %v", err)
		}
	}

	g.Expect(successes).To(Equal(1), "exactly one Machine must win the Hardware claim")
	g.Expect(conflicts).To(Equal(1), "exactly one Machine must lose with a resourceVersion conflict")

	persisted := &tinkv2.Hardware{}
	g.Expect(baseClient.Get(context.Background(), namespacedName, persisted)).To(Succeed())
	g.Expect([]string{firstMachineName, secondMachineName}).To(
		ContainElement(persisted.Labels[HardwareOwnerNameLabel]),
		"the persisted owner must be one of the competing Machines",
	)
}

func Test_hardwareIP(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		hw      *tinkv2.Hardware
		want    string
		wantErr error
	}{
		"the_ipv4_address_of_the_primary_interface": {
			hw: &tinkv2.Hardware{Spec: tinkv2.HardwareSpec{
				AgentID: "52:54:00:00:00:02",
				NetworkInterfaces: tinkv2.NetworkInterfaces{
					"52:54:00:00:00:01": {IPAM: &tinkv2.IPAM{IPv4: &tinkv2.IP{Address: "192.0.2.1"}}},
					"52:54:00:00:00:02": {IPAM: &tinkv2.IPAM{IPv4: &tinkv2.IP{Address: "192.0.2.2"}}},
				},
			}},
			want: "192.0.2.2",
		},
		"the_ipv6_address_when_there_is_no_ipv4_one": {
			hw: &tinkv2.Hardware{Spec: tinkv2.HardwareSpec{NetworkInterfaces: tinkv2.NetworkInterfaces{
				"52:54:00:00:00:01": {IPAM: &tinkv2.IPAM{IPv4: &tinkv2.IP{Hostname: ptr.To("node")}, IPv6: &tinkv2.IP{Address: "2001:db8::1"}}},
			}}},
			want: "2001:db8::1",
		},
		"no_interfaces": {
			hw:      &tinkv2.Hardware{},
			wantErr: ErrHardwareMissingInterfaces,
		},
		"no_ipam": {
			hw: &tinkv2.Hardware{Spec: tinkv2.HardwareSpec{NetworkInterfaces: tinkv2.NetworkInterfaces{
				"52:54:00:00:00:01": {},
			}}},
			wantErr: ErrHardwareFirstInterfaceNotDHCP,
		},
		"no_address": {
			hw: &tinkv2.Hardware{Spec: tinkv2.HardwareSpec{NetworkInterfaces: tinkv2.NetworkInterfaces{
				"52:54:00:00:00:01": {IPAM: &tinkv2.IPAM{IPv4: &tinkv2.IP{Gateway: "192.0.2.254"}}},
			}}},
			wantErr: ErrHardwareFirstInterfaceDHCPMissingIP,
		},
		"nil": {
			wantErr: ErrHardwareIsNil,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			g := NewWithT(t)

			got, err := hardwareIP(tc.hw)
			if tc.wantErr != nil {
				g.Expect(err).To(MatchError(tc.wantErr))

				return
			}

			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(got).To(Equal(tc.want))
		})
	}
}

func Test_hasBMC(t *testing.T) {
	t.Parallel()

	fromV1Alpha1 := func(spec tinkv1.HardwareSpec) *tinkv2.Hardware {
		hw := &tinkv2.Hardware{}
		if err := conversion.HardwareToV1Alpha2(&tinkv1.Hardware{Spec: spec}, hw); err != nil {
			t.Fatal(err)
		}

		return hw
	}

	cases := map[string]struct {
		hw   *tinkv2.Hardware
		want bool
	}{
		"a_bmc_connection": {
			hw: &tinkv2.Hardware{Spec: tinkv2.HardwareSpec{BMC: &tinkv2.BMC{
				Connection: &rufiov2.Connection{Host: "10.0.0.10"},
			}}},
			want: true,
		},
		"a_bmc_without_a_connection": {
			hw: &tinkv2.Hardware{Spec: tinkv2.HardwareSpec{BMC: &tinkv2.BMC{BootMode: tinkv2.FirmwareModeUEFI}}},
		},
		"a_v1alpha1_bmc_ref": {
			hw:   fromV1Alpha1(tinkv1.HardwareSpec{BMCRef: &corev1.TypedLocalObjectReference{Kind: "Machine", Name: "bmc-1"}}),
			want: true,
		},
		"v1alpha1_hardware_without_a_bmc_ref": {
			hw: fromV1Alpha1(tinkv1.HardwareSpec{AgentID: "agent"}),
		},
		"no_bmc": {
			hw: &tinkv2.Hardware{},
		},
		"nil": {},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			g := NewWithT(t)

			g.Expect(hasBMC(tc.hw)).To(Equal(tc.want))
		})
	}
}

// Hardware created through v1alpha1 carries the conversion annotation, which holds what v1alpha2
// cannot represent (here a bmcRef, facility metadata, dhcp.uefi, iface_name, allowWorkflow). The
// writes CAPT makes while it provisions and releases a machine are merge patches of its own
// fields, so the annotation, and with it the v1alpha1-only data, survives them, and v1alpha1
// clients see CAPT's writes as they did before.
func Test_hardware_created_through_v1alpha1_keeps_its_v1alpha1_data(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	v1 := &tinkv1.Hardware{
		ObjectMeta: metav1.ObjectMeta{Name: ipamHardware, Namespace: ipamNamespace},
		Spec: tinkv1.HardwareSpec{
			BMCRef:      &corev1.TypedLocalObjectReference{Kind: "Machine", Name: "bmc-1"},
			TinkVersion: 3,
			Disks:       []tinkv1.Disk{{Device: "/dev/sda"}},
			Metadata: &tinkv1.HardwareMetadata{
				Facility: &tinkv1.MetadataFacility{FacilityCode: "onprem", PlanSlug: "c3.small.x86"},
				Instance: &tinkv1.MetadataInstance{ID: ipamMAC, Hostname: "node-1"},
			},
			Interfaces: []tinkv1.Interface{{
				DHCP:    &tinkv1.DHCP{MAC: ipamMAC, IfaceName: "eno1", UEFI: true, Hostname: "node-1", Arch: "x86_64"},
				Netboot: &tinkv1.Netboot{AllowPXE: ptr.To(true), AllowWorkflow: ptr.To(true)},
			}},
		},
	}
	stored := &tinkv2.Hardware{}
	g.Expect(conversion.HardwareToV1Alpha2(v1, stored)).To(Succeed())
	stash := stored.Annotations[conversion.AnnotationV1Alpha1]
	g.Expect(stash).NotTo(BeEmpty())

	machine := ipamMachine()
	claim, address := boundClaim(machine, stored, ipamMAC)
	scope, cl := ipamScope(g, machine, stored, claim, address)
	scope.bootstrapCloudConfig = "#cloud-config"

	hw := &tinkv2.Hardware{}
	g.Expect(cl.Get(context.Background(), client.ObjectKeyFromObject(stored), hw)).To(Succeed())

	// What CAPT writes to the Hardware while it provisions a machine.
	g.Expect(scope.takeHardwareOwnership(hw)).To(Succeed())
	g.Expect(scope.ensureHardwareUserData(hw, "tinkerbell://ipam-ns/ipam-hardware")).To(Succeed())
	g.Expect(scope.reconcileIPAM(hw)).To(Succeed())
	g.Expect(scope.patchHardwareAnnotations(hw, map[string]string{HardwareProvisionedAnnotation: "true"})).To(Succeed())

	provisioned := &tinkv2.Hardware{}
	g.Expect(cl.Get(context.Background(), client.ObjectKeyFromObject(stored), provisioned)).To(Succeed())
	g.Expect(provisioned.Annotations).To(HaveKeyWithValue(conversion.AnnotationV1Alpha1, stash),
		"CAPT must not touch the conversion annotation")

	view := &tinkv1.Hardware{}
	g.Expect(conversion.HardwareToV1Alpha1(provisioned, view)).To(Succeed())
	expectV1Alpha1Only(g, view, v1)
	g.Expect(view.Spec.UserData).To(Equal(ptr.To("#cloud-config")))
	g.Expect(view.Spec.Interfaces[0].DHCP.IP).To(Equal(&tinkv1.IP{Address: "10.1.40.32", Netmask: "255.255.255.0", Gateway: "10.1.40.1", Family: 4}),
		"a v1alpha1 client sees the IPAM reservation where CAPT used to write it")
	g.Expect(view.Spec.Interfaces[0].DHCP.Hostname).To(Equal("node-1"))

	// And what it writes when it releases it.
	g.Expect(scope.releaseHardware(provisioned)).To(Succeed())

	released := &tinkv2.Hardware{}
	g.Expect(cl.Get(context.Background(), client.ObjectKeyFromObject(stored), released)).To(Succeed())
	g.Expect(released.Annotations).To(HaveKeyWithValue(conversion.AnnotationV1Alpha1, stash))
	g.Expect(released.Annotations).NotTo(HaveKey(HardwareProvisionedAnnotation))

	view = &tinkv1.Hardware{}
	g.Expect(conversion.HardwareToV1Alpha1(released, view)).To(Succeed())
	expectV1Alpha1Only(g, view, v1)
	g.Expect(view.Spec.UserData).To(BeNil())
	g.Expect(view.Spec.Interfaces[0].DHCP.IP).To(BeNil())
	g.Expect(view.Spec.Interfaces[0].DHCP.Hostname).To(Equal("node-1"), "only the reservation is cleared")
}

// expectV1Alpha1Only asserts that view, the v1alpha1 form of a Hardware, still carries the
// fields of original that v1alpha2 cannot represent.
func expectV1Alpha1Only(g Gomega, view, original *tinkv1.Hardware) {
	g.Expect(view.Spec.BMCRef).To(Equal(original.Spec.BMCRef))
	g.Expect(view.Spec.TinkVersion).To(Equal(original.Spec.TinkVersion))
	g.Expect(view.Spec.Metadata).NotTo(BeNil())
	g.Expect(view.Spec.Metadata.Facility).To(Equal(original.Spec.Metadata.Facility))
	g.Expect(view.Spec.Metadata.Instance.ID).To(Equal(original.Spec.Metadata.Instance.ID))
	g.Expect(view.Spec.Metadata.Instance.Hostname).To(Equal(original.Spec.Metadata.Instance.Hostname))
	g.Expect(view.Spec.Interfaces).To(HaveLen(1))
	g.Expect(view.Spec.Interfaces[0].DHCP.IfaceName).To(Equal("eno1"))
	g.Expect(view.Spec.Interfaces[0].DHCP.UEFI).To(BeTrue())
	g.Expect(view.Spec.Interfaces[0].Netboot.AllowWorkflow).To(Equal(ptr.To(true)))
}
