package machine

import (
	"errors"
	"fmt"
	"net/netip"
	"reflect"
	"strconv"
	"strings"

	tinkv2 "github.com/tinkerbell/tinkerbell/api/v1alpha2/tinkerbell"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ipamv1 "sigs.k8s.io/cluster-api/api/ipam/v1beta2"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	infrastructurev1 "github.com/tinkerbell/cluster-api-provider-tinkerbell/api/v1beta2"
)

const (
	// AnnotationHardwareName records, on an IPAddressClaim, the Hardware the claim was made for.
	AnnotationHardwareName = "capt.tinkerbell.org/hardware-name"
	// AnnotationHardwareNamespace records the namespace of that Hardware.
	AnnotationHardwareNamespace = "capt.tinkerbell.org/hardware-namespace"
	// AnnotationMACAddress records the MAC of the Hardware interface the address is for. It
	// is the key the allocated address is written back under, and is available to IPAM
	// providers that can reserve by MAC.
	AnnotationMACAddress = "capt.tinkerbell.org/mac-address"

	// AnnotationIPAMMACAddress tells the IPAM provider, in provider-neutral terms, the MAC
	// to reserve the address for. It duplicates AnnotationMACAddress under a key no
	// provider has to know CAPT to read.
	AnnotationIPAMMACAddress = "ipam.cluster.x-k8s.io/mac-address"
	// AnnotationIPAMHostname tells the IPAM provider the name the node goes by: its
	// Hardware name, which Tinkerbell already hands it as hostname.
	AnnotationIPAMHostname = "ipam.cluster.x-k8s.io/hostname"

	ipv4Bits   = 32
	ipv6Bits   = 128
	ipv4Family = 4
	ipv6Family = 6
)

var (
	// errWaitingForIPAddress signals that the IPAM provider has not answered the claim yet.
	errWaitingForIPAddress = errors.New("waiting for IP address allocation")
	// ErrHardwareInterfaceMissingMAC is returned when the primary interface has no MAC to
	// reserve an address for.
	ErrHardwareInterfaceMissingMAC = errors.New("hardware's primary interface has no MAC address")
	// ErrIPAddressMissingPrefix is returned when an IPv4 IPAddress carries no prefix; smee
	// refuses an IPv4 reservation without one.
	ErrIPAddressMissingPrefix = errors.New("IPAddress has no prefix")
	// ErrHardwareInterfaceNotFound is returned when no Hardware interface carries the MAC the
	// claim was made for.
	ErrHardwareInterfaceNotFound = errors.New("no hardware interface matches the claim's MAC address")
	// ErrIPAddressClaimConflict is returned when the claim CAPT would use belongs to a
	// different machine or was made for a different Hardware.
	ErrIPAddressClaimConflict = errors.New("IPAddressClaim belongs to a different machine or hardware")
)

// hardwareAddress is an address CAPT reserves on a Hardware interface: the address, prefix and
// gateway of the interface's spec.networkInterfaces[mac].ipam.ipv4, or ipv6 for an IPv6 address.
// The other fields of that IP configuration (hostname, nameservers, ...) are not CAPT's.
type hardwareAddress struct {
	family  int
	address string
	prefix  string
	gateway string
}

// primaryInterfaceMAC returns the lowercased MAC of the interface CAPT reserves addresses
// for: the primary interface, the same one hardwareIP reports the node address from.
func primaryInterfaceMAC(hw *tinkv2.Hardware) (string, error) {
	mac, err := primaryInterface(hw)
	if err != nil {
		return "", err
	}

	if mac == "" {
		return "", ErrHardwareInterfaceMissingMAC
	}

	return strings.ToLower(string(mac)), nil
}

// prefixLength validates an IP prefix length for the family and renders it as v1alpha2 stores it.
func prefixLength(prefix int32, family int) (string, error) {
	bits := int32(ipv4Bits)
	if family == ipv6Family {
		bits = ipv6Bits
	}

	if prefix < 0 || prefix > bits {
		return "", fmt.Errorf("prefix /%d is not a valid IPv%d prefix length", prefix, family)
	}

	return strconv.Itoa(int(prefix)), nil
}

