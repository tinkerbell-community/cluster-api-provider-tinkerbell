package machine //nolint:testpackage

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	. "github.com/onsi/gomega" //nolint:revive // one day we will remove gomega
	tinkv1 "github.com/tinkerbell/tinkerbell/api/v1alpha1/tinkerbell"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ipamv1 "sigs.k8s.io/cluster-api/api/ipam/v1beta2"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/cluster-api/util/patch"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	infrastructurev1 "github.com/tinkerbell/cluster-api-provider-tinkerbell/api/v1beta2"
)

const (
	ipamMachineName = "ipam-machine"
	ipamMachineUID  = "11111111-1111-1111-1111-111111111111"
	ipamNamespace   = "ipam-ns"
	ipamHardware    = "ipam-hardware"
	ipamPoolName    = "lab-pool"
	ipamMAC         = "aa:bb:cc:dd:ee:ff"
	ipamClusterName = "ipam-cluster"
	// ipamOtherMachineUID identifies a machine that is not the one under test.
	ipamOtherMachineUID = "22222222-2222-2222-2222-222222222222"
)

func ipamPool() *ipamv1.IPPoolReference {
	return &ipamv1.IPPoolReference{APIGroup: "ipam.cluster.x-k8s.io", Kind: "InClusterIPPool", Name: ipamPoolName}
}

func ipamMachine() *infrastructurev1.TinkerbellMachine {
	return &infrastructurev1.TinkerbellMachine{
		ObjectMeta: metav1.ObjectMeta{Name: ipamMachineName, Namespace: ipamNamespace, UID: types.UID(ipamMachineUID)},
		Spec: infrastructurev1.TinkerbellMachineSpec{
			TinkerbellMachineConfig: infrastructurev1.TinkerbellMachineConfig{AddressFromPool: ipamPool()},
			HardwareName:            ipamHardware,
		},
	}
}

func ipamHardwareObject(macs ...string) *tinkv1.Hardware {
	hw := &tinkv1.Hardware{ObjectMeta: metav1.ObjectMeta{Name: ipamHardware, Namespace: ipamNamespace}}
	for _, mac := range macs {
		hw.Spec.Interfaces = append(hw.Spec.Interfaces, tinkv1.Interface{DHCP: &tinkv1.DHCP{MAC: mac}})
	}

	return hw
}

// ipamScope builds a scope whose management and Tinkerbell clients are the same fake client.
func ipamScope(g Gomega, machine *infrastructurev1.TinkerbellMachine, objects ...client.Object) (*machineReconcileScope, client.Client) {
	cl := newHardwareTestClient(g, append(objects, machine)...)

	patchHelper, err := patch.NewHelper(machine, cl)
	g.Expect(err).NotTo(HaveOccurred())

	return &machineReconcileScope{
		ctx:               context.Background(),
		log:               logr.Discard(),
		client:            cl,
		tinkerbellClient:  cl,
		scheme:            hardwareTestScheme(g),
		tinkerbellMachine: machine,
		patchHelper:       patchHelper,
		machine:           &clusterv1.Machine{Spec: clusterv1.MachineSpec{ClusterName: ipamClusterName}},
	}, cl
}

// concurrentlyRenamedHardware returns a stale copy of hw alongside a client whose stored
// Hardware has since been written by someone else (a renamed interface, as the discovery
// syncer's re-apply would do). Any write built from the stale copy must be refused.
func concurrentlyRenamedHardware(g Gomega, cl client.Client, hw *tinkv1.Hardware) *tinkv1.Hardware {
	stale := &tinkv1.Hardware{}
	g.Expect(cl.Get(context.Background(), client.ObjectKeyFromObject(hw), stale)).To(Succeed())

	live := stale.DeepCopy()
	live.Spec.Interfaces[0].DHCP.Hostname = "renamed-concurrently"
	g.Expect(cl.Update(context.Background(), live)).To(Succeed())

	return stale
}

