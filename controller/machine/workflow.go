package machine

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"strings"
	"text/template"

	"github.com/Masterminds/sprig/v3"
	"github.com/tinkerbell/tinkerbell/api/conversion"
	tinkv1 "github.com/tinkerbell/tinkerbell/api/v1alpha1/tinkerbell"
	tinkv2 "github.com/tinkerbell/tinkerbell/api/v1alpha2/tinkerbell"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/cluster-api/util/record"
)

const (
	// AnnotationCompatibilityMode is set on a Workflow CAPT created in v1alpha1 compatibility
	// mode, running the machine's v1alpha1 Template rather than v1alpha2 Tasks, to the reason why.
	AnnotationCompatibilityMode = "capt.tinkerbell.org/v1alpha1-compatibility"

	// compatibilityModeReason is the reason of the event recorded when a Workflow is created in
	// v1alpha1 compatibility mode.
	compatibilityModeReason = "V1Alpha1CompatibilityMode"
)

// errWorkflowFailed is the error returned when the workflow fails.
var errWorkflowFailed = errors.New("workflow failed")

var errWorkflowTimeout = errors.New("workflow timed out")

// errISOBootURLRequired is the error returned when the isoURL is required for iso boot mode.
var errISOBootURLRequired = errors.New("iso boot mode requires an isoURL")

// errNotRendered is returned by the stand-ins of Tinkerbell's template functions, which CAPT
// never calls.
var errNotRendered = errors.New("CAPT does not render templates")

func (scope *machineReconcileScope) getWorkflow() (*tinkv2.Workflow, error) {
	namespacedName := types.NamespacedName{
		Name:      scope.tinkerbellMachine.Name,
		Namespace: scope.tinkerbellNamespace(),
	}

	t := &tinkv2.Workflow{}

	err := scope.tinkerbellClient.Get(scope.ctx, namespacedName, t)
	if err != nil {
		return t, fmt.Errorf("getting workflow %s: %w", namespacedName, err)
	}

	return t, nil
}

// v1alpha1Workflow returns the Workflow CAPT creates for the machine in the v1alpha1 form it has
// always had: it runs the machine's Template on its Hardware, with device_1 in its hardwareMap and
// the machine's boot options. newWorkflow converts it to v1alpha2.
func (scope *machineReconcileScope) v1alpha1Workflow(hw *tinkv2.Hardware) (*tinkv1.Workflow, error) {
	workflow := &tinkv1.Workflow{
		ObjectMeta: metav1.ObjectMeta{
			Name:      scope.tinkerbellMachine.Name,
			Namespace: scope.tinkerbellNamespace(),
		},
		Spec: tinkv1.WorkflowSpec{
			TemplateRef: scope.tinkerbellMachine.Name,
			HardwareRef: hw.Name,
			HardwareMap: scope.workflowTemplateData(hw),
			BootOptions: tinkv1.BootOptions{
				ToggleAllowNetboot: true,
			},
		},
	}

	if err := scope.setResourceOwnership(workflow); err != nil {
		return nil, fmt.Errorf("setting workflow ownership: %w", err)
	}

	// We check for a BMC so that the implementation behaves similar to how it was when
	// CAPT was creating the BMCJob.
	if hasBMC(hw) {
		switch scope.tinkerbellMachine.Spec.BootOptions.BootMode {
		case tinkv1.BootModeNetboot:
			workflow.Spec.BootOptions.BootMode = tinkv1.BootModeNetboot
		case tinkv1.BootModeISO, tinkv1.BootModeIsoboot:
			if scope.tinkerbellMachine.Spec.BootOptions.ISOURL == "" {
				return nil, errISOBootURLRequired
			}

			u, err := url.Parse(scope.tinkerbellMachine.Spec.BootOptions.ISOURL)
			if err != nil {
				return nil, fmt.Errorf("boot option isoURL is not parse-able: %w", err)
			}

			urlPath, file := path.Split(u.Path)
			u.Path = path.Join(urlPath, strings.Replace(hardwareLookupID(hw), ":", "-", 5), file)

			workflow.Spec.BootOptions.ISOURL = u.String()
			workflow.Spec.BootOptions.BootMode = tinkv1.BootModeIsoboot

			// This is required for backward compatibility of Tinkerbell.
			if scope.tinkerbellMachine.Spec.BootOptions.BootMode == tinkv1.BootModeISO {
				workflow.Spec.BootOptions.BootMode = tinkv1.BootModeISO
			}
		case tinkv1.BootModeCustomboot:
			workflow.Spec.BootOptions.CustombootConfig = scope.tinkerbellMachine.Spec.BootOptions.CustombootConfig
			workflow.Spec.BootOptions.BootMode = tinkv1.BootModeCustomboot
		}
	}

	return workflow, nil
}

