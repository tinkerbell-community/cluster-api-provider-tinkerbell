# v1alpha2 Task Fields Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let TinkerbellMachines and TinkerbellClusters describe provisioning as an ordered
`tasks` list of v1alpha2 Tasks (inline or by reference, each with optional boot options),
deprecating `templateInline`/`templateRef`, and let CABPT absorb the new fields in place.

**Architecture:** New v1beta2 API types embed Tinkerbell's v1alpha2 `TaskSpec` and
`BootOptions`. The machine controller decides early whether a machine's source is a Task list
(`taskSource`). If so, a new path builds the native v1alpha2 Workflow directly
(`createTaskWorkflow`). Otherwise the existing template path runs untouched. The webhooks add
deprecation warnings and exclusivity checks. CABPT's allowlist grows.

**Tech Stack:** Go 1.26, controller-runtime, kubebuilder markers/CEL, envtest 1.37, gomega
(CAPT), testify (CABPT), kind.

**Spec:** `docs/superpowers/specs/2026-11-08-v1alpha2-task-fields-design.md`

## Global Constraints

- CAPT work: worktree `/home/appkins/src/tinkerbell-community/cluster-api-provider-tinkerbell.tasks`,
  branch `feat/v1alpha2-task-fields` (from `origin/main` 0e7e65b).
- CABPT work: create worktree
  `/home/appkins/src/sidero-community/cluster-api-bootstrap-provider-talos.tasks`, branch
  `feat/tinkerbell-task-fields` from `main` 94d31ab.
- Never stash or commit in the main CAPT, CABPT or Tinkerbell checkouts. Other sessions use
  them.
- Commit locally on the feature branches. Don't push, tag or release: that needs the user's
  say-so.
- Tinkerbell types come from `github.com/tinkerbell/tinkerbell/api` replaced by
  `github.com/tinkerbell-community/tinkerbell/api v0.28.0`. Import aliases as in the code:
  `tinkv1` (v1alpha1/tinkerbell), `tinkv2` (v1alpha2/tinkerbell), `infrastructurev1`
  (CAPT api/v1beta2).
- Field names, exactly: `tasks`, `taskRef` (`name`, `namespace`), `taskInline`,
  `bootOptions`. Annotation: `hardware.tinkerbell.org/capt-task-refs`.
- Task created from an inline entry: name `<TinkerbellMachine name>-<entry name>`, in
  `scope.tinkerbellNamespace()`, labelled via `setResourceOwnership`.
- `taskRef.namespace` defaults to `scope.tinkerbellNamespace()`. Annotation refs use the
  Hardware's namespace.
- Lint: `GOTOOLCHAIN=go1.26.8 make lint` in CAPT.
- envtest: `export KUBEBUILDER_ASSETS=$HOME/.local/share/kubebuilder-envtest/k8s/1.37.0-linux-amd64`.
  Tinkerbell CRDs for `TestTinkerbellCRDs`: extract them once with
  `mkdir -p $SCRATCH/tink-crds && git -C /home/appkins/src/tinkerbell-community/tinkerbell archive v0.28.6 crd/bases | tar -x -C $SCRATCH/tink-crds`
  and `export TINKERBELL_CRD_DIR=$SCRATCH/tink-crds/crd/bases` (where `$SCRATCH` is the
  session scratchpad).
- Never run kubectl, helm or clusterctl against `admin@talos` or any context except the
  kind cluster created in Task 7. Always pass that cluster's own `--kubeconfig`.
- Event reasons, exactly: `DeprecatedTemplateSource` (Normal), `ConflictingHardwareAnnotations`,
  `BootOptionsIgnored`, `TaskNotFound`, `TaskConflict`, `TemplatedCustombootConfig`,
  `WorkflowNotRendered` (Warning). Emit with `sigs.k8s.io/cluster-api/util/record`
  (`record.Eventf` / `record.Warnf`) on `scope.tinkerbellMachine`, as `workflow.go` does.

## Review Focus

1. **CRD size and CEL cost.** Embedding `TaskSpec` and `BootOptions` (with BMC operations)
   into four CRDs can blow the API server's CEL cost budget or the 256 KiB client-side apply
   limit. The CRDs must still install, and each stays under 250000 bytes. *(Task 1: envtest
   install plus a size check step)*
