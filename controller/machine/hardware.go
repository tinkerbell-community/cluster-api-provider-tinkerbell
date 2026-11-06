package machine

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"

	"github.com/tinkerbell/tinkerbell/api/conversion"
	tinkv1 "github.com/tinkerbell/tinkerbell/api/v1alpha1/tinkerbell"
	tinkv2 "github.com/tinkerbell/tinkerbell/api/v1alpha2/tinkerbell"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	infrastructurev1 "github.com/tinkerbell/cluster-api-provider-tinkerbell/api/v1beta2"
)

const (
	// ProviderIDScheme is the URI scheme used in TinkerbellMachine.Spec.ProviderID.
	ProviderIDScheme = "tinkerbell"

	// HardwareOwnerNameLabel is a label set by either CAPT controllers or Tinkerbell controller to indicate
	// that given hardware takes part of at least one workflow.
	HardwareOwnerNameLabel = "v1alpha1.tinkerbell.org/ownerName"

	// HardwareOwnerNamespaceLabel is a label set by either CAPT controllers or Tinkerbell controller to indicate
	// that given hardware takes part of at least one workflow.
	HardwareOwnerNamespaceLabel = "v1alpha1.tinkerbell.org/ownerNamespace"

	// HardwareProvisionedAnnotation signifies that the Hardware with this annotation has be provisioned by CAPT.
	HardwareProvisionedAnnotation = "v1alpha1.tinkerbell.org/provisioned"

	// HardwareTemplateOverrideAnnotation can be used to override the default Template used for provisioning.
	HardwareTemplateOverrideAnnotation = "hardware.tinkerbell.org/capt-template-override"
)

var (
	// ErrNoHardwareAvailable is the error returned when there is no hardware available for provisioning.
	ErrNoHardwareAvailable = fmt.Errorf("no hardware available")
	// ErrHardwareIsNil is the error returned when the given hardware resource is nil.
	ErrHardwareIsNil = fmt.Errorf("given Hardware object is nil")
	// ErrHardwareMissingInterfaces is the error returned when the referenced hardware does not have any
	// network interfaces defined.
	ErrHardwareMissingInterfaces = fmt.Errorf("hardware has no interfaces defined")
	// ErrHardwareFirstInterfaceNotDHCP is the error returned when the referenced hardware's primary
	// network interface (see primaryInterfaceMAC) has no IPAM configuration.
	ErrHardwareFirstInterfaceNotDHCP = fmt.Errorf("hardware's primary interface has no IPAM configuration defined")
	// ErrHardwareFirstInterfaceDHCPMissingIP is the error returned when the referenced hardware's primary
	// network interface has no IP address in its IPAM configuration.
	ErrHardwareFirstInterfaceDHCPMissingIP = fmt.Errorf("hardware's primary interface has no IP address defined")
	// ErrHardwareMissingDiskConfiguration is returned when the referenced hardware has no storage
	// devices (spec.storageDevices).
	ErrHardwareMissingDiskConfiguration = fmt.Errorf("disk configuration is required")
	// errHardwareClaimRequeue marks expected claim contention that should be retried
	// without surfacing a reconciliation error.
	errHardwareClaimRequeue = errors.New("hardware claim requires requeue")
)

// hardwareIP returns the IP address of the primary network interface of the given hardware (see
// primaryInterface): its IPv4 address, or its IPv6 address when it has no IPv4 one. A v1alpha1
// interface held a single address of either family, which converts to one of the two.
func hardwareIP(hardware *tinkv2.Hardware) (string, error) {
	if hardware == nil {
		return "", ErrHardwareIsNil
	}

	mac, err := primaryInterface(hardware)
	if err != nil {
		return "", err
	}

	ipam := hardware.Spec.NetworkInterfaces[mac].IPAM
	if ipam == nil || (ipam.IPv4 == nil && ipam.IPv6 == nil) {
		return "", ErrHardwareFirstInterfaceNotDHCP
	}

	for _, ip := range []*tinkv2.IP{ipam.IPv4, ipam.IPv6} {
		if ip != nil && ip.Address != "" {
			return ip.Address, nil
		}
	}

	return "", ErrHardwareFirstInterfaceDHCPMissingIP
}