// hardwareAddressFromIPAddress translates an allocated IPAddress into the reservation Tinkerbell
// serves. An IPv4 address needs a prefix: smee rejects an IPv4 reservation without one.
func hardwareAddressFromIPAddress(addr *ipamv1.IPAddress) (*hardwareAddress, error) {
	parsed, err := netip.ParseAddr(addr.Spec.Address)
	if err != nil {
		return nil, fmt.Errorf("parsing IPAddress %q: %w", addr.Name, err)
	}

	ip := &hardwareAddress{family: ipv4Family, address: addr.Spec.Address, gateway: addr.Spec.Gateway}
	if !parsed.Unmap().Is4() {
		ip.family = ipv6Family
	}

	if addr.Spec.Prefix == nil {
		if ip.family == ipv4Family {
			return nil, fmt.Errorf("%w: %q", ErrIPAddressMissingPrefix, addr.Name)
		}

		return ip, nil
	}

	if ip.prefix, err = prefixLength(*addr.Spec.Prefix, ip.family); err != nil {
		return nil, fmt.Errorf("IPAddress %q: %w", addr.Name, err)
	}

	return ip, nil
}

// interfaceAddress returns the IP configuration of ni that holds addresses of family.
func interfaceAddress(ni tinkv2.NetworkInterface, family int) *tinkv2.IP {
	if ni.IPAM == nil {
		return nil
	}

	if family == ipv6Family {
		return ni.IPAM.IPv6
	}

	return ni.IPAM.IPv4
}

// heldBy reports whether ip already carries the address, prefix and gateway of addr.
func (addr *hardwareAddress) heldBy(ip *tinkv2.IP) bool {
	return ip != nil && ip.Address == addr.address && ip.Prefix == addr.prefix && ip.Gateway == addr.gateway
}

// reconcileIPAM claims an address from spec.addressFromPool for the Hardware's primary
// interface and reserves it on the Hardware once the IPAM provider has answered.
//
// The write-back locates the interface by the MAC recorded on the claim, so a change in which
// interface is primary cannot direct the address at the wrong NIC.
func (scope *machineReconcileScope) reconcileIPAM(hw *tinkv2.Hardware) error {
	pool := scope.tinkerbellMachine.Spec.AddressFromPool
	if pool == nil {
		return nil
	}

	mac, err := primaryInterfaceMAC(hw)
	if err != nil {
		scope.setIPAddressClaimedCondition(metav1.ConditionFalse, infrastructurev1.IPAddressClaimInvalidReason, err.Error())

		return fmt.Errorf("resolving hardware interface for IPAM: %w", err)
	}

	claim, err := scope.ensureIPAddressClaim(hw, mac, pool)
	if err != nil {
		return fmt.Errorf("ensuring IPAddressClaim: %w", err)
	}

	if !claim.DeletionTimestamp.IsZero() {
		// Ours: someone deleted it under a machine that may be using the address. Not ours:
		// the previous machine on this Hardware is still handing its address back.
		msg := fmt.Sprintf("IPAddressClaim %s is being deleted; not replacing it while the machine may be using its address", claim.Name)
		if !metav1.IsControlledBy(claim, scope.tinkerbellMachine) {
			msg = fmt.Sprintf("IPAddressClaim %s left by a previous machine on this hardware is still being released", claim.Name)

			if err := scope.releaseOrphanedIPAddressClaim(claim); err != nil {
				return err
			}
		}

		scope.setIPAddressClaimedCondition(metav1.ConditionFalse, infrastructurev1.IPAddressClaimDeletingReason, msg)

		return errWaitingForIPAddress
	}

	if claim.Status.AddressRef.Name == "" {
		scope.setIPAddressClaimedCondition(metav1.ConditionFalse, infrastructurev1.WaitingForIPAddressReason,
			fmt.Sprintf("waiting for IPAddressClaim %s to be bound by the IPAM provider", claim.Name))

		return errWaitingForIPAddress
	}

	return scope.applyIPAddress(hw, claim)
}

// ipAddressClaimKey is the deterministic identity of the machine's claim. The Hardware, not
// the machine, is the stable part of the name: the same box keeps the same claim across
// machine replacement, which is what pool-side pinning keys on.
func (scope *machineReconcileScope) ipAddressClaimKey() types.NamespacedName {
	return types.NamespacedName{
		Namespace: scope.tinkerbellMachine.Namespace,
		Name:      scope.tinkerbellMachine.Spec.HardwareName + "-" + scope.tinkerbellMachine.Spec.AddressFromPool.Name,
	}
}