2. **An inline Task that doesn't render.** The likeliest migration mistake is v1alpha1 syntax
   (`.Hardware.Disks`) in a `taskInline`. Tinkerbell then leaves the Workflow at state 0 with
   `TemplateRenderedSuccess=False`, and today CAPT says nothing. Expected: a Warning event
   with Tinkerbell's message, and a returned error. *(Task 5: `Test_reconcile_reports_a_workflow_that_does_not_render`)*
3. **External Tinkerbell cluster.** With `externalTinkerbell`, Tasks and Workflows must go
   through `scope.tinkerbellClient`, never `scope.client`. *(Task 5:
   `Test_createTaskWorkflow_uses_the_tinkerbell_client`)*
4. **Untidy annotation values.** `" a, ,b,"` means Tasks `a` then `b`. Whitespace is trimmed,
   empty items are skipped, duplicates are kept in order. *(Task 3: `Test_hardwareTaskRefs`)*
5. **A missing reference after inline Tasks were prepared.** Checking references must happen
   before any Task is created, so a missing reference leaves no Tasks behind. *(Task 5:
   `Test_createTaskWorkflow_missing_ref_creates_nothing`)*

---

### Task 1: API types, CEL rules, generated code and v1beta1 conversion

**Files:**
- Modify: `api/go.mod` (add `replace github.com/tinkerbell/tinkerbell/api => github.com/tinkerbell-community/tinkerbell/api v0.28.0`; `cd api && go mod tidy`)
- Modify: `api/v1beta2/types.go` (new types)
- Modify: `api/v1beta2/tinkerbellmachine.go` (`TinkerbellMachineConfig`: field, CEL, doc comments)
- Modify: `api/v1beta2/tinkerbellcluster.go` (`TinkerbellClusterSpec`: field, CEL, doc comments)
- Modify: `api/v1beta1/conversion.go` (restore `Tasks` in `ConvertClusterToHub`, `ConvertMachineToHub`, `ConvertMachineTemplateToHub`)
- Modify: `api/v1beta1/conversion_test.go`
- Regenerate: `api/v1beta2/zz_generated.deepcopy.go`, `config/crd/bases/*.yaml` (`make generate`)
- Create: `webhooks/crd_validation_test.go` (envtest against `config/crd/bases`)

**Interfaces:**
- Produces (package `api/v1beta2`):
  ```go
  type MachineTask struct {
      Name        string              `json:"name"`                  // Pattern ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$, MaxLength 63
      TaskRef     *TaskRef            `json:"taskRef,omitempty"`
      TaskInline  *tinkv2.TaskSpec    `json:"taskInline,omitempty"`
      BootOptions *tinkv2.BootOptions `json:"bootOptions,omitempty"`
  }
  type TaskRef struct {
      Name      string `json:"name"`
      Namespace string `json:"namespace,omitempty"`
  }
  // on TinkerbellMachineConfig and TinkerbellClusterSpec:
  Tasks []MachineTask `json:"tasks,omitempty"` // +listType=map +listMapKey=name +MinItems=1 +MaxItems=32
  ```
- CEL on `MachineTask`: `has(self.taskRef) != has(self.taskInline)`, message
  `exactly one of taskRef and taskInline must be set`.
- CEL on `TinkerbellMachineConfig` and `TinkerbellClusterSpec` (replacing the two-field
  rule): `[has(self.tasks), has(self.templateInline), has(self.templateRef)].filter(x, x).size() <= 1`,
  message `at most one of tasks, templateInline and templateRef may be set`.
- Doc comments: `templateInline`/`templateRef` start their last paragraph with
  `Deprecated: use tasks.`. The machine-level `bootOptions` says it applies to template
  sources, and to `tasks` whose entries set no `bootOptions`.

- [ ] **Step 1: Write the failing round-trip test** in `api/v1beta1/conversion_test.go`:
  `TestTasksSurviveRoundTrip` builds a hub `TinkerbellMachine`, `TinkerbellMachineTemplate`
  and `TinkerbellCluster`, each with `Tasks` = one `taskInline` entry (one action, with
  `BootOptions{ToggleNetboot: true, BootMode: "netboot"}`) plus one `taskRef` entry
  (`{Name: "shared", Namespace: "tink"}`). Each is converted hub → v1beta1 → hub, and the
  test asserts `cmp.Diff(want.Spec, got.Spec)` is empty (template: `Spec.Template.Spec`).