func ownedClaim(machine *infrastructurev1.TinkerbellMachine, hw *tinkv1.Hardware, mac string) *ipamv1.IPAddressClaim {
	return &ipamv1.IPAddressClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      hw.Name + "-" + ipamPoolName,
			Namespace: machine.Namespace,
			Annotations: map[string]string{
				AnnotationHardwareName:      hw.Name,
				AnnotationHardwareNamespace: hw.Namespace,
				AnnotationMACAddress:        mac,
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: infrastructurev1.GroupVersion.String(),
				Kind:       "TinkerbellMachine",
				Name:       machine.Name,
				UID:        machine.UID,
				Controller: ptr.To(true),
			}},
			Finalizers: []string{infrastructurev1.IPAddressClaimFinalizer},
		},
		Spec: ipamv1.IPAddressClaimSpec{ClusterName: ipamClusterName, PoolRef: *ipamPool()},
	}
}

func Test_reconcileIPAM_creates_claim_and_waits(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	machine := ipamMachine()
	hw := ipamHardwareObject(ipamMAC)
	scope, cl := ipamScope(g, machine, hw)

	err := scope.reconcileIPAM(hw)
	g.Expect(err).To(MatchError(errWaitingForIPAddress))

	claim := &ipamv1.IPAddressClaim{}
	g.Expect(cl.Get(context.Background(), types.NamespacedName{Name: ipamHardware + "-" + ipamPoolName, Namespace: ipamNamespace}, claim)).To(Succeed(),
		"claim is named <hardware>-<pool> in the machine's namespace")

	g.Expect(claim.Spec.PoolRef).To(Equal(*ipamPool()))
	g.Expect(claim.Spec.ClusterName).To(Equal(ipamClusterName))
	g.Expect(claim.Labels).To(HaveKeyWithValue(clusterv1.ClusterNameLabel, ipamClusterName))
	g.Expect(claim.Labels).To(HaveKeyWithValue(LabelMachineName, ipamMachineName))
	g.Expect(claim.Labels).To(HaveKeyWithValue(LabelMachineNamespace, ipamNamespace))
	g.Expect(claim.Annotations).To(HaveKeyWithValue(AnnotationHardwareName, ipamHardware))
	g.Expect(claim.Annotations).To(HaveKeyWithValue(AnnotationHardwareNamespace, ipamNamespace))
	g.Expect(claim.Annotations).To(HaveKeyWithValue(AnnotationMACAddress, ipamMAC))
	g.Expect(claim.Annotations).To(HaveKeyWithValue(AnnotationIPAMMACAddress, ipamMAC),
		"the provider-neutral MAC annotation is what an IPAM provider reads")
	g.Expect(claim.Annotations).To(HaveKeyWithValue(AnnotationIPAMHostname, ipamHardware),
		"the hostname is the Hardware name, which is what the node calls itself")
	g.Expect(metav1.IsControlledBy(claim, machine)).To(BeTrue(), "claim must be controller-owned by the TinkerbellMachine")
	g.Expect(controllerutil.ContainsFinalizer(claim, infrastructurev1.IPAddressClaimFinalizer)).To(BeTrue())

	cond := conditions.Get(machine, infrastructurev1.IPAddressClaimedCondition)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(infrastructurev1.WaitingForIPAddressReason))
}

// A claim made before the neutral annotations existed gets them on the next reconcile,
// so a provider upgrade reaches nodes without recreating their claims.
func Test_reconcileIPAM_backfills_the_neutral_annotations_on_an_owned_claim(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	machine := ipamMachine()
	hw := ipamHardwareObject(ipamMAC)
	claim := ownedClaim(machine, hw, ipamMAC)
	scope, cl := ipamScope(g, machine, hw, claim)

	g.Expect(scope.reconcileIPAM(hw)).To(MatchError(errWaitingForIPAddress))

	got := &ipamv1.IPAddressClaim{}
	g.Expect(cl.Get(context.Background(), client.ObjectKeyFromObject(claim), got)).To(Succeed())
	g.Expect(got.Annotations).To(HaveKeyWithValue(AnnotationIPAMMACAddress, ipamMAC))
	g.Expect(got.Annotations).To(HaveKeyWithValue(AnnotationIPAMHostname, ipamHardware))
	g.Expect(got.Annotations).To(HaveKeyWithValue(AnnotationMACAddress, ipamMAC), "CAPT's own keys are kept")
}