// getIPAddressClaim fetches the machine's claim. A NotFound error is passed through.
func (scope *machineReconcileScope) getIPAddressClaim() (*ipamv1.IPAddressClaim, error) {
	claim := &ipamv1.IPAddressClaim{}
	if err := scope.client.Get(scope.ctx, scope.ipAddressClaimKey(), claim); err != nil {
		return nil, fmt.Errorf("getting IPAddressClaim %s: %w", scope.ipAddressClaimKey(), err)
	}

	return claim, nil
}

// ensureIPAddressClaim returns the machine's claim, creating it when absent. An existing
// claim is only accepted when this machine controls it and it was made for this Hardware;
// anything else is a name collision that must be surfaced rather than adopted.
func (scope *machineReconcileScope) ensureIPAddressClaim(hw *tinkv2.Hardware, mac string, pool *ipamv1.IPPoolReference) (*ipamv1.IPAddressClaim, error) {
	claim, err := scope.getIPAddressClaim()

	switch {
	case apierrors.IsNotFound(err):
		claim = scope.newIPAddressClaim(hw, mac, pool)

		if err := controllerutil.SetControllerReference(scope.tinkerbellMachine, claim, scope.scheme); err != nil {
			return nil, fmt.Errorf("setting IPAddressClaim owner: %w", err)
		}

		if err := scope.client.Create(scope.ctx, claim); err != nil {
			return nil, fmt.Errorf("creating IPAddressClaim %s: %w", claim.Name, err)
		}

		scope.log.Info("created IPAddressClaim", "claim", claim.Name, "pool", pool.Name, "mac", mac)

		return claim, nil
	case err != nil:
		return nil, err
	}

	// A claim on its way out is a wait, whoever owned it; reconcileIPAM reports it.
	if !claim.DeletionTimestamp.IsZero() {
		return claim, nil
	}

	if !metav1.IsControlledBy(claim, scope.tinkerbellMachine) ||
		claim.Annotations[AnnotationHardwareName] != hw.Name ||
		claim.Annotations[AnnotationHardwareNamespace] != hw.Namespace {
		msg := fmt.Sprintf("IPAddressClaim %s exists but is not owned by this machine for hardware %s/%s", claim.Name, hw.Namespace, hw.Name)
		scope.setIPAddressClaimedCondition(metav1.ConditionFalse, infrastructurev1.IPAddressClaimInvalidReason, msg)

		return nil, fmt.Errorf("%w: %s", ErrIPAddressClaimConflict, msg)
	}

	if err := scope.ensureIPAMAnnotations(claim, hw, mac); err != nil {
		return nil, err
	}

	return claim, nil
}

// ensureIPAMAnnotations adds the provider-neutral annotations to a claim made before they
// existed, so an IPAM provider upgrade reaches the node without a new claim.
func (scope *machineReconcileScope) ensureIPAMAnnotations(claim *ipamv1.IPAddressClaim, hw *tinkv2.Hardware, mac string) error {
	want := map[string]string{
		AnnotationIPAMMACAddress: mac,
		AnnotationIPAMHostname:   hw.Name,
	}

	base := claim.DeepCopy()
	changed := false

	for key, value := range want {
		if claim.Annotations[key] == value {
			continue
		}

		if claim.Annotations == nil {
			claim.Annotations = map[string]string{}
		}

		claim.Annotations[key] = value
		changed = true
	}

	if !changed {
		return nil
	}

	if err := scope.client.Patch(scope.ctx, claim, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("annotating IPAddressClaim %s: %w", claim.Name, err)
	}

	return nil
}

func (scope *machineReconcileScope) newIPAddressClaim(hw *tinkv2.Hardware, mac string, pool *ipamv1.IPPoolReference) *ipamv1.IPAddressClaim {
	clusterName := scope.clusterName()
	key := scope.ipAddressClaimKey()

	return &ipamv1.IPAddressClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      key.Name,
			Namespace: key.Namespace,
			Labels: map[string]string{
				clusterv1.ClusterNameLabel: clusterName,
				LabelMachineName:           scope.tinkerbellMachine.Name,
				LabelMachineNamespace:      scope.tinkerbellMachine.Namespace,
			},
			Annotations: map[string]string{
				AnnotationHardwareName:      hw.Name,
				AnnotationHardwareNamespace: hw.Namespace,
				AnnotationMACAddress:        mac,
				AnnotationIPAMMACAddress:    mac,
				AnnotationIPAMHostname:      hw.Name,
			},
			Finalizers: []string{infrastructurev1.IPAddressClaimFinalizer},
		},
		Spec: ipamv1.IPAddressClaimSpec{
			ClusterName: clusterName,
			PoolRef:     *pool,
		},
	}
}

