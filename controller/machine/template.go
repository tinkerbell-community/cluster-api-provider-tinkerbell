package machine

import (
	"cmp"
	"errors"
	"fmt"

	tinkv1 "github.com/tinkerbell/tinkerbell/api/v1alpha1/tinkerbell"
	tinkv2 "github.com/tinkerbell/tinkerbell/api/v1alpha2/tinkerbell"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	// ErrTemplateRefNoData is returned when a Template referenced by TemplateRef has no spec.data.
	ErrTemplateRefNoData = fmt.Errorf("template referenced by TemplateRef has no spec.data")

	// ErrTemplatesNotServed is returned when a v1alpha1 Template is needed and Tinkerbell no
	// longer serves Templates: their CRD was removed once Templates moved to v1alpha2 Tasks.
	ErrTemplatesNotServed = errors.New("tinkerbell no longer serves v1alpha1 Templates")

	// ErrNoTemplateFound is returned when no template source is configured at any level.
	ErrNoTemplateFound = fmt.Errorf("no template found: set templateInline or templateRef on TinkerbellMachine, "+
		"hardware annotation %q, or templateInline/templateRef on TinkerbellCluster", HardwareTemplateOverrideAnnotation)
)

// fetchTemplateRefData fetches a Tinkerbell Template by ObjectRef and returns its spec.data.
// source is a human-readable label for error messages (e.g. "machine TemplateRef").
func (scope *machineReconcileScope) fetchTemplateRefData(name, namespace, source string) (string, error) {
	refTemplate := &tinkv1.Template{}
	namespacedName := types.NamespacedName{
		Name:      name,
		Namespace: cmp.Or(namespace, scope.tinkerbellNamespace()),
	}
	if err := scope.tinkerbellClient.Get(scope.ctx, namespacedName, refTemplate); err != nil {
		if meta.IsNoMatchError(err) {
			return "", fmt.Errorf("getting Template %q referenced by %s: %w; use templateInline", namespacedName.String(), source, ErrTemplatesNotServed)
		}

		return "", fmt.Errorf("failed to get Template %q referenced by %s: %w", namespacedName.String(), source, err)
	}
	if refTemplate.Spec.Data == nil {
		return "", fmt.Errorf("%w: %s", ErrTemplateRefNoData, namespacedName.String())
	}

	return *refTemplate.Spec.Data, nil
}

func (scope *machineReconcileScope) templateExists() (bool, error) {
	namespacedName := types.NamespacedName{
		Name:      scope.tinkerbellMachine.Name,
		Namespace: scope.tinkerbellNamespace(),
	}

	err := scope.tinkerbellClient.Get(scope.ctx, namespacedName, &tinkv1.Template{})
	if err == nil {
		return true, nil
	}

	// With Templates no longer served, there is none.
	if !apierrors.IsNotFound(err) && !meta.IsNoMatchError(err) {
		return false, fmt.Errorf("checking if template exists: %w", err)
	}

	return false, nil
}

func (scope *machineReconcileScope) clusterTemplate() (string, error) {
	switch {
	case scope.tinkerbellCluster.Spec.TemplateInline != "":
		return scope.tinkerbellCluster.Spec.TemplateInline, nil
	case scope.tinkerbellCluster.Spec.TemplateRef != nil:
		ref := scope.tinkerbellCluster.Spec.TemplateRef
		return scope.fetchTemplateRefData(ref.Name, ref.Namespace, "cluster TemplateRef")
	}

	return "", nil
}

// templateData returns the data of the machine's template, which Hardware without storage
// devices cannot be provisioned with.
func (scope *machineReconcileScope) templateData(hw *tinkv2.Hardware) (string, error) {
	if len(hw.Spec.StorageDevices) < 1 {
		return "", ErrHardwareMissingDiskConfiguration
	}

	templateData, err := scope.resolveTemplateData(hw)
	if err != nil {
		return "", err
	}

	if templateData == "" {
		return "", ErrNoTemplateFound
	}

	return templateData, nil
}

