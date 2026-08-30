// Package schematic resolves Talos Image Factory schematics from Tinkerbell Hardware.
//
// A schematic describes the customisations baked into a Talos image — principally which
// system extensions are included. The Image Factory is content-addressed: the same schematic
// always yields the same ID, so resolution is deterministic and a machine's image reference
// does not churn between reconciles.
//
// The resolved schematic backs two different consumers, which is the point of computing it
// in one place:
//
//   - the disk image the Tinkerbell Workflow writes during initial provisioning
//   - the installer image recorded as machine.install.image, which Talos uses to upgrade
//     itself in place
//
// Both derive from one ID, so a machine is installed and upgraded with the same set of
// extensions.
package schematic

import (
	"fmt"
	"sort"
	"strings"

	tinkv1 "github.com/tinkerbell/tinkerbell/api/v1alpha1/tinkerbell"
	"sigs.k8s.io/yaml"
)

const (
	// ExtensionsAnnotation lists additional official system extensions to include, comma
	// separated, e.g. "siderolabs/gvisor,siderolabs/intel-ucode".
	//
	// Built-in rules only cover signals Tinkerbell reliably exposes. Anything that depends on
	// knowledge Hardware does not carry — GPU models, specific NIC modules, CPU vendor — is
	// declared here rather than guessed at.
	ExtensionsAnnotation = "talos.tinkerbell.org/system-extensions"

	// DefaultFactoryURL is the public Image Factory.
	DefaultFactoryURL = "https://factory.talos.dev"

	// nvmeCLIExtension provides NVMe userspace tooling.
	//
	// Note this does not enable NVMe support: Talos has NVMe in the kernel already, and a node
	// boots from an NVMe disk without it. It adds the nvme(8) tooling, which is what makes a
	// node with NVMe storage debuggable.
	nvmeCLIExtension = "siderolabs/nvme-cli"

	// archAMD64 and archARM64 are the Talos architecture names.
	archAMD64 = "amd64"
	archARM64 = "arm64"
)

// Signals are the facts about a machine that determine its schematic.
type Signals struct {
	// Architecture is the Talos architecture name, "amd64" or "arm64".
	Architecture string

	// DiskDevices are the block device paths declared on the Hardware.
	DiskDevices []string

	// ExtraExtensions are operator-supplied official extensions.
	ExtraExtensions []string
}

// SignalsFromHardware extracts the schematic-relevant facts from a Hardware object.
//
// extraFromMachine lets a TinkerbellMachine contribute extensions as well as the Hardware, so
// a workload-specific extension can be requested without editing the shared hardware record.
func SignalsFromHardware(hw *tinkv1.Hardware, extraFromMachine []string) Signals {
	signals := Signals{
		Architecture:    architectureOf(hw),
		ExtraExtensions: append(parseExtensions(hw.GetAnnotations()[ExtensionsAnnotation]), extraFromMachine...),
	}

	for _, disk := range hw.Spec.Disks {
		if disk.Device != "" {
			signals.DiskDevices = append(signals.DiskDevices, disk.Device)
		}
	}

	return signals
}

// architectureOf maps the iPXE architecture Tinkerbell records on an interface onto the Talos
// architecture name, defaulting to amd64 when nothing usable is present.
func architectureOf(hw *tinkv1.Hardware) string {
	for _, iface := range hw.Spec.Interfaces {
		if iface.DHCP == nil || iface.DHCP.Arch == "" {
			continue
		}

		switch strings.ToLower(iface.DHCP.Arch) {
		case "aarch64", archARM64:
			return archARM64
		case "x86_64", archAMD64, "x86":
			return archAMD64
		}
	}

	return archAMD64
}

func parseExtensions(value string) []string {
	if value == "" {
		return nil
	}

	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))

	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}

	return out
}

// Schematic is the Image Factory schematic document.
type Schematic struct {
	Customization Customization `json:"customization"`
}

// Customization holds the schematic's customisation blocks.
//
// Only system extensions are modelled. The Image Factory ignores extraKernelArgs and meta for
// installer and initramfs images, and those are the artefacts this package produces, so
// carrying them would imply an effect that does not exist.
type Customization struct {
	SystemExtensions SystemExtensions `json:"systemExtensions,omitempty"`
}

// SystemExtensions lists the official extensions baked into the image.
type SystemExtensions struct {
	OfficialExtensions []string `json:"officialExtensions,omitempty"`
}

// Build applies the built-in rules to signals and returns the resulting schematic.
//
// Extensions are deduplicated and sorted. That is load-bearing rather than cosmetic: the
// Image Factory hashes the document to derive the ID, so an unstable ordering would produce a
// different ID for identical hardware and churn the machine's image on every reconcile.
func Build(signals Signals) Schematic {
	extensions := map[string]struct{}{}

	for _, extension := range signals.ExtraExtensions {
		extensions[extension] = struct{}{}
	}

	if hasNVMeDisk(signals.DiskDevices) {
		extensions[nvmeCLIExtension] = struct{}{}
	}

	names := make([]string, 0, len(extensions))
	for name := range extensions {
		names = append(names, name)
	}

	sort.Strings(names)

	return Schematic{Customization: Customization{
		SystemExtensions: SystemExtensions{OfficialExtensions: names},
	}}
}

// hasNVMeDisk reports whether any declared disk is an NVMe namespace.
func hasNVMeDisk(devices []string) bool {
	for _, device := range devices {
		if strings.HasPrefix(device, "/dev/nvme") {
			return true
		}
	}

	return false
}

// Marshal renders the schematic as the YAML the Image Factory expects.
func (s Schematic) Marshal() ([]byte, error) {
	encoded, err := yaml.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("marshalling schematic: %w", err)
	}

	return encoded, nil
}

// InstallerImage returns the installer image reference for a schematic and Talos version.
//
// This is the value that belongs in machine.install.image, and the one Talos upgrades to.
// The installer resolves its own architecture, so none is encoded here.
func InstallerImage(factoryURL, id, talosVersion string) string {
	return fmt.Sprintf("%s/metal-installer/%s:%s", registryHost(factoryURL), id, talosVersion)
}

// DiskImageURL returns the raw disk image a Tinkerbell Workflow writes during provisioning.
//
// Unlike the installer reference this is architecture-specific, because it is a concrete
// artefact rather than a multi-arch image reference.
func DiskImageURL(factoryURL, id, talosVersion, architecture string) string {
	return fmt.Sprintf("%s/image/%s/%s/metal-%s.raw.xz",
		strings.TrimSuffix(factoryURL, "/"), id, talosVersion, architecture)
}

// registryHost strips the scheme so a factory URL can be used as an image registry host.
func registryHost(factoryURL string) string {
	host := strings.TrimSuffix(factoryURL, "/")
	host = strings.TrimPrefix(host, "https://")
	host = strings.TrimPrefix(host, "http://")

	return host
}