- [ ] **Step 2: Write the failing CRD validation test** `webhooks/crd_validation_test.go`,
  `TestCRDValidation`:
  - Starts envtest with `CRDDirectoryPaths: []string{filepath.Join("..", "config", "crd", "bases")}`,
    `ErrorIfCRDPathMissing: true`. Skipped when `KUBEBUILDER_ASSETS` is unset.
  - Expects creating a v1beta2 object to fail with the CEL message for each of:
    - `TinkerbellMachine` with `tasks` + `templateInline`;
    - `TinkerbellCluster` with `tasks` + `templateRef`;
    - an entry with both `taskRef` and `taskInline`;
    - an entry with neither;
    - two entries named `a` (expect a duplicate-key error);
    - an entry `bootOptions.bootMode: iso` (the v1alpha2 enum).
  - Expects success for a mixed two-entry list on a `TinkerbellMachine`, a
    `TinkerbellMachineTemplate` and a `TinkerbellCluster`, and for a `templateInline`-only
    machine.
- [ ] **Step 3: Run both and see them fail**:
  `cd api && go test ./v1beta1/ -run TestTasksSurviveRoundTrip` and
  `go test ./webhooks/ -run TestCRDValidation`. Expected: compile errors (`Tasks` undefined).
- [ ] **Step 4: Add the replace to `api/go.mod`**, run `cd api && go mod tidy`, then add the
  types, fields and markers above.
- [ ] **Step 5: Restore `Tasks` in the three `...ToHub` functions** from the `restored`
  object, next to `TemplateRef`. For the cluster, replace the `_ = restored` placeholder.
- [ ] **Step 6: Regenerate.** Run `make generate`. Expected: deepcopy and the four CRDs
  change; `git status` shows nothing else unexpected.
- [ ] **Step 7: Check CRD sizes.** Run `wc -c config/crd/bases/*.yaml`. Expected: each file
  is under 250000 bytes. If one isn't, stop and report rather than trimming the schema.
- [ ] **Step 8: Run the tests**: `cd api && go test ./...`, which includes the existing
  `TestFuzzyConversion`, and `go test ./webhooks/ -run TestCRDValidation` with
  `KUBEBUILDER_ASSETS` set. Expected: PASS. If envtest rejects a CRD for CEL cost, lower
  `MaxItems` and say so in the commit message.
- [ ] **Step 9: Commit**: `git add api config/crd webhooks/crd_validation_test.go && git commit -m "feat(api): add tasks to TinkerbellMachine and TinkerbellCluster"`

### Task 2: Webhook warnings and exclusivity

**Files:**
- Create: `webhooks/tasks.go`
- Modify: `webhooks/tinkerbellmachine_webhook.go`, `webhooks/tinkerbellmachinetemplate_webhook.go`, `webhooks/tinkerbellcluster_webhook.go`, `webhooks/tinkerbellclustertemplate_webhook.go`
- Create: `webhooks/tasks_test.go`

**Interfaces:**
- Consumes: `infrastructurev1.MachineTask`, `infrastructurev1.ObjectRef` (Task 1).
- Produces (package `webhooks`, unexported):
  ```go
  func validateSource(path *field.Path, tasks []infrastructurev1.MachineTask, templateInline string, templateRef *infrastructurev1.ObjectRef) field.ErrorList
  func deprecationWarnings(path *field.Path, templateInline string, templateRef *infrastructurev1.ObjectRef) admission.Warnings
  ```
  - `validateSource` returns `field.Forbidden(path.Child("tasks"), "at most one of tasks, templateInline and templateRef may be set")` when more than one source is set.
  - It returns `field.Invalid(path.Child("tasks").Index(i), name, "exactly one of taskRef and taskInline must be set")` per bad entry.
  - It returns `field.Duplicate(path.Child("tasks").Index(i).Child("name"), name)` for repeated names.
  - `deprecationWarnings` yields `"<path>.templateInline is deprecated; use <path>.tasks"` and the same for `templateRef`.
- Wiring:
  - The Machine checks path `spec`. The MachineTemplate checks `spec.template.spec`.
  - The Cluster checks `spec`. The ClusterTemplate checks `spec.template.spec`.
  - Create and Update return the warnings and merge the errors into the existing
    aggregates. Template updates keep calling `validateTemplateSpecImmutable` as well.

