package machine //nolint:testpackage

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-logr/logr"
	. "github.com/onsi/gomega" //nolint:revive // one day we will remove gomega
	"github.com/tinkerbell/tinkerbell/api/conversion"
	rufiov1 "github.com/tinkerbell/tinkerbell/api/v1alpha1/bmc"
	tinkv1 "github.com/tinkerbell/tinkerbell/api/v1alpha1/tinkerbell"
	tinkv2 "github.com/tinkerbell/tinkerbell/api/v1alpha2/tinkerbell"
	rufiov2 "github.com/tinkerbell/tinkerbell/api/v1alpha2/tinkerbell/bmc"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	infrastructurev1 "github.com/tinkerbell/cluster-api-provider-tinkerbell/api/v1beta2"
)

// tinkerbellCRDDirEnv names the directory of Tinkerbell's CRDs (crd/bases in the Tinkerbell
// repository) that TestTinkerbellCRDs installs into envtest.
const tinkerbellCRDDirEnv = "TINKERBELL_CRD_DIR"

// v1alpha2CRDs loads the Tinkerbell CRDs CAPT uses from dir, keeping only their v1alpha2
// version where they have one: serving v1alpha1 as well needs Tinkerbell's conversion webhook.
func v1alpha2CRDs(g Gomega, dir string) []*apiextensionsv1.CustomResourceDefinition {
	files := []string{
		"tinkerbell.org_hardware.yaml",
		"tinkerbell.org_workflows.yaml",
		"tinkerbell.org_tasks.yaml",
		"tinkerbell.org_templates.yaml",
		"bmc.tinkerbell.org_jobs.yaml",
	}
	crds := make([]*apiextensionsv1.CustomResourceDefinition, 0, len(files))

	for _, file := range files {
		b, err := os.ReadFile(filepath.Join(dir, file)) //nolint:gosec // a test reading the CRDs it is pointed at
		g.Expect(err).NotTo(HaveOccurred())

		crd := &apiextensionsv1.CustomResourceDefinition{}
		g.Expect(yaml.Unmarshal(b, crd)).To(Succeed())

		var versions []apiextensionsv1.CustomResourceDefinitionVersion

		for _, v := range crd.Spec.Versions {
			if v.Name == "v1alpha2" {
				v.Storage = true
				versions = append(versions, v)
			}
		}

		if versions != nil {
			crd.Spec.Versions = versions
		}

		crd.Spec.Conversion = nil
		crds = append(crds, crd)
	}

	return crds
}

// TestTinkerbellCRDs runs what CAPT creates and writes on the Tinkerbell cluster against a real
// API server with Tinkerbell's v1alpha2 CRDs, so their schemas and validation rules (which the
// fake client does not apply) accept it. It needs envtest's binaries (KUBEBUILDER_ASSETS) and
// Tinkerbell's CRDs (TINKERBELL_CRD_DIR), and is skipped without them.
func TestTinkerbellCRDs(t *testing.T) {
	t.Parallel()

	crdDir := os.Getenv(tinkerbellCRDDirEnv)
	if os.Getenv("KUBEBUILDER_ASSETS") == "" || crdDir == "" {
		t.Skipf("set KUBEBUILDER_ASSETS and %s (Tinkerbell's crd/bases) to run against the Tinkerbell CRDs", tinkerbellCRDDirEnv)
	}

	g := NewWithT(t)

	env := &envtest.Environment{CRDs: v1alpha2CRDs(g, crdDir), ErrorIfCRDPathMissing: true}

	cfg, err := env.Start()
	g.Expect(err).NotTo(HaveOccurred())

	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Logf("stopping envtest: %v", err)
		}
	})

	scheme := hardwareTestScheme(g)
	g.Expect(corev1.AddToScheme(scheme)).To(Succeed())

	tinkerbell, err := client.New(cfg, client.Options{Scheme: scheme})
	g.Expect(err).NotTo(HaveOccurred())

	// Each test works in a namespace of its own, so they run in parallel against the one API server.
	for name, test := range map[string]func(*testing.T, client.Client, string){
		"native":     testNativeWorkflowCRDs,
		"compat":     testCompatibilityWorkflowCRDs,
		"isoboot":    testBootOptionsCRDs(infrastructurev1.BootOptions{BootMode: tinkv1.BootModeIsoboot, ISOURL: "http://smee:7171/iso/hook.iso"}),
		"customboot": testBootOptionsCRDs(customboot()),
		"bmc":        testBMCJobCRDs,
		"hardware":   testHardwareWritesCRDs,
	} {
		g.Expect(tinkerbell.Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}})).To(Succeed())

		t.Run(name, func(t *testing.T) {
			t.Parallel()
			test(t, tinkerbell, name)
		})
	}
}