// primaryInterface returns the key of the network interface CAPT treats as the Hardware's primary
// one: the interface it reports the node address from and reserves IPAM addresses for.
//
// v1alpha1 listed interfaces and CAPT used the first; v1alpha2 keys them by MAC, so they have no
// order. The primary interface is, in order of preference:
//   - for Hardware created through v1alpha1, its first v1alpha1 interface that has a MAC: the
//     interface CAPT has always used. The v1alpha1 order survives in the conversion annotation.
//   - the interface spec.agentID names; an Agent's ID is typically its primary interface's MAC.
//   - the interface with the lowest MAC, the order Tinkerbell itself walks interfaces in.
func primaryInterface(hw *tinkv2.Hardware) (tinkv2.MAC, error) {
	if hw == nil {
		return "", ErrHardwareIsNil
	}

	if len(hw.Spec.NetworkInterfaces) == 0 {
		return "", ErrHardwareMissingInterfaces
	}

	if mac, ok := v1alpha1PrimaryInterface(hw); ok {
		return mac, nil
	}

	if mac, ok := interfaceKey(hw, hw.Spec.AgentID); ok {
		return mac, nil
	}

	return slices.Sorted(maps.Keys(hw.Spec.NetworkInterfaces))[0], nil
}

// v1alpha1PrimaryInterface returns the key of the network interface that is the first interface
// with a MAC in hw's v1alpha1 form, when hw was created through v1alpha1.
func v1alpha1PrimaryInterface(hw *tinkv2.Hardware) (tinkv2.MAC, bool) {
	if _, ok := hw.Annotations[conversion.AnnotationV1Alpha1]; !ok {
		return "", false
	}

	v1 := &tinkv1.Hardware{}
	if err := conversion.HardwareToV1Alpha1(hw, v1); err != nil {
		return "", false
	}

	for _, iface := range v1.Spec.Interfaces {
		if iface.DHCP == nil {
			continue
		}

		if mac, ok := interfaceKey(hw, iface.DHCP.MAC); ok {
			return mac, true
		}
	}

	return "", false
}

// interfaceKey returns the key of hw's network interface whose MAC is mac. MACs are compared
// case-insensitively: v1alpha2 keys are lower case, MACs recorded elsewhere need not be.
func interfaceKey(hw *tinkv2.Hardware, mac string) (tinkv2.MAC, bool) {
	if mac == "" {
		return "", false
	}

	if _, ok := hw.Spec.NetworkInterfaces[tinkv2.MAC(mac)]; ok {
		return tinkv2.MAC(mac), true
	}

	for key := range hw.Spec.NetworkInterfaces {
		if strings.EqualFold(string(key), mac) {
			return key, true
		}
	}

	return "", false
}

// hardwareLookupID is the identifier CAPT hands a Workflow as device_1, the Agent a template's
// tasks run on, and puts in per-machine ISO URLs: spec.instance.lookupID, where v1alpha1's
// metadata.instance.id converts to. Hardware without a lookupID falls back to its spec.agentID,
// the Agent's ID in v1alpha2.
func hardwareLookupID(hw *tinkv2.Hardware) string {
	var lookupID string
	if hw.Spec.Instance != nil {
		lookupID = hw.Spec.Instance.LookupID
	}

	return cmp.Or(lookupID, hw.Spec.AgentID)
}

// hasBMC reports whether Rufio can reach hw's BMC from a Job that references hw: hw has its own
// spec.bmc.connection, or it was created through v1alpha1 with a bmcRef naming a deprecated bmc
// Machine, which Rufio still resolves through the Hardware.
func hasBMC(hw *tinkv2.Hardware) bool {
	if hw == nil {
		return false
	}

	if hw.Spec.BMC != nil && hw.Spec.BMC.Connection != nil {
		return true
	}

	ref := conversion.HardwareBMCRef(hw)

	return ref != nil && ref.Name != ""
}

// hardwareUserData returns hw's spec.instance.userdata, or nil when it is not set.
func hardwareUserData(hw *tinkv2.Hardware) *string {
	if hw.Spec.Instance == nil {
		return nil
	}

	return hw.Spec.Instance.Userdata
}

// patchHardwareAnnotations adds annotations to the Hardware. The merge patch carries only them, so
// nothing else on the Hardware, such as the conversion annotation of Hardware created through
// v1alpha1, is touched.
func (scope *machineReconcileScope) patchHardwareAnnotations(hw *tinkv2.Hardware, annotations map[string]string) error {
	base := hw.DeepCopy()

	if hw.Annotations == nil {
		hw.Annotations = map[string]string{}
	}

	maps.Copy(hw.Annotations, annotations)

	if err := scope.tinkerbellClient.Patch(scope.ctx, hw, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("patching Hardware object: %w", err)
	}

	return nil
}

