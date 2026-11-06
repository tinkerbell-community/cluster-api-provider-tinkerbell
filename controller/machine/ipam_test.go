package machine //nolint:testpackage

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	. "github.com/onsi/gomega" //nolint:revive // one day we will remove gomega
	"github.com/tinkerbell/tinkerbell/api/conversion"
	tinkv1 "github.com/tinkerbell/tinkerbell/api/v1alpha1/tinkerbell"
	tinkv2 "github.com/tinkerbell/tinkerbell/api/v1alpha2/tinkerbell"
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
	ipamOtherMAC    = "11:22:33:44:55:66"
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

// ipamHardwareObject returns Hardware with an interface per MAC. The first MAC is its primary
// interface: the Hardware's agentID names it.
func ipamHardwareObject(macs ...string) *tinkv2.Hardware {
	hw := &tinkv2.Hardware{ObjectMeta: metav1.ObjectMeta{Name: ipamHardware, Namespace: ipamNamespace}}
	for i, mac := range macs {
		if i == 0 {
			hw.Spec.AgentID = mac
		}

		if hw.Spec.NetworkInterfaces == nil {
			hw.Spec.NetworkInterfaces = tinkv2.NetworkInterfaces{}
		}

		hw.Spec.NetworkInterfaces[tinkv2.MAC(mac)] = tinkv2.NetworkInterface{}
	}

	return hw
}

// setIPv4 sets the IPv4 configuration of hw's interface mac.
func setIPv4(hw *tinkv2.Hardware, mac string, ip *tinkv2.IP) {
	ni := hw.Spec.NetworkInterfaces[tinkv2.MAC(mac)]
	if ni.IPAM == nil {
		ni.IPAM = &tinkv2.IPAM{}
	}

	ni.IPAM.IPv4 = ip
	hw.Spec.NetworkInterfaces[tinkv2.MAC(mac)] = ni
}

// ipv4Of returns the IPv4 configuration of hw's interface mac, or nil.
func ipv4Of(hw *tinkv2.Hardware, mac string) *tinkv2.IP {
	return interfaceAddress(hw.Spec.NetworkInterfaces[tinkv2.MAC(mac)], ipv4Family)
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
func concurrentlyRenamedHardware(g Gomega, cl client.Client, hw *tinkv2.Hardware) *tinkv2.Hardware {
	stale := &tinkv2.Hardware{}
	g.Expect(cl.Get(context.Background(), client.ObjectKeyFromObject(hw), stale)).To(Succeed())

	live := stale.DeepCopy()
	setIPv4(live, ipamMAC, &tinkv2.IP{Hostname: ptr.To("renamed-concurrently")})
	g.Expect(cl.Update(context.Background(), live)).To(Succeed())

	return stale
}

func ownedClaim(machine *infrastructurev1.TinkerbellMachine, hw *tinkv2.Hardware, mac string) *ipamv1.IPAddressClaim {
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
				Controller: new(true),
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

func Test_prefixLength(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		prefix    int32
		family    int
		want      string
		expectErr bool
	}{
		"ipv4_slash_0":   {prefix: 0, family: ipv4Family, want: "0"},
		"ipv4_slash_24":  {prefix: 24, family: ipv4Family, want: "24"},
		"ipv4_slash_32":  {prefix: 32, family: ipv4Family, want: "32"},
		"ipv4_slash_33":  {prefix: 33, family: ipv4Family, expectErr: true},
		"ipv6_slash_64":  {prefix: 64, family: ipv6Family, want: "64"},
		"ipv6_slash_128": {prefix: 128, family: ipv6Family, want: "128"},
		"ipv6_slash_129": {prefix: 129, family: ipv6Family, expectErr: true},
		"negative":       {prefix: -1, family: ipv4Family, expectErr: true},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			g := NewWithT(t)

			got, err := prefixLength(tc.prefix, tc.family)
			if tc.expectErr {
				g.Expect(err).To(HaveOccurred())

				return
			}

			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(got).To(Equal(tc.want))
		})
	}
}