// customboot returns customboot options that reach the Workflow as BMC operations.
func customboot() infrastructurev1.BootOptions {
	return infrastructurev1.BootOptions{BootMode: tinkv1.BootModeCustomboot, CustombootConfig: tinkv1.CustombootConfig{
		PreparingActions: []rufiov1.Action{
			{PowerAction: ptr.To(rufiov1.PowerHardOff)},
			{VirtualMediaAction: &rufiov1.VirtualMediaAction{MediaURL: "http://smee:7171/iso/hook.iso", Kind: rufiov1.VirtualMediaCD}},
			{PowerAction: ptr.To(rufiov1.PowerOn)},
		},
		PostActions: []rufiov1.Action{{PowerAction: ptr.To(rufiov1.PowerHardOff)}},
	}}
}

// envtestScope is a scope whose Tinkerbell client is the envtest API server and whose
// management cluster is a fake client holding the management objects.
func envtestScope(g Gomega, tinkerbell client.Client, namespace, data string, management ...client.Object) *machineReconcileScope {
	machine := &infrastructurev1.TinkerbellMachine{
		ObjectMeta: metav1.ObjectMeta{Name: wfMachine, Namespace: namespace, UID: "machine-1-uid"},
		Status:     infrastructurev1.TinkerbellMachineStatus{TargetNamespace: namespace},
	}
	machine.Spec.TemplateInline = data

	cl := newHardwareTestClient(g, append(management, machine)...)

	return &machineReconcileScope{
		ctx:               context.Background(),
		log:               logr.Discard(),
		client:            cl,
		tinkerbellClient:  tinkerbell,
		scheme:            hardwareTestScheme(g),
		tinkerbellMachine: machine,
		tinkerbellCluster: &infrastructurev1.TinkerbellCluster{},
	}
}

// envtestHardware creates the test Hardware in namespace and returns it as stored.
func envtestHardware(g Gomega, tinkerbell client.Client, namespace string) *tinkv2.Hardware {
	hw := workflowTestHardware(true)
	hw.Namespace = namespace
	hw.Spec.BMC.Connection.CredentialsRef = rufiov2.SimpleReference{Name: "bmc", Namespace: namespace}
	g.Expect(tinkerbell.Create(context.Background(), hw)).To(Succeed())

	return hw
}

func testNativeWorkflowCRDs(t *testing.T, tinkerbell client.Client, namespace string) {
	t.Helper()
	g := NewWithT(t)

	hw := envtestHardware(g, tinkerbell, namespace)
	scope := envtestScope(g, tinkerbell, namespace, talosTemplate)

	g.Expect(scope.createWorkflow(hw)).To(Succeed(), "the API server must accept the Tasks and the Workflow")

	wf := &tinkv2.Workflow{}
	g.Expect(tinkerbell.Get(scope.ctx, client.ObjectKey{Name: wfMachine, Namespace: namespace}, wf)).To(Succeed())
	g.Expect(wf.Spec.Tasks).To(HaveLen(1))
	g.Expect(wf.Annotations).NotTo(HaveKey(AnnotationCompatibilityMode))

	task := &tinkv2.Task{}
	g.Expect(tinkerbell.Get(scope.ctx, client.ObjectKey{Name: wf.Spec.Tasks[0].TaskRef.Name, Namespace: namespace}, task)).To(Succeed())
	g.Expect(task.Spec.Actions).To(HaveLen(3))

	// Recreating the Workflow keeps the Tasks, as they are what it runs.
	g.Expect(tinkerbell.Delete(scope.ctx, wf)).To(Succeed())
	g.Expect(scope.createWorkflow(hw)).To(Succeed())

	g.Expect(scope.removeDependencies()).To(Succeed())

	tasks := &tinkv2.TaskList{}
	g.Expect(tinkerbell.List(scope.ctx, tasks, client.InNamespace(namespace))).To(Succeed())
	g.Expect(tasks.Items).To(BeEmpty())
}