func Test_reconcileIPAM_is_a_no_op_without_a_pool(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	machine := ipamMachine()
	machine.Spec.AddressFromPool = nil
	hw := ipamHardwareObject(ipamMAC)
	scope, cl := ipamScope(g, machine, hw)

	g.Expect(scope.reconcileIPAM(hw)).To(Succeed())

	claims := &ipamv1.IPAddressClaimList{}
	g.Expect(cl.List(context.Background(), claims)).To(Succeed())
	g.Expect(claims.Items).To(BeEmpty())
	g.Expect(conditions.Get(machine, infrastructurev1.IPAddressClaimedCondition)).To(BeNil())
}

func Test_reconcileIPAM_requires_a_mac_on_the_primary_interface(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	machine := ipamMachine()
	hw := ipamHardwareObject("")
	scope, _ := ipamScope(g, machine, hw)

	err := scope.reconcileIPAM(hw)
	g.Expect(err).To(MatchError(ErrHardwareInterfaceMissingMAC))
	g.Expect(conditions.GetReason(machine, infrastructurev1.IPAddressClaimedCondition)).To(Equal(infrastructurev1.IPAddressClaimInvalidReason))
}

func Test_reconcileIPAM_does_not_recreate_a_terminating_claim(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	machine := ipamMachine()
	hw := ipamHardwareObject(ipamMAC)
	claim := ownedClaim(machine, hw, ipamMAC)
	claim.Finalizers = append(claim.Finalizers, "ipam.example.org/release")
	scope, cl := ipamScope(g, machine, hw, claim)

	// Deleting a finalized object in the fake client sets deletionTimestamp and keeps it.
	g.Expect(cl.Delete(context.Background(), claim)).To(Succeed())

	err := scope.reconcileIPAM(hw)
	g.Expect(err).To(MatchError(errWaitingForIPAddress))
	g.Expect(conditions.GetReason(machine, infrastructurev1.IPAddressClaimedCondition)).To(Equal(infrastructurev1.IPAddressClaimDeletingReason))

	persisted := &ipamv1.IPAddressClaim{}
	g.Expect(cl.Get(context.Background(), client.ObjectKeyFromObject(claim), persisted)).To(Succeed())
	g.Expect(persisted.DeletionTimestamp.IsZero()).To(BeFalse(), "the terminating claim must be left alone, not replaced")
}

func Test_reconcileIPAM_rejects_a_claim_owned_by_another_machine(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	machine := ipamMachine()
	hw := ipamHardwareObject(ipamMAC)
	other := ipamMachine()
	other.Name = "other-machine"
	other.UID = ipamOtherMachineUID
	scope, _ := ipamScope(g, machine, hw, ownedClaim(other, hw, ipamMAC))

	err := scope.reconcileIPAM(hw)
	g.Expect(err).To(MatchError(ErrIPAddressClaimConflict))
	g.Expect(conditions.GetReason(machine, infrastructurev1.IPAddressClaimedCondition)).To(Equal(infrastructurev1.IPAddressClaimInvalidReason))
}

func Test_reconcileIPAM_rejects_a_claim_made_for_other_hardware(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	machine := ipamMachine()
	hw := ipamHardwareObject(ipamMAC)
	claim := ownedClaim(machine, hw, ipamMAC)
	claim.Annotations[AnnotationHardwareNamespace] = "somewhere-else"
	scope, _ := ipamScope(g, machine, hw, claim)

	err := scope.reconcileIPAM(hw)
	g.Expect(err).To(MatchError(ErrIPAddressClaimConflict))
}