- [ ] **Step 1: Write the failing tests** `webhooks/tasks_test.go`:
  - `Test_validateSource`: a table of no source, tasks only, templateInline only, tasks +
    templateInline, tasks + templateRef, an entry with both refs, an entry with neither, and
    a duplicate name. Assert each case's error count and types (`field.ErrorTypeForbidden`,
    `ErrorTypeInvalid`, `ErrorTypeDuplicate`).
  - `Test_deprecationWarnings`: expects exactly
    `[]string{"spec.templateInline is deprecated; use spec.tasks"}` for a templateInline
    machine, and nil for tasks.
  - `Test_webhooks_warn_on_deprecated_fields`: calls `ValidateCreate` on all four webhooks
    with `templateInline` set and asserts one warning and no error each. The template kinds
    use path `spec.template.spec`.
  - `Test_webhooks_reject_two_sources`: calls `ValidateCreate` on all four with `tasks` +
    `templateInline` and asserts an error.
- [ ] **Step 2: Run them and see them fail**: `go test ./webhooks/ -run 'Test_validateSource|Test_deprecationWarnings|Test_webhooks_'`.
  Expected: compile errors.
- [ ] **Step 3: Implement `validateSource` and `deprecationWarnings`** in `webhooks/tasks.go`
  and wire them into the four webhooks.
- [ ] **Step 4: Run the package tests**: `go test ./webhooks/...`. Expected: PASS, existing
  tests included.
- [ ] **Step 5: Commit**: `git add webhooks && git commit -m "feat(webhooks): warn on templateInline and templateRef, and reject two sources"`

### Task 3: Task source resolution and the Hardware annotation

**Files:**
- Create: `controller/machine/tasks.go`
- Modify: `controller/machine/hardware.go` (constant next to `HardwareTemplateOverrideAnnotation`; mark that one `Deprecated:`)
- Modify: `controller/machine/template.go` (`ErrNoTemplateFound` text mentions `tasks`; `templateFromAnnotation` records the `DeprecatedTemplateSource` event when it returns data)
- Create: `controller/machine/tasks_test.go`

**Interfaces:**
- Consumes: `infrastructurev1.MachineTask`, `infrastructurev1.TaskRef` (Task 1).
- Produces:
  ```go
  const HardwareTaskRefsAnnotation = "hardware.tinkerbell.org/capt-task-refs"
  const (
      sourceMachine  = "TinkerbellMachine"
      sourceHardware = "Hardware"
      sourceCluster  = "TinkerbellCluster"
  )
  // hardwareTaskRefs parses the annotation: entries {Name: n, TaskRef: &TaskRef{Name: n, Namespace: hw.Namespace}}.
  func hardwareTaskRefs(hw *tinkv2.Hardware) []infrastructurev1.MachineTask
  // taskSource returns the machine's Task list and where it came from, or (nil, "") when the
  // machine's source is a template (or there is none) and the existing template path applies.
  func (scope *machineReconcileScope) taskSource(hw *tinkv2.Hardware) ([]infrastructurev1.MachineTask, string)
  ```
- Precedence in `taskSource`, levels walked in order, first level with any source decides:
  1. machine (`Tasks` → return; `TemplateInline`/`TemplateRef` → nil);
  2. Hardware (task annotation → return, and if the template annotation is also set,
     record `ConflictingHardwareAnnotations`; template annotation alone → nil);
  3. cluster (`Tasks` → return; template fields → nil);
  4. otherwise nil.

- [ ] **Step 1: Write the failing tests** in `controller/machine/tasks_test.go`:
  - `Test_hardwareTaskRefs`: `"a"` → `[a]`; `" a, ,b,"` → `[a b]`; `"a,a"` → `[a a]`;
    absent or `""` → nil. Each entry's `TaskRef.Namespace` == the Hardware's namespace and
    `Name` == `TaskRef.Name`.
  - `Test_taskSource`: a table over machine/Hardware/cluster settings asserting the
    `(len(tasks), origin)` result:
    - machine tasks beat cluster tasks → `sourceMachine`;
    - machine templateInline beats cluster tasks → `(0, "")`;
    - Hardware task annotation beats cluster templateInline → `sourceHardware`;
    - the Hardware template annotation beats cluster tasks → `(0, "")`;
    - cluster tasks alone → `sourceCluster`;
    - nothing → `(0, "")`;
    - both Hardware annotations → `sourceHardware`.
- [ ] **Step 2: Run them and see them fail**: `go test ./controller/machine/ -run 'Test_hardwareTaskRefs|Test_taskSource'`. Expected: compile errors.
- [ ] **Step 3: Implement** the constant, `hardwareTaskRefs` and `taskSource`, plus the two
  `template.go` touches.