// clusterName is the owning Cluster's name, or empty before the owner Machine is resolved.
func (scope *machineReconcileScope) clusterName() string {
	if scope.machine == nil {
		return ""
	}

	return scope.machine.Spec.ClusterName
}

func (scope *machineReconcileScope) setIPAddressClaimedCondition(status metav1.ConditionStatus, reason, message string) {
	conditions.Set(scope.tinkerbellMachine, metav1.Condition{
		Type:    infrastructurev1.IPAddressClaimedCondition,
		Status:  status,
		Reason:  reason,
		Message: message,
	})
}

// applyIPAddress reads the bound IPAddress and reserves it on the Hardware interface the
// claim was made for.
func (scope *machineReconcileScope) applyIPAddress(hw *tinkv2.Hardware, claim *ipamv1.IPAddressClaim) error {
	address := &ipamv1.IPAddress{}
	key := types.NamespacedName{Namespace: claim.Namespace, Name: claim.Status.AddressRef.Name}

	if err := scope.client.Get(scope.ctx, key, address); err != nil {
		if apierrors.IsNotFound(err) {
			scope.setIPAddressClaimedCondition(metav1.ConditionFalse, infrastructurev1.WaitingForIPAddressReason,
				fmt.Sprintf("IPAddress %s referenced by claim %s does not exist yet", key.Name, claim.Name))

			return errWaitingForIPAddress
		}

		return fmt.Errorf("getting IPAddress %s: %w", key, err)
	}

	ip, err := hardwareAddressFromIPAddress(address)
	if err != nil {
		scope.setIPAddressClaimedCondition(metav1.ConditionFalse, infrastructurev1.IPAddressClaimInvalidReason, err.Error())

		return fmt.Errorf("translating IPAddress %s: %w", key.Name, err)
	}

	mac := claim.Annotations[AnnotationMACAddress]

	if err := scope.ensureHardwareAddress(hw, mac, ip); err != nil {
		if errors.Is(err, ErrHardwareInterfaceNotFound) {
			scope.setIPAddressClaimedCondition(metav1.ConditionFalse, infrastructurev1.IPAddressClaimInvalidReason, err.Error())
		}

		return err
	}

	scope.reservedAddress = ip
	scope.setIPAddressClaimedCondition(metav1.ConditionTrue, infrastructurev1.IPAddressClaimedReason,
		fmt.Sprintf("%s reserved for %s from %s %s", ip.address, mac, claim.Spec.PoolRef.Kind, claim.Spec.PoolRef.Name))

	return nil
}

// ensureHardwareAddress writes ip to the interface carrying mac, unless it already holds it:
// spec.networkInterfaces[mac].ipam.ipv4 (ipv6 for an IPv6 address) gets its address, prefix and
// gateway, and keeps everything else. Skipping the no-op write keeps a steady-state reconcile
// from bumping the Hardware's resourceVersion and creating conflicts for its other writers.
func (scope *machineReconcileScope) ensureHardwareAddress(hw *tinkv2.Hardware, mac string, ip *hardwareAddress) error {
	key, ok := interfaceKey(hw, mac)
	if !ok {
		return fmt.Errorf("%w: %s on hardware %s", ErrHardwareInterfaceNotFound, mac, hw.Name)
	}

	if ip.heldBy(interfaceAddress(hw.Spec.NetworkInterfaces[key], ip.family)) {
		return nil
	}

	base := hw.DeepCopy()

	ni := hw.Spec.NetworkInterfaces[key]
	if ni.IPAM == nil {
		ni.IPAM = &tinkv2.IPAM{}
	}

	current := interfaceAddress(ni, ip.family)
	if current == nil {
		current = &tinkv2.IP{}
	}

	current.Address, current.Prefix, current.Gateway = ip.address, ip.prefix, ip.gateway

	if ip.family == ipv6Family {
		ni.IPAM.IPv6 = current
	} else {
		ni.IPAM.IPv4 = current
	}

	hw.Spec.NetworkInterfaces[key] = ni

	// spec.networkInterfaces is a map, so this merge patch carries only the three fields above.
	// The optimistic lock still turns a concurrent write (the discovery syncer's re-apply, an
	// interface removed meanwhile, which the patch would otherwise recreate) into a Conflict and
	// a retry against the current Hardware.
	if err := scope.tinkerbellClient.Patch(scope.ctx, hw, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		if apierrors.IsConflict(err) {
			return fmt.Errorf("%w: conflict reserving address on hardware %q: %w", errHardwareClaimRequeue, hw.Name, err)
		}

		return fmt.Errorf("patching Hardware IPAM reservation: %w", err)
	}

	scope.log.Info("reserved IPAM address on hardware", "hardware", hw.Name, "mac", mac, "address", ip.address)

	return nil
}