func Test_prefixToNetmask(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		prefix    int
		want      string
		expectErr bool
	}{
		"slash_0":  {prefix: 0, want: "0.0.0.0"},
		"slash_8":  {prefix: 8, want: "255.0.0.0"},
		"slash_24": {prefix: 24, want: "255.255.255.0"},
		"slash_32": {prefix: 32, want: "255.255.255.255"},
		"slash_33": {prefix: 33, expectErr: true},
		"negative": {prefix: -1, expectErr: true},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			g := NewWithT(t)

			got, err := prefixToNetmask(tc.prefix)
			if tc.expectErr {
				g.Expect(err).To(HaveOccurred())

				return
			}

			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(got).To(Equal(tc.want))
		})
	}
}

func Test_hardwareIPFromIPAddress(t *testing.T) {
	t.Parallel()

	t.Run("ipv4_gets_dotted_netmask_and_family_4", func(t *testing.T) {
		t.Parallel()
		g := NewWithT(t)

		got, err := hardwareIPFromIPAddress(&ipamv1.IPAddress{Spec: ipamv1.IPAddressSpec{
			Address: "10.1.40.32", Prefix: ptr.To[int32](24), Gateway: "10.1.40.1",
		}})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(got).To(Equal(&tinkv1.IP{Address: "10.1.40.32", Netmask: "255.255.255.0", Gateway: "10.1.40.1", Family: 4}))
	})

	t.Run("ipv6_gets_empty_netmask_and_family_6", func(t *testing.T) {
		t.Parallel()
		g := NewWithT(t)

		got, err := hardwareIPFromIPAddress(&ipamv1.IPAddress{Spec: ipamv1.IPAddressSpec{
			Address: "2001:db8::10", Prefix: ptr.To[int32](64), Gateway: "2001:db8::1",
		}})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(got).To(Equal(&tinkv1.IP{Address: "2001:db8::10", Gateway: "2001:db8::1", Family: 6}))
	})

	t.Run("ipv4_without_prefix_is_rejected", func(t *testing.T) {
		t.Parallel()
		g := NewWithT(t)

		_, err := hardwareIPFromIPAddress(&ipamv1.IPAddress{Spec: ipamv1.IPAddressSpec{Address: "10.1.40.32"}})
		g.Expect(err).To(MatchError(ErrIPAddressMissingPrefix))
	})

	t.Run("unparseable_address_is_rejected", func(t *testing.T) {
		t.Parallel()
		g := NewWithT(t)

		_, err := hardwareIPFromIPAddress(&ipamv1.IPAddress{Spec: ipamv1.IPAddressSpec{Address: "not-an-ip", Prefix: ptr.To[int32](24)}})
		g.Expect(err).To(HaveOccurred())
	})
}

func Test_primaryInterfaceMAC(t *testing.T) {
	t.Parallel()

	t.Run("returns_lowercased_mac_of_first_interface", func(t *testing.T) {
		t.Parallel()
		g := NewWithT(t)

		hw := &tinkv1.Hardware{Spec: tinkv1.HardwareSpec{Interfaces: []tinkv1.Interface{
			{DHCP: &tinkv1.DHCP{MAC: "AA:BB:CC:DD:EE:FF"}},
			{DHCP: &tinkv1.DHCP{MAC: "11:22:33:44:55:66"}},
		}}}

		mac, err := primaryInterfaceMAC(hw)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(mac).To(Equal("aa:bb:cc:dd:ee:ff"))
	})

	t.Run("rejects_missing_mac", func(t *testing.T) {
		t.Parallel()
		g := NewWithT(t)

		_, err := primaryInterfaceMAC(&tinkv1.Hardware{Spec: tinkv1.HardwareSpec{Interfaces: []tinkv1.Interface{{DHCP: &tinkv1.DHCP{}}}}})
		g.Expect(err).To(MatchError(ErrHardwareInterfaceMissingMAC))
	})

	t.Run("rejects_missing_interfaces", func(t *testing.T) {
		t.Parallel()
		g := NewWithT(t)

		_, err := primaryInterfaceMAC(&tinkv1.Hardware{})
		g.Expect(err).To(MatchError(ErrHardwareMissingInterfaces))
	})
}