// templateObject returns the machine's v1alpha1 Template with data: named after the machine, in
// the Tinkerbell namespace. Its Tasks (conversion.TemplateTasks) are named after it.
func (scope *machineReconcileScope) templateObject(data string) *tinkv1.Template {
	return &tinkv1.Template{
		ObjectMeta: metav1.ObjectMeta{
			Name:      scope.tinkerbellMachine.Name,
			Namespace: scope.tinkerbellNamespace(),
		},
		Spec: tinkv1.TemplateSpec{
			Data: &data,
		},
	}
}

// createTemplate creates the machine's v1alpha1 Template with data, for a Workflow that runs it in
// v1alpha1 compatibility mode.
func (scope *machineReconcileScope) createTemplate(data string) error {
	templateObject := scope.templateObject(data)

	if err := scope.setResourceOwnership(templateObject); err != nil {
		return fmt.Errorf("setting template ownership: %w", err)
	}

	if err := scope.tinkerbellClient.Create(scope.ctx, templateObject); err != nil {
		if meta.IsNoMatchError(err) {
			return fmt.Errorf("creating Tinkerbell template: %w", ErrTemplatesNotServed)
		}

		return fmt.Errorf("creating Tinkerbell template: %w", err)
	}

	return nil
}

// ownsResource reports whether obj carries this machine's ownership labels (see
// setResourceOwnership), which CAPT sets on every Tinkerbell object it creates for a machine.
func (scope *machineReconcileScope) ownsResource(obj client.Object) bool {
	labels := obj.GetLabels()

	return labels[LabelMachineName] == scope.tinkerbellMachine.Name &&
		labels[LabelMachineNamespace] == scope.tinkerbellMachine.Namespace
}

// taskConflict returns why the machine cannot use tasks: a Task of one of their names exists that
// is not the machine's, such as one Tinkerbell translated from a Template of the same name.
func (scope *machineReconcileScope) taskConflict(tasks []tinkv2.Task) (string, error) {
	for i := range tasks {
		key := client.ObjectKeyFromObject(&tasks[i])
		existing := &tinkv2.Task{}

		err := scope.tinkerbellClient.Get(scope.ctx, key, existing)

		switch {
		case apierrors.IsNotFound(err):
			continue
		case err != nil:
			return "", fmt.Errorf("getting Task %s: %w", key, err)
		case !scope.ownsResource(existing):
			return fmt.Sprintf("Task %s already exists and does not belong to this machine", key), nil
		}
	}

	return "", nil
}

// ensureTasks makes the machine's Tasks those given: it creates them, updates the ones it already
// has to match (an earlier attempt may have left them), and deletes any other Task of the machine.
func (scope *machineReconcileScope) ensureTasks(tasks []tinkv2.Task) error {
	keep := make(map[string]bool, len(tasks))

	for i := range tasks {
		task := &tasks[i]
		keep[task.Name] = true

		existing := &tinkv2.Task{}

		err := scope.tinkerbellClient.Get(scope.ctx, client.ObjectKeyFromObject(task), existing)

		switch {
		case apierrors.IsNotFound(err):
			if err := scope.tinkerbellClient.Create(scope.ctx, task); err != nil {
				return fmt.Errorf("creating Task %s/%s: %w", task.Namespace, task.Name, err)
			}

			continue
		case err != nil:
			return fmt.Errorf("getting Task %s/%s: %w", task.Namespace, task.Name, err)
		case equality.Semantic.DeepEqual(existing.Spec, task.Spec) &&
			equality.Semantic.DeepEqual(existing.Annotations, task.Annotations):
			continue
		}

		existing.Spec = task.Spec
		existing.Annotations = task.Annotations

		if err := scope.tinkerbellClient.Update(scope.ctx, existing); err != nil {
			return fmt.Errorf("updating Task %s/%s: %w", task.Namespace, task.Name, err)
		}
	}

	return scope.removeTasks(keep)
}