func testCompatibilityWorkflowCRDs(t *testing.T, tinkerbell client.Client, namespace string) {
	t.Helper()
	g := NewWithT(t)

	hw := envtestHardware(g, tinkerbell, namespace)
	scope := envtestScope(g, tinkerbell, namespace, rangeTemplate)

	g.Expect(scope.createWorkflow(hw)).To(Succeed(), "the API server must accept the Template and the Workflow")

	wf := &tinkv2.Workflow{}
	g.Expect(tinkerbell.Get(scope.ctx, client.ObjectKey{Name: wfMachine, Namespace: namespace}, wf)).To(Succeed())
	g.Expect(wf.Annotations).To(HaveKey(AnnotationCompatibilityMode))
	g.Expect(wf.Annotations).To(HaveKeyWithValue(conversion.AnnotationTemplate, wfMachine))
	g.Expect(tinkerbell.Get(scope.ctx, client.ObjectKey{Name: wfMachine, Namespace: namespace}, &tinkv1.Template{})).To(Succeed())

	g.Expect(scope.removeDependencies()).To(Succeed())
}

// testBootOptionsCRDs returns a test that creates a native Workflow with options.
func testBootOptionsCRDs(options infrastructurev1.BootOptions) func(*testing.T, client.Client, string) {
	return func(t *testing.T, tinkerbell client.Client, namespace string) {
		t.Helper()
		g := NewWithT(t)

		hw := envtestHardware(g, tinkerbell, namespace)
		scope := envtestScope(g, tinkerbell, namespace, talosTemplate)
		scope.tinkerbellMachine.Spec.BootOptions = options

		g.Expect(scope.createWorkflow(hw)).To(Succeed(), "the API server must accept the boot options")

		wf := &tinkv2.Workflow{}
		g.Expect(tinkerbell.Get(scope.ctx, client.ObjectKey{Name: wfMachine, Namespace: namespace}, wf)).To(Succeed())
		g.Expect(wf.Spec.Tasks[0].Hardware.BootOptions.BootMode).NotTo(BeEmpty())
		g.Expect(wf.Annotations).NotTo(HaveKey(AnnotationCompatibilityMode))
	}
}

func testBMCJobCRDs(t *testing.T, tinkerbell client.Client, namespace string) {
	t.Helper()
	g := NewWithT(t)

	hw := envtestHardware(g, tinkerbell, namespace)
	scope := envtestScope(g, tinkerbell, namespace, talosTemplate)

	g.Expect(scope.createPowerOffJob(hw)).To(MatchError(ContainSubstring("requeue to wait for job.bmc completion")))
	g.Expect(scope.createPowerCycleJob(hw)).To(Succeed())

	jobs := &rufiov2.JobList{}
	g.Expect(tinkerbell.List(scope.ctx, jobs, client.InNamespace(namespace))).To(Succeed())
	g.Expect(jobs.Items).To(HaveLen(2), "the API server must accept both Jobs")

	for _, job := range jobs.Items {
		g.Expect(job.Spec.HardwareRef).To(Equal(&rufiov2.SimpleReference{Name: wfHardware, Namespace: namespace}))
	}
}