- [ ] **Step 4: Run the package tests**: `go test ./controller/machine/...`. Expected: PASS,
  existing template-path tests included.
- [ ] **Step 5: Commit**: `git add controller/machine && git commit -m "feat(machine): resolve Task sources and the capt-task-refs annotation"`

### Task 4: Boot options for Task lists

**Files:**
- Modify: `controller/machine/tasks.go`
- Modify: `controller/machine/tasks_test.go`

**Interfaces:**
- Consumes: `scope.newWorkflow(hw)` and `templatedBootOptions(wf)` (existing, `workflow.go`); `infrastructurev1.MachineTask` (Task 1).
- Produces:
  ```go
  var ErrTemplatedCustombootConfig = errors.New("bootOptions.custombootConfig contains template expressions, which a Workflow running Tasks renders with v1alpha2 data; set bootOptions on the tasks entries instead")
  // taskBootOptions returns the boot options of each entry, index-aligned with tasks.
  func (scope *machineReconcileScope) taskBootOptions(hw *tinkv2.Hardware, tasks []infrastructurev1.MachineTask) ([]*tinkv2.BootOptions, error)
  // defaultBootOptions returns today's boot options as v1alpha2, for the first entry.
  func (scope *machineReconcileScope) defaultBootOptions(hw *tinkv2.Hardware) (*tinkv2.BootOptions, error)
  ```
- Rules:
  - **Explicit mode:** if any entry has `BootOptions`, return a deep copy for each entry,
    and nil for entries without. If the machine-level `bootOptions` is non-zero as well,
    record `BootOptionsIgnored`.
  - **Default mode:** otherwise the first element is `defaultBootOptions(hw)` and the rest
    are nil.
  - `defaultBootOptions` takes `newWorkflow(hw).Spec.Tasks[0].Hardware.BootOptions`. That
    already does the BMC check, the ISO per-MAC rewrite and the `iso`→`isoboot` mapping via
    the conversion package.
  - If `templatedBootOptions` is true on that Workflow, `defaultBootOptions` records
    `TemplatedCustombootConfig` and returns `ErrTemplatedCustombootConfig`.

- [ ] **Step 1: Write the failing tests** in `tasks_test.go`, using `workflowTestHardware`
  and `workflowTestScope` from `workflow_test.go`:
  - `Test_taskBootOptions_explicit_are_copied_verbatim`: three entries, boot options on
    entries 0 and 2 only (entry 2: `BootMode: customboot` with one `PostOperations`). Expect
    `[bo0, nil, bo2]`, equal by `cmp.Diff`, with no `toggleNetboot` added. Same result on
    Hardware without a BMC.
  - `Test_taskBootOptions_default_goes_on_the_first_entry`: two entries without boot options,
    machine `bootOptions.bootMode: netboot`, Hardware with a BMC. Expect
    `[{ToggleNetboot: true, BootMode: netboot}, nil]`. Without a BMC, expect
    `[{ToggleNetboot: true}, nil]`.
  - `Test_taskBootOptions_default_iso`: machine `bootMode: iso`, `isoURL:
    http://smee:7171/iso/hook.iso`, Hardware with a BMC. Expect `BootMode: isoboot` and
    `ISOURL: http://smee:7171/iso/52-54-00-00-00-01/hook.iso`.
  - `Test_taskBootOptions_default_refuses_templated_customboot`: `custombootConfig` with a
    virtual media URL containing `{{ .Hardware.Disks }}`. Expect
    `errors.Is(err, ErrTemplatedCustombootConfig)`.
- [ ] **Step 2: Run them and see them fail**: `go test ./controller/machine/ -run Test_taskBootOptions`. Expected: compile errors.
- [ ] **Step 3: Implement** `taskBootOptions` and `defaultBootOptions`.
- [ ] **Step 4: Run them and see them pass**: `go test ./controller/machine/ -run Test_taskBootOptions`. Expected: PASS.
- [ ] **Step 5: Commit**: `git commit -am "feat(machine): per-entry and default boot options for Task lists"`

### Task 5: Build the Workflow for Task lists, and wire it in

**Files:**
- Modify: `controller/machine/tasks.go`
- Modify: `controller/machine/workflow.go` (`createWorkflow`: Task branch first)
- Modify: `controller/machine/scope.go` (`reconcile`: render failures)
- Modify: `controller/machine/tasks_test.go`, `controller/machine/envtest_test.go`

