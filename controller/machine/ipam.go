package machine

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"

	tinkv1 "github.com/tinkerbell/tinkerbell/api/v1alpha1/tinkerbell"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

	ipv4Bits   = 32
	ipv4Family = 4
	ipv6Family = 6
)

var (
	// errWaitingForIPAddress signals that the IPAM provider has not answered the claim yet.
	errWaitingForIPAddress = errors.New("waiting for IP address allocation")
	// ErrHardwareInterfaceMissingMAC is returned when the primary interface has no MAC to
	// reserve an address for.
	ErrHardwareInterfaceMissingMAC = errors.New("hardware's first interface has no DHCP MAC address")
	// ErrIPAddressMissingPrefix is returned when an IPv4 IPAddress carries no prefix; smee
	// refuses an IPv4 reservation without a netmask.
	ErrIPAddressMissingPrefix = errors.New("IPAddress has no prefix")
	// ErrHardwareInterfaceNotFound is returned when no Hardware interface carries the MAC the
	// claim was made for.
	ErrHardwareInterfaceNotFound = errors.New("no hardware interface matches the claim's MAC address")
	// ErrIPAddressClaimConflict is returned when the claim CAPT would use belongs to a
	// different machine or was made for a different Hardware.
	ErrIPAddressClaimConflict = errors.New("IPAddressClaim belongs to a different machine or hardware")
)

// primaryInterfaceMAC returns the lowercased MAC of the interface CAPT reserves addresses
// for: the first one, the same interface hardwareIP reports the node address from.
func primaryInterfaceMAC(hw *tinkv1.Hardware) (string, error) {
	if hw == nil {
		return "", ErrHardwareIsNil
	}

	if len(hw.Spec.Interfaces) == 0 {
		return "", ErrHardwareMissingInterfaces
	}

	dhcp := hw.Spec.Interfaces[0].DHCP
	if dhcp == nil || dhcp.MAC == "" {
		return "", ErrHardwareInterfaceMissingMAC
	}

	return strings.ToLower(dhcp.MAC), nil
}

// interfaceIndexByMAC returns the index of the interface whose DHCP MAC equals mac, or -1.
func interfaceIndexByMAC(hw *tinkv1.Hardware, mac string) int {
	for i := range hw.Spec.Interfaces {
		dhcp := hw.Spec.Interfaces[i].DHCP
		if dhcp != nil && dhcp.MAC != "" && strings.EqualFold(dhcp.MAC, mac) {
			return i
		}
	}

	return -1
}

// prefixToNetmask renders an IPv4 prefix length as the dotted-decimal netmask smee expects.
func prefixToNetmask(prefix int) (string, error) {
	if prefix < 0 || prefix > ipv4Bits {
		return "", fmt.Errorf("prefix /%d is not a valid IPv4 prefix length", prefix)
	}

	return net.IP(net.CIDRMask(prefix, ipv4Bits)).String(), nil
}

// hardwareIPFromIPAddress translates an allocated IPAddress into the reservation Tinkerbell
// serves: IPv4 needs a dotted netmask (smee rejects a reservation without one), IPv6 must not
// carry one.
func hardwareIPFromIPAddress(addr *ipamv1.IPAddress) (*tinkv1.IP, error) {
	parsed, err := netip.ParseAddr(addr.Spec.Address)
	if err != nil {
		return nil, fmt.Errorf("parsing IPAddress %q: %w", addr.Name, err)
	}

	ip := &tinkv1.IP{Address: addr.Spec.Address, Gateway: addr.Spec.Gateway}

	if !parsed.Unmap().Is4() {
		ip.Family = ipv6Family

		return ip, nil
	}

	if addr.Spec.Prefix == nil {
		return nil, fmt.Errorf("%w: %q", ErrIPAddressMissingPrefix, addr.Name)
	}

	netmask, err := prefixToNetmask(int(*addr.Spec.Prefix))
	if err != nil {
		return nil, fmt.Errorf("IPAddress %q: %w", addr.Name, err)
	}

	ip.Netmask = netmask
	ip.Family = ipv4Family

	return ip, nil
}

