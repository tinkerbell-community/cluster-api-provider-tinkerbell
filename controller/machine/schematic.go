package machine

import (
	"fmt"
	"regexp"

	tinkv1 "github.com/tinkerbell/tinkerbell/api/v1alpha1/tinkerbell"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"github.com/tinkerbell/cluster-api-provider-tinkerbell/pkg/schematic"
)

// fullTalosVersion matches a complete Talos version such as v1.14.0 or v1.14.0-rc.1.
//
// A bare minor like v1.13 is a config contract, not an OS version, and cannot be used to pull
// an image, so it is deliberately not accepted.
var fullTalosVersion = regexp.MustCompile(`^v\d+\.\d+\.\d+(-[0-9A-Za-z.\-]+)?$`)

// reconcileSchematic resolves the machine's Image Factory schematic and records the resulting
// image references on status.
//
// The same schematic backs both the disk image the Workflow writes during provisioning and
// the installer image Talos upgrades to later, so installing and upgrading a machine agree on
// which system extensions it has.
//
// Resolution is skipped rather than guessed when the Talos version is unknown: pulling the
// wrong OS version is worse than leaving the field empty and letting the template's own
// default apply.
func (scope *machineReconcileScope) reconcileSchematic(hw *tinkv1.Hardware) error {
	if scope.schematicRegistrar == nil {
		return nil
	}

	talosVersion := scope.resolveTalosVersion(hardwareProvisioned(hw))
	if talosVersion == "" {
		scope.log.V(1).Info("no Talos version resolved, skipping schematic resolution",
			"machine", scope.tinkerbellMachine.Name)

		return nil
	}

	signals := schematic.SignalsFromHardware(hw, parseMachineExtensions(scope.tinkerbellMachine.GetAnnotations()))

	id, err := scope.schematicRegistrar.Register(scope.ctx, schematic.Build(signals))
	if err != nil {
		return fmt.Errorf("resolving Image Factory schematic: %w", err)
	}

	installer := schematic.InstallerImage(scope.factoryURL, id, talosVersion)
	diskImage := schematic.DiskImageURL(scope.factoryURL, id, talosVersion, signals.Architecture)

	if scope.tinkerbellMachine.Status.SchematicID == id &&
		scope.tinkerbellMachine.Status.InstallerImage == installer &&
		scope.tinkerbellMachine.Status.DiskImageURL == diskImage {
		return nil
	}

	scope.log.Info("resolved Image Factory schematic",
		"schematicID", id, "installerImage", installer, "extensions", signals.ExtraExtensions)

	scope.tinkerbellMachine.Status.SchematicID = id
	scope.tinkerbellMachine.Status.InstallerImage = installer
	scope.tinkerbellMachine.Status.DiskImageURL = diskImage

	return nil
}

// resolveTalosVersion resolves the concrete Talos OS version the machine installs and upgrades to.
//
//   - A fully pinned spec version (v1.13.9) is used exactly, never bumped: an explicit pin is a
//     deliberate choice to avoid surprise upgrades.
//   - A bare minor (v1.13) tracks the newest GA patch in that minor, so a new patch drives an
//     in-place upgrade while a minor stays fixed.
//   - An unset or "latest" spec pins the newest GA minor to the machine (see contractMinor) and
//     tracks its newest patch thereafter.
//
// An empty result means "not knowable", which callers treat as "do not resolve": pulling the
// wrong OS version is worse than leaving the field for the template's own default.
func (scope *machineReconcileScope) resolveTalosVersion(provisioned bool) string {
	raw := scope.specTalosVersion()

	if fullTalosVersion.MatchString(raw) {
		return raw
	}

	// Anything but a full pin needs the factory to turn a minor or "latest" into a real patch.
	if scope.versionResolver == nil {
		return ""
	}

	floor := raw
	if floor == "" || floor == "latest" {
		floor = scope.contractMinor(provisioned)
	}

	resolved, err := scope.versionResolver.LatestPatch(scope.ctx, floor)
	if err != nil {
		scope.log.V(1).Info("could not resolve latest Talos patch, skipping schematic resolution",
			"floor", floor, "error", err.Error())

		return ""
	}

	return resolved
}

// contractMinor returns the Talos minor line an unset machine tracks.
//
// The newest GA minor is pinned to the machine on first resolution so it does not follow new
// minors as they are released; crossing a minor then requires deliberately setting the version.
// An empty result means the newest minor could not be determined yet.
//
// A new pin is established only for a machine that is not yet provisioned. An already-provisioned
// machine's running OS minor is not known here, so pinning the factory's newest minor could ask
// the bootstrap provider to skip a minor, which Talos does not support; such a machine is left
// unresolved (the pre-feature behavior) until its version is set deliberately. A fresh machine is
// installed at the pin, so there is no jump.
func (scope *machineReconcileScope) contractMinor(provisioned bool) string {
	if pinned := scope.tinkerbellMachine.GetAnnotations()[schematic.ContractAnnotation]; pinned != "" {
		return pinned
	}

	if provisioned {
		return ""
	}

	minor, err := scope.versionResolver.LatestMinor(scope.ctx)
	if err != nil {
		scope.log.V(1).Info("could not resolve newest Talos minor", "error", err.Error())

		return ""
	}

	if minor == "" {
		return ""
	}

	annotations := scope.tinkerbellMachine.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}

	annotations[schematic.ContractAnnotation] = minor
	scope.tinkerbellMachine.SetAnnotations(annotations)

	return minor
}

// specTalosVersion reads spec.talosVersion from the Machine's bootstrap config.
//
// The bootstrap object is read as unstructured on purpose: the infrastructure provider has no
// business importing a bootstrap provider's types, and any bootstrap provider exposing
// spec.talosVersion works unchanged.
//
// An empty result means the version is unset or the bootstrap config is not readable yet.
func (scope *machineReconcileScope) specTalosVersion() string {
	if scope.machine == nil || !scope.machine.Spec.Bootstrap.ConfigRef.IsDefined() {
		return ""
	}

	ref := scope.machine.Spec.Bootstrap.ConfigRef

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{
		Group: ref.APIGroup,
		// The contract-versioned reference carries no version; any served version exposes
		// spec.talosVersion identically, so the stored version is fine here.
		Version: "v1beta1",
		Kind:    ref.Kind,
	})

	key := types.NamespacedName{Namespace: scope.machine.Namespace, Name: ref.Name}
	if err := scope.client.Get(scope.ctx, key, obj); err != nil {
		// A missing or unreadable bootstrap config is not fatal to provisioning; it only means
		// the schematic cannot be resolved yet. The controller's ClusterRole must grant
		// get on talosconfigs.bootstrap.cluster.x-k8s.io (see the +kubebuilder:rbac marker on
		// Reconcile) — a Forbidden here silently disables schematic resolution.
		scope.log.V(1).Info("could not read bootstrap config for Talos version", "error", err.Error())

		return ""
	}

	version, found, err := unstructured.NestedString(obj.Object, "spec", "talosVersion")
	if err != nil || !found {
		return ""
	}

	return version
}

// parseMachineExtensions reads extra system extensions requested on the TinkerbellMachine,
// letting a workload ask for an extension without editing the shared Hardware record.
func parseMachineExtensions(annotations map[string]string) []string {
	value, ok := annotations[schematic.ExtensionsAnnotation]
	if !ok {
		return nil
	}

	var out []string

	for _, part := range regexp.MustCompile(`\s*,\s*`).Split(value, -1) {
		if part != "" {
			out = append(out, part)
		}
	}

	return out
}