// hardwareAlreadyClaimed reports whether hw already carries this Machine's ownership
// labels and the expected finalizer state, i.e. a claim would have nothing to write.
func (scope *machineReconcileScope) hardwareAlreadyClaimed(hw *tinkv2.Hardware) bool {
	return hw.Labels[HardwareOwnerNameLabel] == scope.tinkerbellMachine.Name &&
		hw.Labels[HardwareOwnerNamespaceLabel] == scope.tinkerbellMachine.Namespace &&
		controllerutil.ContainsFinalizer(hw, infrastructurev1.MachineFinalizer) &&
		!controllerutil.ContainsFinalizer(hw, infrastructurev1.MachineLegacyFinalizer)
}

func (scope *machineReconcileScope) takeHardwareOwnership(hw *tinkv2.Hardware) error {
	// hardwareForMachine() returns already-assigned Hardware for this Machine first,
	// otherwise lists unowned Hardware and returns the first element after a
	// deterministic sort — so two Machines reconciling concurrently can still select
	// the SAME unowned Hardware. Guard against claiming one already owned by a
	// different Machine: back off and requeue so the next pass re-lists (this Hardware
	// is now filtered out by the DoesNotExist owner selector) and picks a still-free one.
	if owner, ok := hw.Labels[HardwareOwnerNameLabel]; ok &&
		(owner != scope.tinkerbellMachine.Name ||
			hw.Labels[HardwareOwnerNamespaceLabel] != scope.tinkerbellMachine.Namespace) {
		return fmt.Errorf("%w: hardware %q already owned by %s/%s",
			errHardwareClaimRequeue,
			hw.Name, hw.Labels[HardwareOwnerNamespaceLabel], owner)
	}

	// Already claimed by this Machine with the expected finalizer state: nothing to
	// write. Skipping the no-op patch avoids bumping resourceVersion on every
	// reconcile and the avoidable conflicts that causes for other writers.
	if scope.hardwareAlreadyClaimed(hw) {
		return nil
	}
	base := hw.DeepCopy()

	if hw.Labels == nil {
		hw.Labels = map[string]string{}
	}

	hw.Labels[HardwareOwnerNameLabel] = scope.tinkerbellMachine.Name
	hw.Labels[HardwareOwnerNamespaceLabel] = scope.tinkerbellMachine.Namespace

	// Add finalizer to hardware as well to make sure we release it before Machine object is removed.
	// Remove the legacy name first so upgraded Hardware objects don't carry both.
	controllerutil.RemoveFinalizer(hw, infrastructurev1.MachineLegacyFinalizer)
	controllerutil.AddFinalizer(hw, infrastructurev1.MachineFinalizer)

	// Claim with an optimistic-lock merge patch. This keeps the write limited to
	// ownership labels and finalizers while carrying the ResourceVersion read by
	// hardwareForMachine. A losing concurrent claim gets a Conflict instead of
	// silently overwriting the winner's ownership.
	claimPatch := client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})
	if err := scope.tinkerbellClient.Patch(scope.ctx, hw, claimPatch); err != nil {
		if apierrors.IsConflict(err) {
			return fmt.Errorf("%w: conflict claiming hardware %q: %w", errHardwareClaimRequeue, hw.Name, err)
		}

		return fmt.Errorf("claiming Hardware ownership: %w", err)
	}

	return nil
}

func (scope *machineReconcileScope) ensureHardwareUserData(hw *tinkv2.Hardware, providerID string) error {
	userData := strings.ReplaceAll(scope.bootstrapCloudConfig, providerIDPlaceholder, providerID)

	if current := hardwareUserData(hw); current != nil && *current == userData {
		return nil
	}

	base := hw.DeepCopy()

	if hw.Spec.Instance == nil {
		hw.Spec.Instance = &tinkv2.Instance{}
	}

	hw.Spec.Instance.Userdata = &userData

	// The merge patch carries spec.instance.userdata alone; the rest of spec.instance is not CAPT's.
	if err := scope.tinkerbellClient.Patch(scope.ctx, hw, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("patching Hardware object: %w", err)
	}

	return nil
}