// reconcileIPAM claims an address from spec.addressFromPool for the Hardware's primary
// interface and reserves it on the Hardware once the IPAM provider has answered.
//
// The write-back locates the interface by the MAC recorded on the claim, not by position,
// so a reordered interface list cannot direct the address at the wrong NIC.
func (scope *machineReconcileScope) reconcileIPAM(hw *tinkv1.Hardware) error {
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
func (scope *machineReconcileScope) ensureIPAddressClaim(hw *tinkv1.Hardware, mac string, pool *ipamv1.IPPoolReference) (*ipamv1.IPAddressClaim, error) {
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

	return claim, nil
}

func (scope *machineReconcileScope) newIPAddressClaim(hw *tinkv1.Hardware, mac string, pool *ipamv1.IPPoolReference) *ipamv1.IPAddressClaim {
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
func (scope *machineReconcileScope) applyIPAddress(hw *tinkv1.Hardware, claim *ipamv1.IPAddressClaim) error {
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

	ip, err := hardwareIPFromIPAddress(address)
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
		fmt.Sprintf("%s reserved for %s from %s %s", ip.Address, mac, claim.Spec.PoolRef.Kind, claim.Spec.PoolRef.Name))

	return nil
}

// ensureHardwareAddress writes ip to the interface carrying mac, unless it already holds it.
// Skipping the no-op write keeps a steady-state reconcile from bumping the Hardware's
// resourceVersion and creating conflicts for its other writers.
func (scope *machineReconcileScope) ensureHardwareAddress(hw *tinkv1.Hardware, mac string, ip *tinkv1.IP) error {
	idx := interfaceIndexByMAC(hw, mac)
	if idx < 0 {
		return fmt.Errorf("%w: %s on hardware %s", ErrHardwareInterfaceNotFound, mac, hw.Name)
	}

	dhcp := hw.Spec.Interfaces[idx].DHCP
	if dhcp.IP != nil && *dhcp.IP == *ip {
		return nil
	}

	base := hw.DeepCopy()
	dhcp.IP = ip.DeepCopy()

	// spec.interfaces is an atomic list, so this merge patch carries the whole list. The
	// optimistic lock turns a concurrent interfaces write (the discovery syncer's re-apply)
	// into a Conflict and a retry instead of a silent revert to this stale copy.
	if err := scope.tinkerbellClient.Patch(scope.ctx, hw, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		if apierrors.IsConflict(err) {
			return fmt.Errorf("%w: conflict reserving address on hardware %q: %w", errHardwareClaimRequeue, hw.Name, err)
		}

		return fmt.Errorf("patching Hardware DHCP reservation: %w", err)
	}

	scope.log.Info("reserved IPAM address on hardware", "hardware", hw.Name, "mac", mac, "address", ip.Address)

	return nil
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

// ipamInterfaceMAC returns the MAC the machine's reservation was written under: the one
// recorded on the claim, or the primary interface's when the claim is already gone. Empty
// means there is nothing to clear.
func (scope *machineReconcileScope) ipamInterfaceMAC(hw *tinkv1.Hardware) (string, error) {
	claim, err := scope.getIPAddressClaim()

	switch {
	case err == nil:
		// Only a claim this machine made says where its reservation went.
		if mac := claim.Annotations[AnnotationMACAddress]; mac != "" && metav1.IsControlledBy(claim, scope.tinkerbellMachine) {
			return mac, nil
		}
	case !apierrors.IsNotFound(err):
		return "", err
	}

	mac, err := primaryInterfaceMAC(hw)
	if err != nil {
		// No interface or no MAC means CAPT never reserved anything here.
		return "", nil //nolint:nilerr // the absence is the answer, not a failure
	}

	return mac, nil
}

// clearHardwareAddress drops the DHCP reservation on the interface carrying mac.
func clearHardwareAddress(hw *tinkv1.Hardware, mac string) {
	if idx := interfaceIndexByMAC(hw, mac); idx >= 0 {
		hw.Spec.Interfaces[idx].DHCP.IP = nil
	}
}