// newWorkflow returns the v1alpha2 form of the Workflow CAPT has always created (see
// v1alpha1Workflow), as Tinkerbell's conversion produces it: it runs the machine's Template, which
// its tinkerbell.org/v1alpha1-template annotation names, in v1alpha1 compatibility mode.
//
// The conversion records the v1alpha1 Workflow in the conversion.tinkerbell.org/v1alpha1
// annotation so that the conversion webhook can give it back unchanged. That annotation is the
// webhook's: a Workflow CAPT creates through v1alpha2 does not carry it.
func (scope *machineReconcileScope) newWorkflow(hw *tinkv2.Hardware) (*tinkv2.Workflow, error) {
	v1wf, err := scope.v1alpha1Workflow(hw)
	if err != nil {
		return nil, err
	}

	wf := &tinkv2.Workflow{}
	if err := conversion.WorkflowToV1Alpha2(v1wf, wf); err != nil {
		return nil, fmt.Errorf("converting workflow to v1alpha2: %w", err)
	}

	annotations := wf.GetAnnotations()
	delete(annotations, conversion.AnnotationV1Alpha1)
	wf.SetAnnotations(annotations)
	wf.Status = tinkv2.WorkflowStatus{}

	return wf, nil
}

// createWorkflow creates the machine's Workflow, natively or in v1alpha1 compatibility mode.
//
// When the machine's template converts to v1alpha2 Tasks and the Workflow can run them, CAPT
// creates the Tasks and a Workflow that runs them, as Tinkerbell rewrites a v1alpha1 Workflow onto
// Tasks itself (see conversion.RewriteWorkflowToTasks), and no v1alpha1 Template. Otherwise it
// creates the v1alpha1 Template and the converted Workflow, which Tinkerbell runs in v1alpha1
// compatibility mode exactly as it ran the v1alpha1 Workflow, and records why.
func (scope *machineReconcileScope) createWorkflow(hw *tinkv2.Hardware) error {
	// A machine provisioned from a Task list never takes the template path below.
	if tasks, origin := scope.taskSource(hw); len(tasks) > 0 {
		return scope.createTaskWorkflow(hw, tasks, origin)
	}

	wf, err := scope.newWorkflow(hw)
	if err != nil {
		return err
	}

	templateExists, err := scope.templateExists()
	if err != nil {
		return fmt.Errorf("checking if Template exists: %w", err)
	}

	if templateExists {
		// An earlier CAPT, or an earlier attempt that fell back to it, created the machine's
		// Template; the Workflow keeps running it, as it did before.
		return scope.createCompatibilityWorkflow(wf, "", "the machine's Template already exists")
	}

	data, err := scope.templateData(hw)
	if err != nil {
		return err
	}

	native, tasks, reason, err := scope.nativeWorkflow(wf, data)
	if err != nil {
		return err
	}

	if native == nil {
		return scope.createCompatibilityWorkflow(wf, data, reason)
	}

	if err := scope.ensureTasks(tasks); err != nil {
		return err
	}

	if err := scope.tinkerbellClient.Create(scope.ctx, native); err != nil {
		return fmt.Errorf("creating workflow: %w", err)
	}

	scope.log.Info("created Workflow running v1alpha2 Tasks", "workflow", native.Name, "tasks", len(tasks))

	return nil
}

// nativeWorkflow returns wf rewritten to run the Tasks data converts to, and those Tasks, owned
// by the machine. When wf cannot run natively it returns the reason instead.
func (scope *machineReconcileScope) nativeWorkflow(wf *tinkv2.Workflow, data string) (*tinkv2.Workflow, []tinkv2.Task, string, error) {
	conv, err := conversion.ConvertTemplate(data, templateFuncs())
	if err != nil {
		return nil, nil, "the template does not convert to v1alpha2 Tasks: " + notConvertibleReason(err), nil
	}

	// Compatibility mode renders customboot operations with v1alpha1 Hardware data; a native
	// Workflow renders them with v1alpha2 data, so templated ones would not mean the same.
	if templatedBootOptions(wf) {
		return nil, nil, "its boot options are templated, with v1alpha1 Hardware data", nil
	}

	tpl := scope.templateObject("")
	native := wf.DeepCopy()

	if err := conversion.RewriteWorkflowToTasks(native, tpl, conv); err != nil {
		return nil, nil, "the Workflow cannot run the template's Tasks: " + notConvertibleReason(err), nil
	}

	tasks := conversion.TemplateTasks(tpl, conv)
	for i := range tasks {
		if err := scope.setResourceOwnership(&tasks[i]); err != nil {
			return nil, nil, "", fmt.Errorf("setting Task ownership: %w", err)
		}
	}

	reason, err := scope.taskConflict(tasks)
	if err != nil || reason != "" {
		return nil, nil, reason, err
	}

	return native, tasks, "", nil
}