func Test_interfaceIndexByMAC(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	hw := &tinkv1.Hardware{Spec: tinkv1.HardwareSpec{Interfaces: []tinkv1.Interface{
		{Netboot: &tinkv1.Netboot{}},
		{DHCP: &tinkv1.DHCP{MAC: "aa:bb:cc:dd:ee:ff"}},
	}}}

	g.Expect(interfaceIndexByMAC(hw, "AA:BB:CC:DD:EE:FF")).To(Equal(1), "matching is case-insensitive")
	g.Expect(interfaceIndexByMAC(hw, "00:00:00:00:00:00")).To(Equal(-1))
}

func boundClaim(machine *infrastructurev1.TinkerbellMachine, hw *tinkv1.Hardware, mac string) (*ipamv1.IPAddressClaim, *ipamv1.IPAddress) {
	claim := ownedClaim(machine, hw, mac)
	claim.Status.AddressRef.Name = claim.Name

	address := &ipamv1.IPAddress{
		ObjectMeta: metav1.ObjectMeta{Name: claim.Name, Namespace: claim.Namespace},
		Spec: ipamv1.IPAddressSpec{
			ClaimRef: ipamv1.IPAddressClaimReference{Name: claim.Name},
			PoolRef:  *ipamPool(),
			Address:  "10.1.40.32",
			Prefix:   ptr.To[int32](24),
			Gateway:  "10.1.40.1",
		},
	}

	return claim, address
}

func Test_reconcileIPAM_writes_the_address_to_the_interface_matching_the_claim_mac(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	machine := ipamMachine()
	// The claimed MAC sits at index 1: the write must follow the MAC, not the position.
	hw := ipamHardwareObject("11:22:33:44:55:66", ipamMAC)
	claim, address := boundClaim(machine, hw, ipamMAC)
	scope, cl := ipamScope(g, machine, hw, claim, address)

	g.Expect(scope.reconcileIPAM(hw)).To(Succeed())

	want := &tinkv1.IP{Address: "10.1.40.32", Netmask: "255.255.255.0", Gateway: "10.1.40.1", Family: 4}

	persisted := &tinkv1.Hardware{}
	g.Expect(cl.Get(context.Background(), client.ObjectKeyFromObject(hw), persisted)).To(Succeed())
	g.Expect(persisted.Spec.Interfaces[1].DHCP.IP).To(Equal(want))
	g.Expect(persisted.Spec.Interfaces[0].DHCP.IP).To(BeNil(), "the other interface must be untouched")
	g.Expect(hw.Spec.Interfaces[1].DHCP.IP).To(Equal(want), "the in-memory Hardware is updated for the rest of the reconcile")

	cond := conditions.Get(machine, infrastructurev1.IPAddressClaimedCondition)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(infrastructurev1.IPAddressClaimedReason))
}

func Test_reconcileIPAM_does_not_rewrite_a_correct_reservation(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	machine := ipamMachine()
	hw := ipamHardwareObject(ipamMAC)
	hw.Spec.Interfaces[0].DHCP.IP = &tinkv1.IP{Address: "10.1.40.32", Netmask: "255.255.255.0", Gateway: "10.1.40.1", Family: 4}
	claim, address := boundClaim(machine, hw, ipamMAC)
	scope, cl := ipamScope(g, machine, hw, claim, address)

	before := &tinkv1.Hardware{}
	g.Expect(cl.Get(context.Background(), client.ObjectKeyFromObject(hw), before)).To(Succeed())

	g.Expect(scope.reconcileIPAM(hw)).To(Succeed())

	after := &tinkv1.Hardware{}
	g.Expect(cl.Get(context.Background(), client.ObjectKeyFromObject(hw), after)).To(Succeed())
	g.Expect(after.ResourceVersion).To(Equal(before.ResourceVersion), "an already-correct reservation must not be written again")
}