// releaseOrphanedIPAddressClaim takes CAPT's finalizer off a deleting claim whose controlling
// TinkerbellMachine no longer exists. Only that machine releases its claim, on its own deletion,
// and it finds the claim by its current Hardware: one that went away without releasing it (it
// had lost its Hardware to another machine, or its finalizer was removed by hand) would leave
// the claim held by CAPT forever, and every later machine on the Hardware waiting for it. The
// IPAM provider's own finalizer, if any, still runs.
func (scope *machineReconcileScope) releaseOrphanedIPAddressClaim(claim *ipamv1.IPAddressClaim) error {
	if !controllerutil.ContainsFinalizer(claim, infrastructurev1.IPAddressClaimFinalizer) {
		return nil
	}

	gone, err := scope.claimOwnerGone(claim)
	if err != nil || !gone {
		return err
	}

	base := claim.DeepCopy()
	controllerutil.RemoveFinalizer(claim, infrastructurev1.IPAddressClaimFinalizer)

	// Optimistic lock: the provider edits the same finalizer list, and a merge patch replaces
	// it wholesale.
	if err := scope.client.Patch(scope.ctx, claim, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("removing finalizer from IPAddressClaim %s of a removed machine: %w", claim.Name, err)
	}

	scope.log.Info("released IPAddressClaim of a machine that no longer exists", "claim", claim.Name)

	return nil
}

// claimOwnerGone reports whether claim's controlling TinkerbellMachine no longer exists: it is
// not found, or its name now belongs to another machine. A claim controlled by anything else
// is not CAPT's to judge.
func (scope *machineReconcileScope) claimOwnerGone(claim *ipamv1.IPAddressClaim) (bool, error) {
	owner := metav1.GetControllerOf(claim)
	if owner == nil || owner.Kind != "TinkerbellMachine" || !isInfrastructureAPIVersion(owner.APIVersion) {
		return false, nil
	}

	machine := &infrastructurev1.TinkerbellMachine{}

	err := scope.client.Get(scope.ctx, types.NamespacedName{Namespace: claim.Namespace, Name: owner.Name}, machine)
	if apierrors.IsNotFound(err) {
		return true, nil
	}

	if err != nil {
		return false, fmt.Errorf("getting the TinkerbellMachine that owns IPAddressClaim %s: %w", claim.Name, err)
	}

	return machine.UID != owner.UID, nil
}

// isInfrastructureAPIVersion reports whether apiVersion is in CAPT's API group.
func isInfrastructureAPIVersion(apiVersion string) bool {
	gv, err := schema.ParseGroupVersion(apiVersion)

	return err == nil && gv.Group == infrastructurev1.GroupVersion.Group
}