**Interfaces:**
- Consumes: `taskSource` (Task 3), `taskBootOptions` (Task 4), and the existing
  `taskConflict`, `ensureTasks`, `setResourceOwnership`, `hardwareLookupID`,
  `workflowTemplateData`, `ErrHardwareMissingDiskConfiguration`.
- Produces:
  ```go
  var ErrTaskNotFound = errors.New("task not found")
  var ErrTaskConflict = errors.New("task belongs to something else")
  var errWorkflowNotRendered = errors.New("workflow tasks did not render")
  // createTaskWorkflow creates the machine's inline Tasks and its native Workflow running tasks.
  func (scope *machineReconcileScope) createTaskWorkflow(hw *tinkv2.Hardware, tasks []infrastructurev1.MachineTask, origin string) error
  ```
- Order inside `createTaskWorkflow`. Nothing is created before steps 1–4 pass:
  1. Hardware without storage devices → `ErrHardwareMissingDiskConfiguration`.
  2. Get every `taskRef` Task through `scope.tinkerbellClient` (namespace default
     `tinkerbellNamespace()`). NotFound → record `TaskNotFound` naming `<ns>/<name>` and
     return `fmt.Errorf("%w: %s/%s", ErrTaskNotFound, ns, name)`.
  3. Build the inline Tasks and set ownership. `taskConflict` returns a non-empty reason →
     record `TaskConflict` and return `fmt.Errorf("%w: %s", ErrTaskConflict, reason)`.
  4. `taskBootOptions`.
  5. `ensureTasks(inline)`. This also removes the machine's other owned Tasks; referenced
     Tasks carry no labels.
  6. Create the Workflow:
     - name `tinkerbellMachine.Name`, namespace `tinkerbellNamespace()`, ownership set;
     - `Spec.Globals = &Extra{TemplateMap: workflowTemplateData(hw)}`;
     - one `WorkflowTask` per entry, in order:
       - `AgentID: hardwareLookupID(hw)`;
       - `TaskRef` (inline → `{Name: <machine>-<name>, Namespace: tinkerbellNamespace()}`);
       - `Hardware: &WorkflowHardware{HardwareRef: &SimpleReference{Name: hw.Name, Namespace: hw.Namespace}, BootOptions: bo[i]}`;
     - no compatibility annotations.
  7. Log `"created Workflow running v1alpha2 Tasks"` with the origin.
- `createWorkflow`:
  - Starts with `if tasks, origin := scope.taskSource(hw); len(tasks) > 0 { return scope.createTaskWorkflow(hw, tasks, origin) }`.
  - Everything after that line is unchanged, so a stale machine Template never affects a
    Task source.