func Test_reconcileIPAM_waits_when_the_ipaddress_is_not_yet_visible(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	machine := ipamMachine()
	hw := ipamHardwareObject(ipamMAC)
	claim, _ := boundClaim(machine, hw, ipamMAC)
	scope, _ := ipamScope(g, machine, hw, claim)

	g.Expect(scope.reconcileIPAM(hw)).To(MatchError(errWaitingForIPAddress))
	g.Expect(conditions.GetReason(machine, infrastructurev1.IPAddressClaimedCondition)).To(Equal(infrastructurev1.WaitingForIPAddressReason))
}

func Test_reconcileIPAM_rejects_an_ipv4_address_without_prefix(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	machine := ipamMachine()
	hw := ipamHardwareObject(ipamMAC)
	claim, address := boundClaim(machine, hw, ipamMAC)
	address.Spec.Prefix = nil
	scope, _ := ipamScope(g, machine, hw, claim, address)

	g.Expect(scope.reconcileIPAM(hw)).To(MatchError(ErrIPAddressMissingPrefix))
	g.Expect(conditions.GetReason(machine, infrastructurev1.IPAddressClaimedCondition)).To(Equal(infrastructurev1.IPAddressClaimInvalidReason))
}

func Test_reconcileIPAM_rejects_a_claim_whose_mac_is_no_longer_on_the_hardware(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	machine := ipamMachine()
	hw := ipamHardwareObject(ipamMAC)
	claim, address := boundClaim(machine, hw, "00:00:00:00:00:01")
	scope, _ := ipamScope(g, machine, hw, claim, address)

	g.Expect(scope.reconcileIPAM(hw)).To(MatchError(ErrHardwareInterfaceNotFound))
	g.Expect(conditions.GetReason(machine, infrastructurev1.IPAddressClaimedCondition)).To(Equal(infrastructurev1.IPAddressClaimInvalidReason))
}

func Test_releaseIPAddressClaim_removes_finalizer_and_deletes(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	machine := ipamMachine()
	hw := ipamHardwareObject(ipamMAC)
	claim := ownedClaim(machine, hw, ipamMAC)
	claim.Finalizers = append(claim.Finalizers, "ipam.example.org/release")
	scope, cl := ipamScope(g, machine, hw, claim)

	g.Expect(scope.releaseIPAddressClaim()).To(Succeed())

	persisted := &ipamv1.IPAddressClaim{}
	g.Expect(cl.Get(context.Background(), client.ObjectKeyFromObject(claim), persisted)).To(Succeed(),
		"the provider's own finalizer keeps the claim around until the address is released")
	g.Expect(persisted.DeletionTimestamp.IsZero()).To(BeFalse(), "claim must be deleted")
	g.Expect(controllerutil.ContainsFinalizer(persisted, infrastructurev1.IPAddressClaimFinalizer)).To(BeFalse(), "CAPT's finalizer must be gone")
	g.Expect(persisted.Finalizers).To(ContainElement("ipam.example.org/release"), "foreign finalizers are not CAPT's to remove")
}

func Test_releaseIPAddressClaim_tolerates_a_missing_claim(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	machine := ipamMachine()
	scope, _ := ipamScope(g, machine)

	g.Expect(scope.releaseIPAddressClaim()).To(Succeed())
}

func Test_releaseIPAddressClaim_leaves_a_foreign_claim_alone(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	machine := ipamMachine()
	hw := ipamHardwareObject(ipamMAC)
	other := ipamMachine()
	other.UID = ipamOtherMachineUID
	claim := ownedClaim(other, hw, ipamMAC)
	scope, cl := ipamScope(g, machine, hw, claim)

	g.Expect(scope.releaseIPAddressClaim()).To(Succeed())

	persisted := &ipamv1.IPAddressClaim{}
	g.Expect(cl.Get(context.Background(), client.ObjectKeyFromObject(claim), persisted)).To(Succeed())
	g.Expect(persisted.DeletionTimestamp.IsZero()).To(BeTrue())
	g.Expect(controllerutil.ContainsFinalizer(persisted, infrastructurev1.IPAddressClaimFinalizer)).To(BeTrue())
}