// releaseIPAddressClaim hands the machine's address back: CAPT's finalizer comes off the claim
// and the claim is deleted. The IPAM provider's own finalizer releases the address
// asynchronously; CAPT does not wait for it. A missing claim, or one this machine does not
// control, is not CAPT's to touch.
func (scope *machineReconcileScope) releaseIPAddressClaim() error {
	if scope.tinkerbellMachine.Spec.AddressFromPool == nil || scope.tinkerbellMachine.Spec.HardwareName == "" {
		return nil
	}

	claim, err := scope.getIPAddressClaim()
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}

		return err
	}

	if !metav1.IsControlledBy(claim, scope.tinkerbellMachine) {
		scope.log.Info("IPAddressClaim is not controlled by this machine; leaving it in place", "claim", claim.Name)

		return nil
	}

	if controllerutil.ContainsFinalizer(claim, infrastructurev1.IPAddressClaimFinalizer) {
		base := claim.DeepCopy()
		controllerutil.RemoveFinalizer(claim, infrastructurev1.IPAddressClaimFinalizer)

		// Optimistic lock: the provider edits the same finalizer list, and a merge patch
		// replaces it wholesale.
		if err := scope.client.Patch(scope.ctx, claim, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return fmt.Errorf("removing finalizer from IPAddressClaim %s: %w", claim.Name, err)
		}
	}

	if err := scope.client.Delete(scope.ctx, claim); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting IPAddressClaim %s: %w", claim.Name, err)
	}

	scope.log.Info("released IPAddressClaim", "claim", claim.Name)

	return nil
}

// ipamReservation returns where the machine's reservation was written: the MAC recorded on the
// claim, or the primary interface's when the claim is already gone, and the IP family of the
// claim's address, or 0 when it is not known. An empty MAC means there is nothing to clear.
func (scope *machineReconcileScope) ipamReservation(hw *tinkv2.Hardware) (string, int, error) {
	claim, err := scope.getIPAddressClaim()

	switch {
	case err == nil:
		// Only a claim this machine made says where its reservation went.
		if mac := claim.Annotations[AnnotationMACAddress]; mac != "" && metav1.IsControlledBy(claim, scope.tinkerbellMachine) {
			family, err := scope.claimedFamily(claim)

			return mac, family, err
		}
	case !apierrors.IsNotFound(err):
		return "", 0, err
	}

	mac, err := primaryInterfaceMAC(hw)
	if err != nil {
		// No interface or no MAC means CAPT never reserved anything here.
		return "", 0, nil //nolint:nilerr // the absence is the answer, not a failure
	}

	return mac, 0, nil
}

// claimedFamily returns the IP family of the address bound to claim, or 0 when the claim is
// not bound or its IPAddress is gone.
func (scope *machineReconcileScope) claimedFamily(claim *ipamv1.IPAddressClaim) (int, error) {
	if claim.Status.AddressRef.Name == "" {
		return 0, nil
	}

	address := &ipamv1.IPAddress{}
	key := types.NamespacedName{Namespace: claim.Namespace, Name: claim.Status.AddressRef.Name}

	if err := scope.client.Get(scope.ctx, key, address); err != nil {
		if apierrors.IsNotFound(err) {
			return 0, nil
		}

		return 0, fmt.Errorf("getting IPAddress %s: %w", key, err)
	}

	parsed, err := netip.ParseAddr(address.Spec.Address)
	if err != nil {
		return 0, nil //nolint:nilerr // an address CAPT could not parse is one it never wrote
	}

	if parsed.Unmap().Is4() {
		return ipv4Family, nil
	}

	return ipv6Family, nil
}

// clearHardwareAddress drops the reservation on the interface carrying mac: the address, prefix
// and gateway of its IP configuration of family, or of both families when family is 0 (a
// v1alpha1 interface held a single address, so that is what clearing it meant). The rest of the
// IP configuration is not CAPT's and stays; a configuration left empty is removed.
func clearHardwareAddress(hw *tinkv2.Hardware, mac string, family int) {
	key, ok := interfaceKey(hw, mac)
	if !ok {
		return
	}

	ni := hw.Spec.NetworkInterfaces[key]
	if ni.IPAM == nil {
		return
	}

	if family != ipv6Family {
		ni.IPAM.IPv4 = withoutAddress(ni.IPAM.IPv4)
	}

	if family != ipv4Family {
		ni.IPAM.IPv6 = withoutAddress(ni.IPAM.IPv6)
	}

	if ni.IPAM.IPv4 == nil && ni.IPAM.IPv6 == nil {
		ni.IPAM = nil
	}

	hw.Spec.NetworkInterfaces[key] = ni
}

// withoutAddress returns ip without the fields CAPT writes, or nil when nothing else is set.
func withoutAddress(ip *tinkv2.IP) *tinkv2.IP {
	if ip == nil {
		return nil
	}

	rest := *ip
	rest.Address, rest.Prefix, rest.Gateway = "", "", ""

	if reflect.DeepEqual(rest, tinkv2.IP{}) {
		return nil
	}

	return &rest
}