func Test_hardwareAddressFromIPAddress(t *testing.T) {
	t.Parallel()

	t.Run("ipv4_gets_its_prefix_length", func(t *testing.T) {
		t.Parallel()
		g := NewWithT(t)

		got, err := hardwareAddressFromIPAddress(&ipamv1.IPAddress{Spec: ipamv1.IPAddressSpec{
			Address: "10.1.40.32", Prefix: ptr.To[int32](24), Gateway: "10.1.40.1",
		}})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(got).To(Equal(&hardwareAddress{family: ipv4Family, address: "10.1.40.32", prefix: "24", gateway: "10.1.40.1"}))
	})

	t.Run("ipv6_is_family_6", func(t *testing.T) {
		t.Parallel()
		g := NewWithT(t)

		got, err := hardwareAddressFromIPAddress(&ipamv1.IPAddress{Spec: ipamv1.IPAddressSpec{
			Address: "2001:db8::10", Prefix: ptr.To[int32](64), Gateway: "2001:db8::1",
		}})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(got).To(Equal(&hardwareAddress{family: ipv6Family, address: "2001:db8::10", prefix: "64", gateway: "2001:db8::1"}))
	})

	t.Run("ipv6_without_prefix_is_accepted", func(t *testing.T) {
		t.Parallel()
		g := NewWithT(t)

		got, err := hardwareAddressFromIPAddress(&ipamv1.IPAddress{Spec: ipamv1.IPAddressSpec{Address: "2001:db8::10"}})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(got).To(Equal(&hardwareAddress{family: ipv6Family, address: "2001:db8::10"}))
	})

	t.Run("ipv4_without_prefix_is_rejected", func(t *testing.T) {
		t.Parallel()
		g := NewWithT(t)

		_, err := hardwareAddressFromIPAddress(&ipamv1.IPAddress{Spec: ipamv1.IPAddressSpec{Address: "10.1.40.32"}})
		g.Expect(err).To(MatchError(ErrIPAddressMissingPrefix))
	})

	t.Run("unparseable_address_is_rejected", func(t *testing.T) {
		t.Parallel()
		g := NewWithT(t)

		_, err := hardwareAddressFromIPAddress(&ipamv1.IPAddress{Spec: ipamv1.IPAddressSpec{Address: "not-an-ip", Prefix: ptr.To[int32](24)}})
		g.Expect(err).To(HaveOccurred())
	})
}

func Test_primaryInterfaceMAC(t *testing.T) {
	t.Parallel()

	t.Run("prefers_the_interface_the_agent_id_names", func(t *testing.T) {
		t.Parallel()
		g := NewWithT(t)

		hw := &tinkv2.Hardware{Spec: tinkv2.HardwareSpec{
			AgentID: "AA:BB:CC:DD:EE:FF",
			NetworkInterfaces: tinkv2.NetworkInterfaces{
				"aa:bb:cc:dd:ee:ff": {},
				"11:22:33:44:55:66": {},
			},
		}}

		mac, err := primaryInterfaceMAC(hw)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(mac).To(Equal("aa:bb:cc:dd:ee:ff"))
	})

	t.Run("falls_back_to_the_lowest_mac", func(t *testing.T) {
		t.Parallel()
		g := NewWithT(t)

		hw := &tinkv2.Hardware{Spec: tinkv2.HardwareSpec{
			AgentID: "not-a-mac",
			NetworkInterfaces: tinkv2.NetworkInterfaces{
				"aa:bb:cc:dd:ee:ff": {},
				"11:22:33:44:55:66": {},
			},
		}}

		mac, err := primaryInterfaceMAC(hw)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(mac).To(Equal("11:22:33:44:55:66"))
	})

	t.Run("keeps_the_first_v1alpha1_interface_of_hardware_created_through_v1alpha1", func(t *testing.T) {
		t.Parallel()
		g := NewWithT(t)

		// The first v1alpha1 interface sorts last, and the agentID names another interface:
		// CAPT must keep using the interface it used before the migration.
		v1 := &tinkv1.Hardware{
			ObjectMeta: metav1.ObjectMeta{Name: ipamHardware, Namespace: ipamNamespace},
			Spec: tinkv1.HardwareSpec{
				AgentID: "11:22:33:44:55:66",
				Interfaces: []tinkv1.Interface{
					{DHCP: &tinkv1.DHCP{}},
					{DHCP: &tinkv1.DHCP{MAC: "AA:BB:CC:DD:EE:FF"}},
					{DHCP: &tinkv1.DHCP{MAC: "11:22:33:44:55:66"}},
				},
			},
		}
		hw := &tinkv2.Hardware{}
		g.Expect(conversion.HardwareToV1Alpha2(v1, hw)).To(Succeed())

		mac, err := primaryInterfaceMAC(hw)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(mac).To(Equal("aa:bb:cc:dd:ee:ff"))
	})

	t.Run("rejects_missing_mac", func(t *testing.T) {
		t.Parallel()
		g := NewWithT(t)

		_, err := primaryInterfaceMAC(&tinkv2.Hardware{Spec: tinkv2.HardwareSpec{NetworkInterfaces: tinkv2.NetworkInterfaces{"": {}}}})
		g.Expect(err).To(MatchError(ErrHardwareInterfaceMissingMAC))
	})

	t.Run("rejects_missing_interfaces", func(t *testing.T) {
		t.Parallel()
		g := NewWithT(t)

		_, err := primaryInterfaceMAC(&tinkv2.Hardware{})
		g.Expect(err).To(MatchError(ErrHardwareMissingInterfaces))
	})
}

