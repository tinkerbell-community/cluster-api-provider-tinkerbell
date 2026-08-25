package schematic_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	tinkv1 "github.com/tinkerbell/tinkerbell/api/v1alpha1/tinkerbell"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/tinkerbell/cluster-api-provider-tinkerbell/pkg/schematic"
)

func hardware(annotations map[string]string, arch string, disks ...string) *tinkv1.Hardware {
	hw := &tinkv1.Hardware{
		ObjectMeta: metav1.ObjectMeta{Name: "hw-1", Namespace: "default", Annotations: annotations},
	}

	for _, device := range disks {
		hw.Spec.Disks = append(hw.Spec.Disks, tinkv1.Disk{Device: device})
	}

	if arch != "" {
		hw.Spec.Interfaces = []tinkv1.Interface{{DHCP: &tinkv1.DHCP{Arch: arch}}}
	}

	return hw
}

func extensionsFor(t *testing.T, hw *tinkv1.Hardware, extra ...string) []string {
	t.Helper()

	return schematic.Build(schematic.SignalsFromHardware(hw, extra)).
		Customization.SystemExtensions.OfficialExtensions
}

func TestNVMeDiskAddsNVMeCLI(t *testing.T) {
	got := extensionsFor(t, hardware(nil, "x86_64", "/dev/nvme0n1"))

	if len(got) != 1 || got[0] != "siderolabs/nvme-cli" {
		t.Fatalf("extensions = %v, want [siderolabs/nvme-cli]", got)
	}
}

func TestNonNVMeDiskAddsNothing(t *testing.T) {
	if got := extensionsFor(t, hardware(nil, "x86_64", "/dev/sda")); len(got) != 0 {
		t.Fatalf("extensions = %v, want none", got)
	}
}

// A machine with mixed storage still needs the tooling.
func TestMixedDisksAddNVMeCLI(t *testing.T) {
	got := extensionsFor(t, hardware(nil, "x86_64", "/dev/sda", "/dev/nvme1n1"))

	if len(got) != 1 || got[0] != "siderolabs/nvme-cli" {
		t.Fatalf("extensions = %v, want [siderolabs/nvme-cli]", got)
	}
}

func TestAnnotationExtensionsAreIncluded(t *testing.T) {
	hw := hardware(map[string]string{
		schematic.ExtensionsAnnotation: "siderolabs/gvisor, siderolabs/intel-ucode",
	}, "x86_64")

	got := extensionsFor(t, hw)

	want := []string{"siderolabs/gvisor", "siderolabs/intel-ucode"}
	if len(got) != len(want) {
		t.Fatalf("extensions = %v, want %v", got, want)
	}

	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("extensions = %v, want %v", got, want)
		}
	}
}

// Built-in rules and operator-supplied extensions combine rather than override.
func TestAnnotationAndRuleExtensionsMerge(t *testing.T) {
	hw := hardware(map[string]string{
		schematic.ExtensionsAnnotation: "siderolabs/gvisor",
	}, "x86_64", "/dev/nvme0n1")

	got := extensionsFor(t, hw)
	if len(got) != 2 {
		t.Fatalf("extensions = %v, want gvisor and nvme-cli", got)
	}
}

// Duplicates from different sources must collapse, or the schematic content changes and the
// factory hands back a different ID for identical hardware.
func TestDuplicateExtensionsAreDeduplicated(t *testing.T) {
	hw := hardware(map[string]string{
		schematic.ExtensionsAnnotation: "siderolabs/nvme-cli",
	}, "x86_64", "/dev/nvme0n1")

	if got := extensionsFor(t, hw); len(got) != 1 {
		t.Fatalf("extensions = %v, want one entry", got)
	}
}

// The whole scheme depends on this: identical hardware must always marshal identically, or
// the resolved image would churn on every reconcile.
func TestSchematicIsDeterministic(t *testing.T) {
	hwA := hardware(map[string]string{
		schematic.ExtensionsAnnotation: "siderolabs/gvisor,siderolabs/intel-ucode",
	}, "x86_64", "/dev/nvme0n1")

	hwB := hardware(map[string]string{
		schematic.ExtensionsAnnotation: "siderolabs/intel-ucode,siderolabs/gvisor",
	}, "x86_64", "/dev/nvme0n1")

	first, err := schematic.Build(schematic.SignalsFromHardware(hwA, nil)).Marshal()
	if err != nil {
		t.Fatal(err)
	}

	second, err := schematic.Build(schematic.SignalsFromHardware(hwB, []string{"siderolabs/gvisor"})).Marshal()
	if err != nil {
		t.Fatal(err)
	}

	if string(first) != string(second) {
		t.Fatalf("schematic is order-dependent:\n%s\nvs\n%s", first, second)
	}
}

func TestArchitectureMapping(t *testing.T) {
	for arch, want := range map[string]string{
		"x86_64":  "amd64",
		"aarch64": "arm64",
		"arm64":   "arm64",
		"":        "amd64",
		"weird":   "amd64",
	} {
		got := schematic.SignalsFromHardware(hardware(nil, arch), nil).Architecture
		if got != want {
			t.Errorf("arch %q = %q, want %q", arch, got, want)
		}
	}
}

func TestImageReferences(t *testing.T) {
	const id = "9ed5fecdacb36b5c5427b87d409f1065cfb2df69b0f71c58b868d9d466d8dab3"

	installer := schematic.InstallerImage(schematic.DefaultFactoryURL, id, "v1.14.0-rc.1")
	want := "factory.talos.dev/metal-installer/" + id + ":v1.14.0-rc.1"

	if installer != want {
		t.Errorf("installer = %q, want %q", installer, want)
	}

	disk := schematic.DiskImageURL(schematic.DefaultFactoryURL, id, "v1.14.0-rc.1", "amd64")
	wantDisk := "https://factory.talos.dev/image/" + id + "/v1.14.0-rc.1/metal-amd64.raw.xz"

	if disk != wantDisk {
		t.Errorf("disk image = %q, want %q", disk, wantDisk)
	}
}

func TestRegisterUploadsOnceAndCaches(t *testing.T) {
	var calls int

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++

		if r.URL.Path != "/schematics" {
			t.Errorf("path = %q, want /schematics", r.URL.Path)
		}

		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", r.Method)
		}

		_ = json.NewEncoder(w).Encode(map[string]string{"id": "abc123"})
	}))
	defer server.Close()

	registrar := schematic.NewRegistrar(server.URL)
	doc := schematic.Build(schematic.SignalsFromHardware(hardware(nil, "x86_64", "/dev/nvme0n1"), nil))

	for range 3 {
		id, err := registrar.Register(context.Background(), doc)
		if err != nil {
			t.Fatal(err)
		}

		if id != "abc123" {
			t.Fatalf("id = %q, want abc123", id)
		}
	}

	if calls != 1 {
		t.Errorf("uploaded %d times, want 1 — steady-state reconciles must not call the factory", calls)
	}
}

func TestRegisterReportsFactoryErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("invalid schematic"))
	}))
	defer server.Close()

	_, err := schematic.NewRegistrar(server.URL).
		Register(context.Background(), schematic.Build(schematic.Signals{}))
	if err == nil {
		t.Fatal("expected an error when the factory rejects the schematic")
	}
}