func testHardwareWritesCRDs(t *testing.T, tinkerbell client.Client, namespace string) {
	t.Helper()
	g := NewWithT(t)

	// Hardware created through v1alpha1, as the conversion webhook stores it.
	v1 := &tinkv1.Hardware{
		ObjectMeta: metav1.ObjectMeta{Name: ipamHardware, Namespace: namespace},
		Spec: tinkv1.HardwareSpec{
			BMCRef:   &corev1.TypedLocalObjectReference{Kind: "Machine", Name: "bmc-1"},
			Disks:    []tinkv1.Disk{{Device: "/dev/sda"}},
			Metadata: &tinkv1.HardwareMetadata{Facility: &tinkv1.MetadataFacility{FacilityCode: "onprem"}, Instance: &tinkv1.MetadataInstance{ID: ipamMAC}},
			Interfaces: []tinkv1.Interface{{
				DHCP:    &tinkv1.DHCP{MAC: ipamMAC, Hostname: "node-1", UEFI: true},
				Netboot: &tinkv1.Netboot{AllowPXE: ptr.To(true), AllowWorkflow: ptr.To(true)},
			}},
		},
	}
	hw := &tinkv2.Hardware{}
	g.Expect(conversion.HardwareToV1Alpha2(v1, hw)).To(Succeed())
	hw.ResourceVersion = ""
	g.Expect(tinkerbell.Create(context.Background(), hw)).To(Succeed())

	stash := hw.Annotations[conversion.AnnotationV1Alpha1]

	machine := ipamMachine()
	machine.Namespace = namespace
	claim, address := boundClaim(machine, hw, ipamMAC)
	scope := envtestScope(g, tinkerbell, namespace, talosTemplate, claim, address)
	// The claim is controlled by the machine with ipamMachine()'s UID.
	scope.tinkerbellMachine.UID = machine.UID
	scope.tinkerbellMachine.Spec.AddressFromPool = machine.Spec.AddressFromPool
	scope.tinkerbellMachine.Spec.HardwareName = ipamHardware
	scope.bootstrapCloudConfig = "#cloud-config"

	g.Expect(scope.takeHardwareOwnership(hw)).To(Succeed())
	g.Expect(scope.ensureHardwareUserData(hw, "tinkerbell://"+namespace+"/"+ipamHardware)).To(Succeed())
	g.Expect(scope.reconcileIPAM(hw)).To(Succeed())
	g.Expect(scope.patchHardwareAnnotations(hw, map[string]string{HardwareProvisionedAnnotation: "true"})).To(Succeed())

	stored := &tinkv2.Hardware{}
	g.Expect(tinkerbell.Get(scope.ctx, client.ObjectKeyFromObject(hw), stored)).To(Succeed())
	g.Expect(stored.Annotations).To(HaveKeyWithValue(conversion.AnnotationV1Alpha1, stash))
	g.Expect(stored.Spec.NetworkInterfaces[ipamMAC].IPAM.IPv4).To(Equal(&tinkv2.IP{
		Address: "10.1.40.32", Prefix: "24", Gateway: "10.1.40.1", Hostname: ptr.To("node-1"),
	}))
	g.Expect(stored.Spec.Instance.Userdata).To(Equal(ptr.To("#cloud-config")))

	g.Expect(scope.releaseHardware(stored)).To(Succeed())

	released := &tinkv2.Hardware{}
	g.Expect(tinkerbell.Get(scope.ctx, client.ObjectKeyFromObject(hw), released)).To(Succeed())
	g.Expect(released.Annotations).To(HaveKeyWithValue(conversion.AnnotationV1Alpha1, stash))
	g.Expect(released.Spec.NetworkInterfaces[ipamMAC].IPAM.IPv4).To(Equal(&tinkv2.IP{Hostname: ptr.To("node-1")}))
	g.Expect(released.Spec.Instance.Userdata).To(BeNil())
	g.Expect(released.Finalizers).To(BeEmpty())

	view := &tinkv1.Hardware{}
	g.Expect(conversion.HardwareToV1Alpha1(released, view)).To(Succeed())
	g.Expect(view.Spec.BMCRef).To(Equal(v1.Spec.BMCRef))
	g.Expect(view.Spec.Metadata.Facility).To(Equal(v1.Spec.Metadata.Facility))
	g.Expect(view.Spec.Interfaces[0].DHCP.UEFI).To(BeTrue())
}