func Test_interfaceKey(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	hw := ipamHardwareObject(ipamOtherMAC, ipamMAC)

	key, ok := interfaceKey(hw, "AA:BB:CC:DD:EE:FF")
	g.Expect(ok).To(BeTrue(), "matching is case-insensitive")
	g.Expect(key).To(Equal(tinkv2.MAC(ipamMAC)))

	_, ok = interfaceKey(hw, "00:00:00:00:00:00")
	g.Expect(ok).To(BeFalse())

	_, ok = interfaceKey(hw, "")
	g.Expect(ok).To(BeFalse())
}

func boundClaim(machine *infrastructurev1.TinkerbellMachine, hw *tinkv2.Hardware, mac string) (*ipamv1.IPAddressClaim, *ipamv1.IPAddress) {
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
	// The claimed MAC is not the primary interface: the write must follow the MAC.
	hw := ipamHardwareObject(ipamOtherMAC, ipamMAC)
	claim, address := boundClaim(machine, hw, ipamMAC)
	scope, cl := ipamScope(g, machine, hw, claim, address)

	g.Expect(scope.reconcileIPAM(hw)).To(Succeed())

	want := &tinkv2.IP{Address: "10.1.40.32", Prefix: "24", Gateway: "10.1.40.1"}

	persisted := &tinkv2.Hardware{}
	g.Expect(cl.Get(context.Background(), client.ObjectKeyFromObject(hw), persisted)).To(Succeed())
	g.Expect(ipv4Of(persisted, ipamMAC)).To(Equal(want))
	g.Expect(persisted.Spec.NetworkInterfaces[ipamOtherMAC].IPAM).To(BeNil(), "the other interface must be untouched")
	g.Expect(ipv4Of(hw, ipamMAC)).To(Equal(want), "the in-memory Hardware is updated for the rest of the reconcile")

	cond := conditions.Get(machine, infrastructurev1.IPAddressClaimedCondition)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(infrastructurev1.IPAddressClaimedReason))
}

// spec.networkInterfaces[mac].ipam.ipv4 also holds the hostname, nameservers and more, which
// CAPT does not own: writing the address must leave them, and the interface's other
// configuration, as they are.
func Test_reconcileIPAM_writes_only_the_address_prefix_and_gateway(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	machine := ipamMachine()
	hw := ipamHardwareObject(ipamMAC)
	ni := hw.Spec.NetworkInterfaces[ipamMAC]
	ni.IPAM = &tinkv2.IPAM{
		IPv4: &tinkv2.IP{Address: "192.0.2.10", Prefix: "16", Hostname: ptr.To("node-1"), Nameservers: []tinkv2.Nameserver{"1.1.1.1"}},
		IPv6: &tinkv2.IP{Address: "2001:db8::10", Prefix: "64"},
	}
	ni.Netboot = &tinkv2.Netboot{Disabled: true}
	hw.Spec.NetworkInterfaces[ipamMAC] = ni
	claim, address := boundClaim(machine, hw, ipamMAC)
	scope, cl := ipamScope(g, machine, hw, claim, address)

	g.Expect(scope.reconcileIPAM(hw)).To(Succeed())

	persisted := &tinkv2.Hardware{}
	g.Expect(cl.Get(context.Background(), client.ObjectKeyFromObject(hw), persisted)).To(Succeed())

	got := persisted.Spec.NetworkInterfaces[ipamMAC]
	g.Expect(got.IPAM.IPv4).To(Equal(&tinkv2.IP{
		Address: "10.1.40.32", Prefix: "24", Gateway: "10.1.40.1",
		Hostname: ptr.To("node-1"), Nameservers: []tinkv2.Nameserver{"1.1.1.1"},
	}))
	g.Expect(got.IPAM.IPv6).To(Equal(&tinkv2.IP{Address: "2001:db8::10", Prefix: "64"}), "the other family is not CAPT's")
	g.Expect(got.Netboot).To(Equal(&tinkv2.Netboot{Disabled: true}))
}