func (scope *machineReconcileScope) ensureHardware() (*tinkv2.Hardware, error) {
	hw, err := scope.hardwareForMachine()
	if err != nil {
		return nil, fmt.Errorf("getting hardware: %w", err)
	}

	if err := scope.takeHardwareOwnership(hw); err != nil {
		return nil, fmt.Errorf("taking Hardware ownership: %w", err)
	}

	if scope.tinkerbellMachine.Spec.HardwareName == "" {
		scope.log.Info("Selected Hardware for machine", "Hardware name", hw.Name)
	}

	scope.tinkerbellMachine.Spec.HardwareName = hw.Name
	scope.tinkerbellMachine.Spec.ProviderID = fmt.Sprintf("%s://%s/%s", ProviderIDScheme, hw.Namespace, hw.Name)

	// Persist the hardware's namespace as the target namespace for all
	// Tinkerbell resources. The Hardware namespace is authoritative because
	// Workflow.Spec.HardwareRef is a name-only reference resolved within the
	// Workflow's own namespace. Persisting it ensures deletion works even if
	// the Hardware object is already gone.
	if scope.tinkerbellMachine.Status.TargetNamespace == "" {
		scope.tinkerbellMachine.Status.TargetNamespace = hw.Namespace
	}

	// In JIT watch mode, ensure there is a namespace-scoped informer cache
	// watching Workflows, Tasks and Jobs in the Hardware's namespace so that the
	// controller receives events for resources created there.
	if scope.watchManager != nil {
		if err := scope.watchManager.EnsureWatch(scope.ctx, hw.Namespace); err != nil {
			return nil, fmt.Errorf("ensuring namespace watch for %q: %w", hw.Namespace, err)
		}
	}

	if err := scope.ensureHardwareUserData(hw, scope.tinkerbellMachine.Spec.ProviderID); err != nil {
		return nil, fmt.Errorf("ensuring Hardware user data: %w", err)
	}

	return hw, nil
}

func (scope *machineReconcileScope) hardwareForMachine() (*tinkv2.Hardware, error) {
	// first query for hardware that's already assigned
	if hardware, err := scope.assignedHardware(); err != nil {
		return nil, err
	} else if hardware != nil {
		return hardware, nil
	}

	// then fallback to searching for new hardware
	hardwareSelector := scope.tinkerbellMachine.Spec.HardwareAffinity.DeepCopy()
	if hardwareSelector == nil {
		hardwareSelector = &infrastructurev1.HardwareAffinity{}
	}
	// if no terms are specified, we create an empty one to ensure we always query for non-selected hardware
	if len(hardwareSelector.Required) == 0 {
		hardwareSelector.Required = append(hardwareSelector.Required, infrastructurev1.HardwareAffinityTerm{})
	}

	var matchingHardware []tinkv2.Hardware

	// OR all of the required terms by selecting each individually, we could end up with duplicates in matchingHardware
	// but it doesn't matter
	for i := range hardwareSelector.Required {
		var matched tinkv2.HardwareList

		// add a selector for unselected hardware
		hardwareSelector.Required[i].LabelSelector.MatchExpressions = append(
			hardwareSelector.Required[i].LabelSelector.MatchExpressions,
			metav1.LabelSelectorRequirement{
				Key:      HardwareOwnerNameLabel,
				Operator: metav1.LabelSelectorOpDoesNotExist,
			})

		selector, err := metav1.LabelSelectorAsSelector(&hardwareSelector.Required[i].LabelSelector)
		if err != nil {
			return nil, fmt.Errorf("converting label selector: %w", err)
		}

		if err := scope.tinkerbellClient.List(scope.ctx, &matched, &client.ListOptions{LabelSelector: selector}); err != nil {
			return nil, fmt.Errorf("listing hardware without owner: %w", err)
		}

		matchingHardware = append(matchingHardware, matched.Items...)
	}

	// finally sort by our preferred affinity terms
	cmp, err := byHardwareAffinity(matchingHardware, hardwareSelector.Preferred)
	if err != nil {
		return nil, fmt.Errorf("sorting hardware by preference: %w", err)
	}

	sort.Slice(matchingHardware, cmp)

	if len(matchingHardware) > 0 {
		return &matchingHardware[0], nil
	}
	// nothing was found
	return nil, ErrNoHardwareAvailable
}