func Test_releaseHardware_clears_the_ipam_reservation_by_mac(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	machine := ipamMachine()
	hw := ipamHardwareObject("11:22:33:44:55:66", ipamMAC)
	static := &tinkv1.IP{Address: "192.0.2.1", Netmask: "255.255.255.0", Family: 4}
	hw.Spec.Interfaces[0].DHCP.IP = static
	hw.Spec.Interfaces[1].DHCP.IP = &tinkv1.IP{Address: "10.1.40.32", Netmask: "255.255.255.0", Family: 4}
	claim, address := boundClaim(machine, hw, ipamMAC)
	scope, cl := ipamScope(g, machine, hw, claim, address)

	g.Expect(scope.releaseHardware(hw)).To(Succeed())

	persisted := &tinkv1.Hardware{}
	g.Expect(cl.Get(context.Background(), client.ObjectKeyFromObject(hw), persisted)).To(Succeed())
	g.Expect(persisted.Spec.Interfaces[1].DHCP.IP).To(BeNil(), "the IPAM reservation must be cleared")
	g.Expect(persisted.Spec.Interfaces[0].DHCP.IP).To(Equal(static), "an interface CAPT never wrote to must be untouched")
}

func Test_releaseHardware_falls_back_to_the_primary_interface_when_the_claim_is_gone(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	machine := ipamMachine()
	hw := ipamHardwareObject(ipamMAC)
	hw.Spec.Interfaces[0].DHCP.IP = &tinkv1.IP{Address: "10.1.40.32", Netmask: "255.255.255.0", Family: 4}
	scope, cl := ipamScope(g, machine, hw)

	g.Expect(scope.releaseHardware(hw)).To(Succeed())

	persisted := &tinkv1.Hardware{}
	g.Expect(cl.Get(context.Background(), client.ObjectKeyFromObject(hw), persisted)).To(Succeed())
	g.Expect(persisted.Spec.Interfaces[0].DHCP.IP).To(BeNil())
}

func Test_releaseHardware_keeps_the_reservation_without_a_pool(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	machine := ipamMachine()
	machine.Spec.AddressFromPool = nil
	hw := ipamHardwareObject(ipamMAC)
	static := &tinkv1.IP{Address: "10.1.40.32", Netmask: "255.255.255.0", Family: 4}
	hw.Spec.Interfaces[0].DHCP.IP = static
	scope, cl := ipamScope(g, machine, hw)

	g.Expect(scope.releaseHardware(hw)).To(Succeed())

	persisted := &tinkv1.Hardware{}
	g.Expect(cl.Get(context.Background(), client.ObjectKeyFromObject(hw), persisted)).To(Succeed())
	g.Expect(persisted.Spec.Interfaces[0].DHCP.IP).To(Equal(static), "a hand-managed address is not CAPT's to clear")
}

func Test_setStatus_reports_the_ipam_address_even_when_its_interface_is_not_first(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	machine := ipamMachine()
	hw := ipamHardwareObject("11:22:33:44:55:66", ipamMAC)
	claim, address := boundClaim(machine, hw, ipamMAC)
	scope, _ := ipamScope(g, machine, hw, claim, address)

	g.Expect(scope.reconcileIPAM(hw)).To(Succeed())
	g.Expect(scope.setStatus(hw)).To(Succeed(), "the address CAPT just reserved must be the one it reports")

	g.Expect(machine.Status.Addresses).To(ConsistOf(corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: "10.1.40.32"}))
}