func Test_reconcileIPAM_writes_an_ipv6_address_to_ipv6(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	machine := ipamMachine()
	hw := ipamHardwareObject(ipamMAC)
	claim, address := boundClaim(machine, hw, ipamMAC)
	address.Spec.Address, address.Spec.Prefix, address.Spec.Gateway = "2001:db8::32", ptr.To[int32](64), "2001:db8::1"
	scope, cl := ipamScope(g, machine, hw, claim, address)

	g.Expect(scope.reconcileIPAM(hw)).To(Succeed())

	persisted := &tinkv2.Hardware{}
	g.Expect(cl.Get(context.Background(), client.ObjectKeyFromObject(hw), persisted)).To(Succeed())
	g.Expect(persisted.Spec.NetworkInterfaces[ipamMAC].IPAM).To(Equal(&tinkv2.IPAM{
		IPv6: &tinkv2.IP{Address: "2001:db8::32", Prefix: "64", Gateway: "2001:db8::1"},
	}))
}

func Test_reconcileIPAM_does_not_rewrite_a_correct_reservation(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	machine := ipamMachine()
	hw := ipamHardwareObject(ipamMAC)
	setIPv4(hw, ipamMAC, &tinkv2.IP{Address: "10.1.40.32", Prefix: "24", Gateway: "10.1.40.1"})
	claim, address := boundClaim(machine, hw, ipamMAC)
	scope, cl := ipamScope(g, machine, hw, claim, address)

	before := &tinkv2.Hardware{}
	g.Expect(cl.Get(context.Background(), client.ObjectKeyFromObject(hw), before)).To(Succeed())

	g.Expect(scope.reconcileIPAM(hw)).To(Succeed())

	after := &tinkv2.Hardware{}
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
	hw := ipamHardwareObject(ipamOtherMAC, ipamMAC)
	static := &tinkv2.IP{Address: "192.0.2.1", Prefix: "24"}
	setIPv4(hw, ipamOtherMAC, static)
	setIPv4(hw, ipamMAC, &tinkv2.IP{Address: "10.1.40.32", Prefix: "24", Gateway: "10.1.40.1"})
	claim, address := boundClaim(machine, hw, ipamMAC)
	scope, cl := ipamScope(g, machine, hw, claim, address)

	g.Expect(scope.releaseHardware(hw)).To(Succeed())

	persisted := &tinkv2.Hardware{}
	g.Expect(cl.Get(context.Background(), client.ObjectKeyFromObject(hw), persisted)).To(Succeed())
	g.Expect(persisted.Spec.NetworkInterfaces[ipamMAC].IPAM).To(BeNil(), "the IPAM reservation must be cleared")
	g.Expect(ipv4Of(persisted, ipamOtherMAC)).To(Equal(static), "an interface CAPT never wrote to must be untouched")
}

// Clearing the reservation removes what CAPT wrote and nothing else: the rest of the IPv4
// configuration and a hand-managed address of the other family stay.
func Test_releaseHardware_clears_only_what_ipam_wrote(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	machine := ipamMachine()
	hw := ipamHardwareObject(ipamMAC)
	ni := hw.Spec.NetworkInterfaces[ipamMAC]
	ni.IPAM = &tinkv2.IPAM{
		IPv4: &tinkv2.IP{Address: "10.1.40.32", Prefix: "24", Gateway: "10.1.40.1", Hostname: ptr.To("node-1")},
		IPv6: &tinkv2.IP{Address: "2001:db8::10", Prefix: "64"},
	}
	hw.Spec.NetworkInterfaces[ipamMAC] = ni
	claim, address := boundClaim(machine, hw, ipamMAC)
	scope, cl := ipamScope(g, machine, hw, claim, address)

	g.Expect(scope.releaseHardware(hw)).To(Succeed())

	persisted := &tinkv2.Hardware{}
	g.Expect(cl.Get(context.Background(), client.ObjectKeyFromObject(hw), persisted)).To(Succeed())
	g.Expect(persisted.Spec.NetworkInterfaces[ipamMAC].IPAM).To(Equal(&tinkv2.IPAM{
		IPv4: &tinkv2.IP{Hostname: ptr.To("node-1")},
		IPv6: &tinkv2.IP{Address: "2001:db8::10", Prefix: "64"},
	}))
}