// assignedHardware returns hardware that is already assigned. In the event of no hardware being assigned, it returns
// nil, nil.
func (scope *machineReconcileScope) assignedHardware() (*tinkv2.Hardware, error) {
	listOpts := []client.ListOption{
		client.MatchingLabels{
			HardwareOwnerNameLabel:      scope.tinkerbellMachine.Name,
			HardwareOwnerNamespaceLabel: scope.tinkerbellMachine.Namespace,
		},
	}

	var selectedHardware tinkv2.HardwareList
	if err := scope.tinkerbellClient.List(scope.ctx, &selectedHardware, listOpts...); err != nil {
		return nil, fmt.Errorf("listing hardware with owner: %w", err)
	}

	if len(selectedHardware.Items) > 0 {
		return &selectedHardware.Items[0], nil
	}

	return nil, nil
}

func byHardwareAffinity(hardware []tinkv2.Hardware, preferred []infrastructurev1.WeightedHardwareAffinityTerm) (func(i int, j int) bool, error) {
	scores := map[client.ObjectKey]int32{}
	// compute scores for each item based on the preferred term weights
	for _, term := range preferred {
		selector, err := metav1.LabelSelectorAsSelector(&term.HardwareAffinityTerm.LabelSelector)
		if err != nil {
			return nil, fmt.Errorf("constructing label selector: %w", err)
		}

		for i := range hardware {
			hw := &hardware[i]
			if selector.Matches(labels.Set(hw.Labels)) {
				scores[client.ObjectKeyFromObject(hw)] = term.Weight
			}
		}
	}

	return func(i, j int) bool {
		lhsScore := scores[client.ObjectKeyFromObject(&hardware[i])]
		rhsScore := scores[client.ObjectKeyFromObject(&hardware[j])]
		// sort by score in descending order
		if lhsScore > rhsScore {
			return true
		} else if lhsScore < rhsScore {
			return false
		}

		// just give a consistent ordering so we predictably pick one if scores are equal
		if hardware[i].Namespace != hardware[j].Namespace {
			return hardware[i].Namespace < hardware[j].Namespace
		}

		return hardware[i].Name < hardware[j].Name
	}, nil
}

func (scope *machineReconcileScope) releaseHardware(hw *tinkv2.Hardware) error {
	base := hw.DeepCopy()

	delete(hw.Labels, HardwareOwnerNameLabel)
	delete(hw.Labels, HardwareOwnerNamespaceLabel)
	delete(hw.Annotations, HardwareProvisionedAnnotation)

	// CAPT is the sole writer of the user data; clearing it in the same patch that
	// releases the Hardware keeps the machine configuration (cluster PKI,
	// bootstrap token) from being served by Tootles to whatever boots this
	// hardware next.
	if hw.Spec.Instance != nil {
		hw.Spec.Instance.Userdata = nil
	}

	// The same holds for the DHCP reservation written from IPAM: the claim is released right
	// after this, so the address must not stay reserved for this MAC.
	if scope.tinkerbellMachine.Spec.AddressFromPool != nil {
		mac, family, err := scope.ipamReservation(hw)
		if err != nil {
			return fmt.Errorf("resolving IPAM interface to clear: %w", err)
		}

		clearHardwareAddress(hw, mac, family)
	}

	controllerutil.RemoveFinalizer(hw, infrastructurev1.MachineFinalizer)
	controllerutil.RemoveFinalizer(hw, infrastructurev1.MachineLegacyFinalizer)

	// The merge patch carries only what changed above, but a merge patch replaces a list such as
	// the finalizers wholesale, and would recreate an interface removed meanwhile. The optimistic
	// lock turns such a concurrent write into a Conflict instead of a revert to this copy.
	releasePatch := client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})
	if err := scope.tinkerbellClient.Patch(scope.ctx, hw, releasePatch); err != nil {
		return fmt.Errorf("patching Hardware object: %w", err)
	}

	return nil
}

func (scope *machineReconcileScope) getHardwareForMachine(hardware *tinkv2.Hardware) error {
	namespacedName := types.NamespacedName{
		Name:      scope.tinkerbellMachine.Spec.HardwareName,
		Namespace: scope.tinkerbellNamespace(),
	}

	if err := scope.tinkerbellClient.Get(scope.ctx, namespacedName, hardware); err != nil {
		return fmt.Errorf("getting hardware: %w", err)
	}

	return nil
}