- `reconcile`: when the Workflow state is 0 and
  `wf.Status.HasCondition(tinkv2.TemplateRenderedSuccess, metav1.ConditionFalse)`, record
  `WorkflowNotRendered` with the condition's message and return `fmt.Errorf("%w: %s",
  errWorkflowNotRendered, msg)`.

- [ ] **Step 1: Write the failing tests** in `tasks_test.go` (fake client via `workflowTestScope`):
  - `Test_createWorkflow_runs_inline_tasks`:
    - machine `Tasks = [{name: install, taskInline: <1 action>}]`;
    - expect the Task `machine-1-install` to exist with `expectOwnedByMachine` and its
      spec equal to the inline spec;
    - expect the Workflow to have one task with
      `TaskRef{machine-1-install, tink}`, `AgentID: wfAgent` and HardwareRef `hw-1`;
    - expect `Globals.TemplateMap["device_1"] == wfAgent`;
    - expect no `AnnotationCompatibilityMode` and no Template.
  - `Test_createWorkflow_runs_a_mixed_list_in_order`:
    - `[inline a, ref shared, inline b]`, with Task `shared` pre-created with no labels;
    - expect TaskRefs `[machine-1-a, shared, machine-1-b]`;
    - after `scope.removeDependencies()`, expect `shared` to still exist unchanged and both
      inline Tasks to be gone.
  - `Test_createTaskWorkflow_missing_ref_creates_nothing`: `[inline a, ref missing]`.
    Expect `errors.Is(err, ErrTaskNotFound)`, and no Workflow and no Task after the call.
  - `Test_createTaskWorkflow_conflicting_task`: a Task `machine-1-a` exists without
    ownership labels. Expect `errors.Is(err, ErrTaskConflict)` and no Workflow.
  - `Test_createWorkflow_tasks_ignore_a_stale_template`: a v1alpha1 Template named
    `machine-1` exists and the machine has tasks. Expect a native Workflow with no
    compatibility annotation.
  - `Test_createWorkflow_tasks_from_the_cluster_and_annotation`:
    - cluster tasks → Workflow TaskRefs use `machine-1-<name>`;
    - Hardware annotation `capt-task-refs: shared` with Task `shared` existing → TaskRef
      `shared` in the Hardware namespace, and the first task gets default boot options.
  - `Test_createTaskWorkflow_uses_the_tinkerbell_client`: the scope gets two separate fake
    clients (`client` and `tinkerbellClient`) with `externalTinkerbell: true`. Expect the
    Task and the Workflow in `tinkerbellClient`, and none in `client`.
  - `Test_reconcile_reports_a_workflow_that_does_not_render`: a Workflow at state 0 with
    condition `TemplateRenderedSuccess=False, Message: "boom"`. Expect
    `scope.reconcile(hw)` to return an error wrapping `errWorkflowNotRendered` and
    containing `boom`.
- [ ] **Step 2: Run them and see them fail**: `go test ./controller/machine/ -run 'Test_createWorkflow_|Test_createTaskWorkflow_|Test_reconcile_reports'`. Expected: compile errors or failures.
- [ ] **Step 3: Implement** `createTaskWorkflow`, the `createWorkflow` branch and the
  `reconcile` render check.
- [ ] **Step 4: Add an envtest case.** Add `"tasks": testTaskWorkflowCRDs` to
  `TestTinkerbellCRDs` in `envtest_test.go`. It creates a machine with
  `[inline a (bootOptions toggleNetboot+netboot), ref shared (bootOptions customboot with postOperations)]`
  against the real Tinkerbell v0.28.6 CRDs (pre-create `shared`), and expects
  `createWorkflow` to succeed and the Workflow to read back with both boot options intact.
- [ ] **Step 5: Run the whole CAPT suite**: with `KUBEBUILDER_ASSETS` and
  `TINKERBELL_CRD_DIR` set, `go test ./...` and `GOTOOLCHAIN=go1.26.8 make lint`. Expected:
  PASS, lint 0 issues.
- [ ] **Step 6: Commit**: `git add controller && git commit -m "feat(machine): provision from tasks with a native v1alpha2 Workflow"`

### Task 6: CABPT in-place allowlist

**Files** (CABPT worktree):
- Modify: `internal/inplace/policy.go` (TinkerbellMachine paths and comment)
- Modify: `internal/inplace/canupdate_test.go`

**Interfaces:**
- TinkerbellMachine paths become exactly: `spec.templateInline`, `spec.templateRef`,
  `spec.tasks`, `spec.hardwareAffinity`, `spec.bootOptions`.
- Comment:
  - Drop the "bootMode … not inert" exclusion line. Say CAPT reads `bootOptions` and `tasks`
    only when it creates a Workflow.
  - Keep the exclusions of `spec.hardwareName` and `spec.providerID`, and add
    `spec.addressFromPool` (immutable once Hardware is selected).

- [ ] **Step 1: Create the worktree**: `git -C /home/appkins/src/sidero-community/cluster-api-bootstrap-provider-talos worktree add -b feat/tinkerbell-task-fields ../cluster-api-bootstrap-provider-talos.tasks main`
- [ ] **Step 2: Update the tests first** in `canupdate_test.go`:
  - In `TestCanUpdateMachine_InfrastructureMachine`:
    - rename "boot mode change is declined" to "boot mode change is absorbed" and drop
      `declines`;
    - add "tasks change is absorbed": `tasks: [{name: a, taskRef: {name: x}}]` →
      `[{name: a, taskRef: {name: y}, bootOptions: {toggleNetboot: true}}]`;
    - add "address pool change is declined": `addressFromPool: {name: p1}` → `{name: p2}`,
      `declines: true`.
  - In `TestCanUpdateMachineSet_InfrastructureMachineTemplate`:
    - flip "boot mode change is declined" and "tink template change alongside boot mode
      change is declined" to absorbed;
    - add a tasks case and an `addressFromPool` declined case for
      `TinkerbellMachineTemplate`.
- [ ] **Step 3: Run them and see them fail**: `go test ./internal/inplace/ -run 'TestCanUpdateMachine_InfrastructureMachine|TestCanUpdateMachineSet_InfrastructureMachineTemplate'`.
  Expected: the flipped and tasks cases FAIL.
- [ ] **Step 4: Update `policy.go`.**
- [ ] **Step 5: Run the tests**: `go test ./internal/inplace/...`. Expected: PASS.
- [ ] **Step 6: Commit** (CABPT worktree): `git commit -am "feat(inplace): absorb TinkerbellMachine tasks and boot options in place"`

### Task 7: Docs, template, and an end-to-end run on kind

**Files** (CAPT worktree):
- Modify: `templates/cluster-template.yaml` (the TinkerbellCluster comment points at `tasks`; worker `TinkerbellMachineTemplate` comment shows where `tasks` goes)
- Modify: `docs/QUICK-START.md`:
  - the template section becomes "Tasks", with a `tasks` example (one `taskInline` with
    explicit boot options, one `taskRef`);
  - the resolution order lists `tasks`/`capt-task-refs` beside the deprecated fields;
  - a "From templateInline to tasks" subsection: the syntax mapping from the spec
    (`{{ index .Hardware.Disks 0 }}` → `{{ (index .hardware.spec.storageDevices 0).name }}`;
    `.device_1` → not needed, or `.workflow.spec.globals.templateMap.device_1`);
  - the two boot-option modes, and the fact that existing objects keep working.
- Modify: `docs/COMPATIBILITY.md` (the Task CRD row mentions references; the v1beta2 field list adds `tasks`; deprecation note)

- [ ] **Step 1: Write the docs and template changes.**
- [ ] **Step 2: Render check**: `clusterctl generate yaml --from templates/cluster-template.yaml --list-variables` (if clusterctl is installed) or `yq` parse each document. Expected: it parses.
- [ ] **Step 3: Commit**: `git commit -am "docs: describe tasks and deprecate templateInline and templateRef"`
- [ ] **Step 4: Bring up kind.**
  - `kind create cluster --name capt-tasks --kubeconfig $SCRATCH/e2e/kubeconfig`.
  - Define a shell guard that refuses any context other than `kind-capt-tasks`, and pass
    `--kubeconfig $SCRATCH/e2e/kubeconfig` on every call.
  - Install the Tinkerbell chart `oci://ghcr.io/tinkerbell-community/charts/tinkerbell`
    `v0.28.6`, with `trustedProxies` set via `deployment.envs.globals`.