func Test_releaseHardware_falls_back_to_the_primary_interface_when_the_claim_is_gone(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	machine := ipamMachine()
	hw := ipamHardwareObject(ipamMAC)
	setIPv4(hw, ipamMAC, &tinkv2.IP{Address: "10.1.40.32", Prefix: "24"})
	scope, cl := ipamScope(g, machine, hw)

	g.Expect(scope.releaseHardware(hw)).To(Succeed())

	persisted := &tinkv2.Hardware{}
	g.Expect(cl.Get(context.Background(), client.ObjectKeyFromObject(hw), persisted)).To(Succeed())
	g.Expect(persisted.Spec.NetworkInterfaces[ipamMAC].IPAM).To(BeNil())
}

func Test_releaseHardware_keeps_the_reservation_without_a_pool(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	machine := ipamMachine()
	machine.Spec.AddressFromPool = nil
	hw := ipamHardwareObject(ipamMAC)
	static := &tinkv2.IP{Address: "10.1.40.32", Prefix: "24"}
	setIPv4(hw, ipamMAC, static)
	scope, cl := ipamScope(g, machine, hw)

	g.Expect(scope.releaseHardware(hw)).To(Succeed())

	persisted := &tinkv2.Hardware{}
	g.Expect(cl.Get(context.Background(), client.ObjectKeyFromObject(hw), persisted)).To(Succeed())
	g.Expect(ipv4Of(persisted, ipamMAC)).To(Equal(static), "a hand-managed address is not CAPT's to clear")
}

func Test_setStatus_reports_the_ipam_address_even_when_its_interface_is_not_primary(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	machine := ipamMachine()
	hw := ipamHardwareObject(ipamOtherMAC, ipamMAC)
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

	err := scope.ensureHardwareAddress(stale, ipamMAC, &hardwareAddress{family: ipv4Family, address: "10.1.40.32", prefix: "24"})
	g.Expect(err).To(MatchError(errHardwareClaimRequeue), "a lost race must requeue, not clobber")

	persisted := &tinkv2.Hardware{}
	g.Expect(cl.Get(context.Background(), client.ObjectKeyFromObject(hw), persisted)).To(Succeed())
	g.Expect(ipv4Of(persisted, ipamMAC)).To(Equal(&tinkv2.IP{Hostname: ptr.To("renamed-concurrently")}),
		"the concurrent write must survive, and nothing from the stale copy may have been applied")
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

	persisted := &tinkv2.Hardware{}
	g.Expect(cl.Get(context.Background(), client.ObjectKeyFromObject(hw), persisted)).To(Succeed())
	g.Expect(ipv4Of(persisted, ipamMAC)).To(Equal(&tinkv2.IP{Hostname: ptr.To("renamed-concurrently")}), "the concurrent write must survive")
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
	hw := ipamHardwareObject(ipamMAC, ipamOtherMAC)
	setIPv4(hw, ipamMAC, &tinkv2.IP{Address: "10.1.40.32", Prefix: "24"})
	foreign := &tinkv2.IP{Address: "10.1.40.33", Prefix: "24"}
	setIPv4(hw, ipamOtherMAC, foreign)
	other := ipamMachine()
	other.UID = ipamOtherMachineUID
	scope, cl := ipamScope(g, machine, hw, ownedClaim(other, hw, ipamOtherMAC))

	g.Expect(scope.releaseHardware(hw)).To(Succeed())

	persisted := &tinkv2.Hardware{}
	g.Expect(cl.Get(context.Background(), client.ObjectKeyFromObject(hw), persisted)).To(Succeed())
	g.Expect(persisted.Spec.NetworkInterfaces[ipamMAC].IPAM).To(BeNil(), "this machine's reservation lives on the primary interface")
	g.Expect(ipv4Of(persisted, ipamOtherMAC)).To(Equal(foreign), "a claim CAPT does not control says nothing about what to clear")
}
