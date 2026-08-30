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

	talosVersion := scope.talosVersion()
	if talosVersion == "" {
		scope.log.V(1).Info("no full Talos version available, skipping schematic resolution",
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

// talosVersion reads the target Talos version from the Machine's bootstrap config.
//
// The bootstrap object is read as unstructured on purpose: the infrastructure provider has no
// business importing a bootstrap provider's types, and any bootstrap provider exposing
// spec.talosVersion works unchanged.
//
// An empty result means "not knowable", which callers treat as "do not resolve".
func (scope *machineReconcileScope) talosVersion() string {
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
		// the schematic cannot be resolved yet.
		scope.log.V(1).Info("could not read bootstrap config for Talos version", "error", err.Error())

		return ""
	}

	version, found, err := unstructured.NestedString(obj.Object, "spec", "talosVersion")
	if err != nil || !found {
		return ""
	}

	if !fullTalosVersion.MatchString(version) {
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