- [ ] **Step 5: Install CAPI and CAPT.**
  - `clusterctl init --core cluster-api --bootstrap - --control-plane -` against the kind
    kubeconfig (core only; installs cert-manager).
  - Build the CAPT image from the worktree (`docker build -t capt:tasks .`) and
    `kind load docker-image capt:tasks --name capt-tasks`.
  - Apply `kustomize build config/default` with the image set to `capt:tasks`.
- [ ] **Step 6: Create the test objects.**
  - Two Hardware objects (MACs `52:54:00:00:00:a1`/`a2`, a storage device, no BMC).
  - A Task `shared` (one `alpine` action `echo shared`).
  - A Cluster + TinkerbellCluster with no source.
  - Two Machines + TinkerbellMachines with `bootstrap.dataSecretName` pointing at a dummy
    Secret:
    - **m1** has `tasks: [{name: one, taskInline: <alpine echo one>}]`, default boot options;
    - **m2** has `tasks: [{name: one, taskInline: <alpine echo two>, bootOptions: {toggleNetboot: true}}, {name: two, taskRef: {name: shared}}]`.
  - Run two agents on host docker:
    `docker run -d --network kind -v /var/run/docker.sock:/var/run/docker.sock ghcr.io/tinkerbell-community/tink-agent:v0.28.6 -id <mac> -transport grpc -runtime docker -grpc-server <nodeIP>:<tink-server nodePort> -attribute-detection=false`.
- [ ] **Step 7: Check the result.**
  - Both Workflows reach `Success`. m2's Workflow lists TaskRefs
    `[m2-one, shared]` with boot options only on task 0.
  - Both Hardware get `v1alpha1.tinkerbell.org/provisioned=true`, and both
    TinkerbellMachines are Ready.
  - The agent logs show `one`, `two` and `shared`.
  - `kubectl get events` shows no unexpected Warnings.
  - A TinkerbellMachine applied with `templateInline` returns the deprecation warning.
  - Deleting m2 removes `m2-one` and leaves `shared`.
- [ ] **Step 8: Clean up**: `kind delete cluster --name capt-tasks` and stop the agent
  containers. Record anything surprising in the hand-off.