func Test_ensureHardwareAddress_refuses_to_overwrite_a_concurrent_interfaces_write(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	machine := ipamMachine()
	hw := ipamHardwareObject(ipamMAC)
	scope, cl := ipamScope(g, machine, hw)
	stale := concurrentlyRenamedHardware(g, cl, hw)

	err := scope.ensureHardwareAddress(stale, ipamMAC, &tinkv1.IP{Address: "10.1.40.32", Netmask: "255.255.255.0", Family: 4})
	g.Expect(err).To(MatchError(errHardwareClaimRequeue), "a lost race must requeue, not clobber")

	persisted := &tinkv1.Hardware{}
	g.Expect(cl.Get(context.Background(), client.ObjectKeyFromObject(hw), persisted)).To(Succeed())
	g.Expect(persisted.Spec.Interfaces[0].DHCP.Hostname).To(Equal("renamed-concurrently"), "the concurrent write must survive")
	g.Expect(persisted.Spec.Interfaces[0].DHCP.IP).To(BeNil())
}

func Test_releaseHardware_refuses_to_overwrite_a_concurrent_interfaces_write(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	machine := ipamMachine()
	hw := ipamHardwareObject(ipamMAC)
	hw.Labels = map[string]string{HardwareOwnerNameLabel: ipamMachineName, HardwareOwnerNamespaceLabel: ipamNamespace}
	scope, cl := ipamScope(g, machine, hw)
	stale := concurrentlyRenamedHardware(g, cl, hw)

	g.Expect(scope.releaseHardware(stale)).NotTo(Succeed(), "a lost race must fail, not clobber")

	persisted := &tinkv1.Hardware{}
	g.Expect(cl.Get(context.Background(), client.ObjectKeyFromObject(hw), persisted)).To(Succeed())
	g.Expect(persisted.Spec.Interfaces[0].DHCP.Hostname).To(Equal("renamed-concurrently"), "the concurrent write must survive")
	g.Expect(persisted.Labels).To(HaveKey(HardwareOwnerNameLabel), "nothing from the stale copy may have been applied")
}

func Test_reconcileIPAM_waits_for_a_previous_machines_terminating_claim(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	machine := ipamMachine()
	hw := ipamHardwareObject(ipamMAC)
	previous := ipamMachine()
	previous.Name = "previous-machine"
	previous.UID = ipamOtherMachineUID
	claim := ownedClaim(previous, hw, ipamMAC)
	claim.Finalizers = append(claim.Finalizers, "ipam.example.org/release")
	scope, cl := ipamScope(g, machine, hw, claim)

	// The provider's finalizer keeps the previous machine's claim around for a while.
	g.Expect(cl.Delete(context.Background(), claim)).To(Succeed())

	err := scope.reconcileIPAM(hw)
	g.Expect(err).To(MatchError(errWaitingForIPAddress), "machine replacement on the same hardware is a wait, not a conflict")
	g.Expect(conditions.GetReason(machine, infrastructurev1.IPAddressClaimedCondition)).To(Equal(infrastructurev1.IPAddressClaimDeletingReason))
}

func Test_releaseHardware_ignores_the_mac_recorded_on_a_foreign_claim(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	machine := ipamMachine()
	hw := ipamHardwareObject(ipamMAC, "11:22:33:44:55:66")
	hw.Spec.Interfaces[0].DHCP.IP = &tinkv1.IP{Address: "10.1.40.32", Netmask: "255.255.255.0", Family: 4}
	foreign := &tinkv1.IP{Address: "10.1.40.33", Netmask: "255.255.255.0", Family: 4}
	hw.Spec.Interfaces[1].DHCP.IP = foreign
	other := ipamMachine()
	other.UID = ipamOtherMachineUID
	scope, cl := ipamScope(g, machine, hw, ownedClaim(other, hw, "11:22:33:44:55:66"))

	g.Expect(scope.releaseHardware(hw)).To(Succeed())

	persisted := &tinkv1.Hardware{}
	g.Expect(cl.Get(context.Background(), client.ObjectKeyFromObject(hw), persisted)).To(Succeed())
	g.Expect(persisted.Spec.Interfaces[0].DHCP.IP).To(BeNil(), "this machine's reservation lives on the primary interface")
	g.Expect(persisted.Spec.Interfaces[1].DHCP.IP).To(Equal(foreign), "a claim CAPT does not control says nothing about what to clear")
}