// createCompatibilityWorkflow creates wf to run the machine's v1alpha1 Template in v1alpha1
// compatibility mode, creating the Template from data unless data is empty (the Template exists).
func (scope *machineReconcileScope) createCompatibilityWorkflow(wf *tinkv2.Workflow, data, reason string) error {
	scope.log.Info("creating Workflow in v1alpha1 compatibility mode", "workflow", wf.Name, "reason", reason)
	record.Eventf(scope.tinkerbellMachine, compatibilityModeReason,
		"Workflow %s/%s runs Template %s in v1alpha1 compatibility mode: %s", wf.Namespace, wf.Name, wf.Name, reason)

	if data != "" {
		if err := scope.createTemplate(data); err != nil {
			if errors.Is(err, ErrTemplatesNotServed) {
				return fmt.Errorf("the Workflow cannot run v1alpha2 Tasks (%s) and cannot fall back to v1alpha1 compatibility mode: %w", reason, err)
			}

			return err
		}
	}

	// Tasks an earlier attempt created for the machine are not what this Workflow runs.
	if err := scope.removeTasks(nil); err != nil {
		return err
	}

	annotations := wf.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}

	annotations[AnnotationCompatibilityMode] = reason
	wf.SetAnnotations(annotations)

	if err := scope.tinkerbellClient.Create(scope.ctx, wf); err != nil {
		return fmt.Errorf("creating workflow: %w", err)
	}

	return nil
}

// notConvertibleReason returns why a conversion function refused, without its prefix.
func notConvertibleReason(err error) string {
	var nc conversion.NotConvertibleError
	if errors.As(err, &nc) {
		return nc.Reason
	}

	return err.Error()
}

// templatedBootOptions reports whether any of wf's boot options contains a template action.
func templatedBootOptions(wf *tinkv2.Workflow) bool {
	for _, task := range wf.Spec.Tasks {
		if task.Hardware == nil || task.Hardware.BootOptions == nil {
			continue
		}

		b, err := json.Marshal(task.Hardware.BootOptions)
		if err != nil || strings.Contains(string(b), "{{") {
			return true
		}
	}

	return false
}

// templateFuncs are the functions Tinkerbell renders Templates with, as conversion.ConvertTemplate
// needs them. It only parses Template data, so only their names matter: Sprig's hermetic functions
// and Tinkerbell's own helpers, which live in Tinkerbell's root module and are stood in for here.
func templateFuncs() template.FuncMap {
	funcs := sprig.HermeticTxtFuncMap()
	for _, name := range []string{"formatPartition", "netmaskToPrefixLength", "toYaml", "fromYaml"} {
		funcs[name] = notRendered
	}

	return funcs
}

// notRendered stands in for a Tinkerbell template function.
func notRendered(...any) (string, error) {
	return "", errNotRendered
}

// removeWorkflow makes sure workflow for TinkerbellMachine has been cleaned up.
func (scope *machineReconcileScope) removeWorkflow() error {
	namespacedName := types.NamespacedName{
		Name:      scope.tinkerbellMachine.Name,
		Namespace: scope.tinkerbellNamespace(),
	}

	workflow := &tinkv2.Workflow{}

	err := scope.tinkerbellClient.Get(scope.ctx, namespacedName, workflow)
	if err != nil {
		if apierrors.IsNotFound(err) {
			scope.log.Info("Workflow already removed", "name", namespacedName)

			return nil
		}

		return fmt.Errorf("checking if workflow exists: %w", err)
	}

	scope.log.Info("Removing Workflow", "name", namespacedName)

	if err := scope.tinkerbellClient.Delete(scope.ctx, workflow); err != nil {
		return fmt.Errorf("ensuring workflow has been removed: %w", err)
	}

	return nil
}

// workflowTemplateData is the substitution data made available to the Workflow's Template.
//
// Only the hardware identifier is supplied. Anything else a template needs, such as which
// operating system image to write, belongs on the Hardware object, which the Workflow
// controller exposes to templates in full. The infrastructure provider stays agnostic to it.
func (scope *machineReconcileScope) workflowTemplateData(hw *tinkv2.Hardware) map[string]string {
	return map[string]string{"device_1": hardwareLookupID(hw)}
}