// removeTasks deletes the machine's Tasks, except those named in keep.
func (scope *machineReconcileScope) removeTasks(keep map[string]bool) error {
	tasks := &tinkv2.TaskList{}

	if err := scope.tinkerbellClient.List(scope.ctx, tasks,
		client.InNamespace(scope.tinkerbellNamespace()),
		client.MatchingLabels{
			LabelMachineName:      scope.tinkerbellMachine.Name,
			LabelMachineNamespace: scope.tinkerbellMachine.Namespace,
		},
	); err != nil {
		return fmt.Errorf("listing Tasks of the machine: %w", err)
	}

	for i := range tasks.Items {
		task := &tasks.Items[i]
		if keep[task.Name] {
			continue
		}

		scope.log.Info("Removing Task", "name", client.ObjectKeyFromObject(task))

		if err := scope.tinkerbellClient.Delete(scope.ctx, task); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("removing Task %s/%s: %w", task.Namespace, task.Name, err)
		}
	}

	return nil
}

// resolveTemplateData resolves the workflow template data by checking, in order:
// machine-level inline, machine-level ref, hardware annotation, cluster-level template.
func (scope *machineReconcileScope) resolveTemplateData(hw *tinkv2.Hardware) (string, error) {
	if data := scope.tinkerbellMachine.Spec.TemplateInline; data != "" {
		return data, nil
	}

	if scope.tinkerbellMachine.Spec.TemplateRef != nil {
		ref := scope.tinkerbellMachine.Spec.TemplateRef
		return scope.fetchTemplateRefData(ref.Name, ref.Namespace, "machine TemplateRef")
	}

	scope.log.Info("machine template fields are empty, trying from hardware annotation")
	if data, err := scope.templateFromAnnotation(hw); err != nil {
		return "", err
	} else if data != "" {
		return data, nil
	}

	return scope.clusterTemplate()
}

func (scope *machineReconcileScope) templateFromAnnotation(hw *tinkv2.Hardware) (string, error) {
	// Check if hardware has an annotation 'hardware.tinkerbell.org/capt-template-override', if so,
	// use it as the name of a Template resource in the same namespace as the Hardware, load it,
	// and use its spec.data as the template.
	scope.log.Info("hardware annotations", "annotations", hw.Annotations)
	templateName, ok := hw.Annotations[HardwareTemplateOverrideAnnotation]
	if !ok {
		return "", nil
	}

	scope.log.Info("found template override in Hardware annotation", "templateName", templateName, "namespace", hw.Namespace)

	data, err := scope.fetchTemplateRefData(templateName, hw.Namespace, "hardware annotation")
	if err != nil {
		return "", fmt.Errorf("failed to get template from hardware annotation: %w", err)
	}

	scope.log.V(4).Info("found template override in Hardware annotations, using it as template", "templateName", templateName)

	return data, nil
}

// removeTemplate makes sure template for TinkerbellMachine has been cleaned up. CAPT creates it
// for Workflows in v1alpha1 compatibility mode; CAPT before v1alpha2 created it for every machine.
func (scope *machineReconcileScope) removeTemplate() error {
	namespacedName := types.NamespacedName{
		Name:      scope.tinkerbellMachine.Name,
		Namespace: scope.tinkerbellNamespace(),
	}

	template := &tinkv1.Template{}

	err := scope.tinkerbellClient.Get(scope.ctx, namespacedName, template)
	if err != nil {
		// With Templates no longer served, there is none to remove.
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			scope.log.Info("Template already removed", "name", namespacedName)

			return nil
		}

		return fmt.Errorf("checking if template exists: %w", err)
	}

	scope.log.Info("Removing Template", "name", namespacedName)

	if err := scope.tinkerbellClient.Delete(scope.ctx, template); err != nil {
		return fmt.Errorf("ensuring template has been removed: %w", err)
	}

	return nil
}
