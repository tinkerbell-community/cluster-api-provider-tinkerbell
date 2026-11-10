# Workflow References and Vars Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A TinkerbellMachine names objects relative to its machine in `references`, CAPT
puts them on the machine's Workflow, and `vars` carries their data to the Tasks.

**Architecture:**

- **Rendering package:** a new package, `pkg/workflowref`, owns the machine variables and the
  reference templates. Both the admission webhooks and the controller use it, so they always
  agree on the variable set.
- **Webhooks:** they check templates at admission.
- **Controller:** `createTaskWorkflow` renders the references before anything is created, then
  writes `spec.references`, `spec.vars` and the extra `globals.templateMap` entries on the
  Workflow.
- **Tinkerbell:** unchanged. It renders `vars` with the Workflow's references, and Tasks read them
  as `.workflow.spec.vars`.
- **CABPT:** its in-place allowlist gains the two fields.

**Tech Stack:** Go 1.26, Go `text/template`, controller-runtime, kubebuilder markers/CEL,
envtest 1.37, gomega (CAPT), testify (CABPT), kind.

**Spec:** `docs/superpowers/specs/2026-11-09-workflow-references-design.md`

## Global Constraints

**Where the work happens:**

- **CAPT:** worktree `/home/appkins/src/tinkerbell-community/cluster-api-provider-tinkerbell.references`,
  branch `feat/workflow-references`, created from `main` at 598a0e7.
- **CABPT:** create worktree `/home/appkins/src/sidero-community/cluster-api-bootstrap-provider-talos.references`,
  branch `feat/tinkerbell-workflow-references`, from `main` at f89b362.
- **Main checkouts:** never stash or commit in the main CAPT, CABPT or Tinkerbell checkouts.
  Other sessions use them.
- **Publishing:** commit locally on the feature branches. Don't push, tag or release; that needs
  the user's say-so.

**Clusters:** never run kubectl, helm or clusterctl against `admin@talos` or any context other
than the kind cluster created in Task 6. Always pass that cluster's own `--kubeconfig`.

**Names, exactly as written:**

- **Imports:** Tinkerbell types come from `github.com/tinkerbell/tinkerbell/api`, which is replaced
  by `github.com/tinkerbell-community/tinkerbell/api v0.28.0`. Import aliases as in the code:
  `tinkv2` (v1alpha2/tinkerbell), `infrastructurev1` (CAPT api/v1beta2), `clusterv1`
  (`sigs.k8s.io/cluster-api/api/core/v1beta2`).
- **Fields:**
  - `references`, a `map[string]WorkflowReference` holding `group`, `version`, `resource`,
    `name` and `namespace`;
  - `vars`, a `map[string]string`.
- **Variable names:** `machine_name`, `machine_namespace`, `cluster_name`,
  `bootstrap_config_name`, `tinkerbell_machine_name`, `hardware_name`, `hardware_namespace`.
- **CEL message:** `references and vars apply to tasks and cannot be set with templateInline or templateRef`.
- **Event reasons, all Warning:** `ReferenceNotRendered`, `ReferenceOutsideNamespace`,
  `ReferencesUnsupported`, `ReferencesIgnored`. Emit them with `scope.warnf`.
- **Errors:**
  - `workflowref.ErrNotRendered` and `workflowref.ErrOutsideNamespace`;
  - `machine.ErrReferenceNotRendered` and `machine.ErrReferencesUnsupported`.

**Commands:**

- **Lint:** `GOTOOLCHAIN=go1.26.8 make lint` in CAPT.
- **envtest:** `export KUBEBUILDER_ASSETS=$HOME/.local/share/kubebuilder-envtest/k8s/1.37.0-linux-amd64`.
- **Tinkerbell CRDs for `TestTinkerbellCRDs`:**
  - `SCRATCH=/tmp/claude-1000/-home-appkins-src-sidero-community-cluster-api-bootstrap-provider-talos/dd1fdd4a-ea3d-4bd7-97b4-ef7929803f42/scratchpad`;
  - if `$SCRATCH/tink-crds/crd/bases` is missing, run
    `mkdir -p $SCRATCH/tink-crds && git -C /home/appkins/src/tinkerbell-community/tinkerbell archive v0.28.6 crd/bases | tar -x -C $SCRATCH/tink-crds`;
  - `export TINKERBELL_CRD_DIR=$SCRATCH/tink-crds/crd/bases`.
- **zsh:** don't use `echo ===` (zsh treats `=word` as a command lookup). Use `echo ---`.

## Review Focus

1. **The variable set must be the same at admission and at runtime.** If the webhook's sample
   data lacks a variable `Variables` produces, valid machines are rejected. If it has one
   `Variables` lacks, typos pass admission and fail at runtime. *(Task 2:
   `TestSampleHasEveryVariable`)*
2. **The machine's namespace differs from the Hardware's.** The Workflow lives in the Hardware's
   namespace, but references must default to, and be limited to, the machine's namespace
   (`TinkerbellMachine.Namespace`), not `tinkerbellNamespace()`. *(Task 4:
   `Test_references_use_the_machine_namespace_not_the_workflow_namespace`)*
3. **A templated namespace that passes admission but is refused at runtime.**
   `{{ .hardware_namespace }}` is accepted when created. It must be refused with
   `ReferenceOutsideNamespace`, and nothing created, when the Hardware's namespace differs from
   the machine's. *(Task 4: same test)*
4. **Partial creation.** A reference failure must leave no inline Task behind, because the
   references are checked before `ensureTasks`. *(Task 4:
   `Test_createTaskWorkflow_reference_problems_create_nothing`, with an inline entry)*
5. **v1beta1 writers.** A v1beta1 update of a machine that has `references` must keep them. A
   v1beta1 write that restores them next to a `templateOverride` must be rejected. *(Task 1:
   `TestReferencesAndVarsSurviveRoundTrip`; Task 3:
   `Test_webhook_rejects_a_v1beta1_write_restoring_references_next_to_a_template`)*

---

### Task 1: API types, CEL rule, generated code and v1beta1 conversion

**Files** (CAPT worktree):
- Modify: `api/v1beta2/types.go`: add `WorkflowReference` after `TaskRef`.
- Modify: `api/v1beta2/tinkerbellmachine.go`:
  - add the `References` and `Vars` fields to `TinkerbellMachineConfig`, after `Tasks`;
  - add the second XValidation rule on the struct, next to the existing one at line 45.
- Generated (`make generate`): `api/v1beta2/zz_generated.deepcopy.go`, and
  `config/crd/bases/infrastructure.cluster.x-k8s.io_tinkerbellmachines.yaml` and
  `..._tinkerbellmachinetemplates.yaml`.
- Modify: `api/v1beta1/conversion.go`: in `ConvertMachineToHub` (around line 151) and
  `ConvertMachineTemplateToHub` (around line 204), restore `References` and `Vars` next to
  `Tasks`. Update the "v1beta2-only" comments in both `FromHub` functions.
- Test: `api/v1beta1/conversion_test.go`, `webhooks/crd_validation_test.go`.

**Interfaces:**
- Produces: `infrastructurev1.WorkflowReference{Group, Version, Resource, Name, Namespace string}`,
  with the field docs and markers exactly as in the spec's API section. Also
  `TinkerbellMachineConfig.References map[string]WorkflowReference` and
  `TinkerbellMachineConfig.Vars map[string]string`, with `MaxProperties` 16 and 32.

- [ ] **Step 1: Write the failing tests.**
  - **`TestReferencesAndVarsSurviveRoundTrip`** in `conversion_test.go`, modelled on
    `TestTasksSurviveRoundTrip`. A hub TinkerbellMachine and a TinkerbellMachineTemplate, each
    with `References: {"talos": {Group: "bootstrap.cluster.x-k8s.io", Version: "v1beta1", Resource: "talosconfigs", Name: "{{ .bootstrap_config_name }}", Namespace: "{{ .machine_namespace }}"}}`
    and `Vars: {"schematic_id": "{{ .references.talos.status.imageFactory.schematicID }}"}`, go
    through `FromHub` and then `ToHub`. Expect `References` and `Vars` to be equal to the
    originals.
  - **CRD validation cases** in `TestCRDValidation`:
    - a TinkerbellMachine with `References` and `TemplateInline: "t"` is rejected with the CEL
      message;
    - the same with `Vars` and a `TemplateRef` is rejected;
    - `References` with `Tasks` is accepted;
    - a reference missing `version` is rejected;
    - 17 references are rejected.
- [ ] **Step 2: Run them and see them fail.**
  - Run `go test ./api/... -run TestReferencesAndVarsSurviveRoundTrip`, then
    `go test ./webhooks/ -run TestCRDValidation` with `KUBEBUILDER_ASSETS` set.
  - Expected: compile errors (`References` undefined).
- [ ] **Step 3: Add the types, fields, rule and conversion restore.**
- [ ] **Step 4: Regenerate.**
  - Run `make generate`.
  - Expected: the deepcopy file and two CRDs change, and `git status` shows nothing else
    unexpected.
- [ ] **Step 5: Run the tests.**
  - Run `go test ./api/...`, which includes `TestFuzzyConversion`, and
    `go test ./webhooks/ -run TestCRDValidation`.
  - Expected: PASS.
- [ ] **Step 6: Commit.**
  `git add -A api config && git commit -m "feat(api): add references and vars to TinkerbellMachineConfig"`

### Task 2: The `pkg/workflowref` rendering package

**Files:**
- Create: `pkg/workflowref/workflowref.go`
- Test: `pkg/workflowref/workflowref_test.go`

**Interfaces:**
- Consumes: `infrastructurev1.WorkflowReference` (Task 1).
- Produces, in package `workflowref`, import path `github.com/tinkerbell/cluster-api-provider-tinkerbell/pkg/workflowref`:
  - **Variable name constants:** `MachineName`, `MachineNamespace`, `ClusterName`,
    `BootstrapConfigName`, `TinkerbellMachineName`, `HardwareName`, `HardwareNamespace`. Their
    values are the variable names in Global Constraints.
  - **`func Names() []string`:** the variable names, sorted.
  - **`func Variables(machine *clusterv1.Machine, tm *infrastructurev1.TinkerbellMachine, hw *tinkv2.Hardware) map[string]string`**:
    - `machine_namespace` comes from `tm.Namespace`. It always equals the Machine's, and
      using it keeps the namespace limit independent of `machine`.
    - `tinkerbell_machine_name` comes from `tm.Name`.
    - `hardware_name` and `hardware_namespace` come from `hw`.
    - `machine_name`, `cluster_name` (`Spec.ClusterName`) and `bootstrap_config_name`
      (`Spec.Bootstrap.ConfigRef.Name`) come from `machine`, and are `""` when `machine` is
      nil.
  - **`func Sample() map[string]string`:** every name in `Names()` mapped to `"x"`, for checks
    at admission.
  - **`func Execute(text string, vars map[string]string) (string, error)`:** `text/template`
    named `reference`, with `Option("missingkey=error")` and no added functions.
  - **`var ErrNotRendered`, `var ErrOutsideNamespace`.**
  - **`func Render(ref infrastructurev1.WorkflowReference, vars map[string]string) (tinkv2.Reference, error)`**:
    - it renders `Name` and `Namespace` with `Execute` and copies `Group`, `Version` and
      `Resource` as they are;
    - a template error, an empty name, or a name that fails
      `validation.IsDNS1123Subdomain` (`k8s.io/apimachinery/pkg/util/validation`) returns an
      error wrapping `ErrNotRendered`, saying which field;
    - an empty rendered namespace becomes `vars[MachineNamespace]`;
    - any other value than `vars[MachineNamespace]` returns an error wrapping
      `ErrOutsideNamespace`, naming both namespaces.

- [ ] **Step 1: Write the failing tests.**
  - **`TestVariables`:** a Machine `m1/capi` with `ClusterName: "c1"` and
    `Bootstrap.ConfigRef.Name: "m1-config"`, a TinkerbellMachine `tm1/capi`, and a Hardware
    `hw1/tink`. Expect exactly the seven keys and values. With a nil Machine, the three
    Machine-derived values are `""` and `machine_namespace` is still `capi`.
  - **`TestSampleHasEveryVariable`:** the keys of `Sample()` equal the keys of `Variables(...)`
    and `Names()`.
  - **`TestExecute`:**
    - `'{{ .machine_name }}-talos'` becomes `m1-talos`;
    - `'{{ .machine_namespcae }}'` returns an error containing `machine_namespcae`;
    - `'{{ .machine_name'` returns a parse error;
    - literal text is returned unchanged.
  - **`TestRender`**, table-driven:
    - name `'{{ .bootstrap_config_name }}'` with namespace `'{{ .machine_namespace }}'` renders
      to `{..., Name: "m1-config", Namespace: "capi"}`, with group, version and resource kept;
    - an empty namespace defaults to `capi`;
    - namespace `other` returns `ErrOutsideNamespace`;
    - namespace `'{{ .hardware_namespace }}'` returns `ErrOutsideNamespace`, because the
      Hardware is in `tink`;
    - an empty `bootstrap_config_name` returns `ErrNotRendered`;
    - name `'{{ .machine_name }}_x'` (not DNS-1123) returns `ErrNotRendered`;
    - an unknown variable returns `ErrNotRendered`.
- [ ] **Step 2: Run them and see them fail.**
  - Run `go test ./pkg/workflowref/`.
  - Expected: compile errors.
- [ ] **Step 3: Implement `workflowref.go`.**
- [ ] **Step 4: Run them and see them pass.**
  - Run `go test ./pkg/workflowref/`.
  - Expected: PASS.
- [ ] **Step 5: Commit.**
  `git add pkg/workflowref && git commit -m "feat: render workflow references with machine variables"`

### Task 3: Admission checks

**Files:**
- Create: `webhooks/references.go`
- Modify: `webhooks/tinkerbellmachine_webhook.go`:
  - `validateMachineSpec` becomes `validateMachineSpec(namespace string, spec *infrastructurev1.TinkerbellMachineSpec)`;
  - both callers pass `obj.Namespace` (or `newTM.Namespace`).
- Modify: `webhooks/tinkerbellmachinetemplate_webhook.go`: `validateMachineTemplateSource` adds
  `validateReferences(path, tmt.Namespace, &spec)`.
- Test: `webhooks/references_test.go`

**Interfaces:**
- Consumes: `workflowref.Execute`, `workflowref.Sample`, `workflowref.Names` (Task 2).
- Produces: `func validateReferences(path *field.Path, namespace string, config *infrastructurev1.TinkerbellMachineConfig) field.ErrorList`.
  It walks the keys in sorted order. For key `k`, at `path.Child("references").Key(k)`:
  - **`group`, `version`, `resource`:** containing `{{` is `field.Invalid`, saying only `name`
    and `namespace` are templates.
  - **`name`, `namespace`:** an error from `Execute(value, Sample())` is `field.Invalid`, with
    the error followed by `; known variables: ` and `Names()` joined with `, `.
  - **A literal namespace:** a `namespace` with no `{{` that is non-empty and different from
    `namespace` is `field.Forbidden`, with "must be empty or the machine's namespace".
  - **Source:** `references` or `vars` set while `TemplateInline != ""` or `TemplateRef != nil`
    is `field.Forbidden` at `path.Child("references")` (or `vars` when only vars is set), with
    the CEL message.

- [ ] **Step 1: Write the failing tests.**
  - **`Test_validateReferences`**, table-driven with `field.NewPath("spec")` and namespace
    `capi`. Error types expected:
    - a valid reference: none;
    - a template in `group`: Invalid;
    - an unknown variable in `name`: Invalid, and the message contains `machine_namespace`;
    - a parse error in `namespace`: Invalid;
    - literal namespace `other`: Forbidden;
    - literal namespace `capi`: none;
    - `{{ .hardware_namespace }}`: none (checked at runtime);
    - references with `templateInline`: Forbidden;
    - only vars with `templateRef`: Forbidden;
    - vars only: none.
  - **`Test_webhooks_check_references`:** `ValidateCreate` on a TinkerbellMachine and a
    TinkerbellMachineTemplate in namespace `capi`, each with namespace `other`, returns an
    error containing "machine's namespace". Paths: `spec.references[talos].namespace` and
    `spec.template.spec.references[talos].namespace`.
  - **`Test_webhook_rejects_a_v1beta1_write_restoring_references_next_to_a_template`:** like
    the existing tasks test in `tasks_test.go`, with `References` in place of `Tasks`.
- [ ] **Step 2: Run them and see them fail.**
  - Run `go test ./webhooks/ -run 'references|References'`.
  - Expected: compile errors.
- [ ] **Step 3: Implement it and wire it into both webhooks.**
- [ ] **Step 4: Run the package tests.**
  - Run `go test ./webhooks/...`, with `KUBEBUILDER_ASSETS` and `TINKERBELL_CRD_DIR` set.
  - Expected: PASS, existing tests included.
- [ ] **Step 5: Commit.**
  `git add webhooks && git commit -m "feat(webhooks): check workflow reference templates at admission"`

### Task 4: Put references, vars and machine variables on the Workflow

**Files:**
- Create: `controller/machine/references.go`
- Modify: `controller/machine/tasks.go`:
  - `createTaskWorkflow` calls `scope.workflowReferences(hw)` after `taskBootOptions` and
    before `ensureTasks`;
  - `taskWorkflow` gains a `references map[string]tinkv2.Reference` parameter.
- Modify: `controller/machine/workflow.go`: in `createWorkflow`, when `taskSource` returns no
  tasks and the machine sets `References` or `Vars`, record `ReferencesIgnored` once, then
  continue as today.
- Modify: `controller/machine/workflow_test.go`: `workflowTestScope` also sets
  `machine: &clusterv1.Machine{ObjectMeta: {Name: wfMachine, Namespace: wfNamespace}, Spec: {ClusterName: "cluster-1", Bootstrap: {ConfigRef: {APIGroup: "bootstrap.cluster.x-k8s.io", Kind: "TalosConfig", Name: "machine-1-config"}}}}`.
- Test: `controller/machine/references_test.go`

**Interfaces:**
- Consumes: `workflowref.Variables`, `workflowref.Render`, `ErrOutsideNamespace` (Task 2).
- Produces in `controller/machine`:
  - **Reason constants:** `referenceNotRenderedReason`, `referenceOutsideNamespaceReason`,
    `referencesUnsupportedReason`, `referencesIgnoredReason`.
  - **`var ErrReferenceNotRendered`, `var ErrReferencesUnsupported`.**
  - **`func (scope *machineReconcileScope) workflowReferences(hw *tinkv2.Hardware) (map[string]tinkv2.Reference, error)`:**
    - no references returns nil, nil;
    - `scope.isExternal()` records `ReferencesUnsupported` and returns
      `ErrReferencesUnsupported`;
    - otherwise it renders every key in sorted order with
      `workflowref.Render(ref, workflowref.Variables(scope.machine, scope.tinkerbellMachine, hw))`;
    - each failure records `ReferenceOutsideNamespace` when it wraps `ErrOutsideNamespace`,
      and `ReferenceNotRendered` otherwise, with the key and error in the message;
    - any failure returns `fmt.Errorf("%w: %s", ErrReferenceNotRendered, <failed keys joined by ", ">)`.
  - **`taskWorkflow`:**
    - sets `Spec.References` (nil when there are none) and
      `Spec.Vars = maps.Clone(scope.tinkerbellMachine.Spec.Vars)`;
    - sets `Globals.TemplateMap` to `workflowTemplateData(hw)` plus
      `workflowref.Variables(...)`;
    - the template path's `workflowTemplateData` is unchanged.

- [ ] **Step 1: Write the failing tests** (`scope := workflowTestScope(g, "", hw)`, then set
  `scope.tinkerbellMachine.Spec.Tasks`, `.References` and `.Vars`):
  - **`Test_createWorkflow_sets_references_vars_and_machine_variables`:**
    - with talos reference names `'{{ .bootstrap_config_name }}'` and
      `'{{ .machine_namespace }}'`, and vars `{schematic_id: '{{ .references.talos.status.imageFactory.schematicID }}'}`,
      `wf.Spec.References["talos"]` is
      `tinkv2.Reference{Group: "bootstrap.cluster.x-k8s.io", Version: "v1beta1", Resource: "talosconfigs", Name: "machine-1-config", Namespace: wfNamespace}`;
    - `wf.Spec.Vars` equals the vars exactly;
    - `wf.Spec.Globals.TemplateMap` has `device_1`, plus `machine_name=machine-1`,
      `cluster_name=cluster-1`, `bootstrap_config_name=machine-1-config`,
      `tinkerbell_machine_name=machine-1`, `hardware_name=hw-1`, `hardware_namespace=tink`
      and `machine_namespace=tink`.
  - **`Test_createWorkflow_without_references_sets_none`:** tasks only gives nil
    `Spec.References` and nil `Spec.Vars`, and the TemplateMap still has `device_1`.
  - **`Test_createTaskWorkflow_reference_problems_create_nothing`**, table-driven, with an
    inline `tasks` entry so a partial creation would show. Each case returns
    `ErrReferenceNotRendered`, creates no Workflow, gives `listTasks` length 0, and records
    the reason given:
    - an empty `bootstrap_config_name` (`scope.machine.Spec.Bootstrap.ConfigRef = clusterv1.ContractVersionedObjectReference{}`):
      `ReferenceNotRendered`;
    - name `'{{ .machine_name }}_x'`: `ReferenceNotRendered`;
    - an unknown variable: `ReferenceNotRendered`;
    - literal namespace `other`: `ReferenceOutsideNamespace`.
  - **`Test_references_use_the_machine_namespace_not_the_workflow_namespace`:**
    - set `scope.tinkerbellMachine.Namespace = "capi"` and `scope.machine.Namespace = "capi"`,
      keeping `TargetNamespace` and the Hardware in `tink`;
    - an empty namespace renders `capi`;
    - `'{{ .hardware_namespace }}'` records `ReferenceOutsideNamespace` and creates nothing.
    - This mirrors a Machine and its Hardware in different namespaces. `setResourceOwnership`
      already skips the owner reference across namespaces and keeps the labels.
  - **`Test_createTaskWorkflow_external_tinkerbell`:** with `scope.externalTinkerbell = true`:
    - references give `ErrReferencesUnsupported` and `ReferencesUnsupported`, and nothing is
      created;
    - vars alone succeed and are on the Workflow.
  - **`Test_createWorkflow_template_source_ignores_references`:**
    - `scope := workflowTestScope(g, "", hw)`, with the cluster's
      `TemplateInline = talosTemplate`, and the machine with references and no tasks;
    - the Workflow is created as before, with empty `Spec.References`;
    - `ReferencesIgnored` is recorded (`recordedEvents`).
- [ ] **Step 2: Run them and see them fail.**
  - Run `go test ./controller/machine/ -run 'references|References|Test_createWorkflow_sets|Test_createWorkflow_without'`.
  - Expected: compile errors or failures.
- [ ] **Step 3: Implement it.**
- [ ] **Step 4: Run everything.**
  - With `KUBEBUILDER_ASSETS` and `TINKERBELL_CRD_DIR` set, run `go test ./...` and
    `GOTOOLCHAIN=go1.26.8 make lint`.
  - Expected: PASS and no lint findings. A clean `make verify` (generate check) is also
    expected.
- [ ] **Step 5: Commit.**
  `git add -A controller && git commit -m "feat(machine): put rendered references, vars and machine variables on task Workflows"`

### Task 5: CABPT in-place allowlist

**Files** (CABPT worktree, created as in Global Constraints):
- Modify: `internal/inplace/policy.go`: add `"spec.references"` and `"spec.vars"` after
  `"spec.tasks"`. Extend that group's comment to say they are read only when CAPT creates a
  Workflow.
- Test: `internal/inplace/canupdate_test.go`

- [ ] **Step 1: Write the failing tests** in `TestCanUpdateMachine_InfrastructureMachine`:
  - **"references change is absorbed":**
    `current: {"references": {"talos": {"version": "v1beta1", "resource": "talosconfigs", "name": "a"}}}`,
    and `desired` the same with `"name": "b"`.
  - **"vars change is absorbed":** `{"vars": {"k": "a"}}` changed to `{"vars": {"k": "b"}}`.
  - Add the same pair to `TestCanUpdateMachineSet_InfrastructureMachineTemplate` if it lists
    fields by case.
- [ ] **Step 2: Run them and see them fail.**
  - Run `go test ./internal/inplace/ -run 'TestCanUpdateMachine_InfrastructureMachine|TestCanUpdateMachineSet'`.
  - Expected: the new cases FAIL (declined).
- [ ] **Step 3: Add the paths.**
- [ ] **Step 4: Run the package.**
  - Run `go test ./internal/inplace/...`.
  - Expected: PASS.
- [ ] **Step 5: Commit.**
  `git commit -am "feat(inplace): absorb TinkerbellMachine references and vars in place"`

### Task 6: Docs, and one end-to-end run on kind

**Files** (CAPT worktree):
- Modify: `docs/QUICK-START.md`: add the section "Read the machine's objects from Tasks",
  after the tasks section, with everything the spec's Documentation section lists.
  - The main example reads CABPT's `status.imageFactory.schematicID`, `.talosVersion` and
    `.installerImage` into `vars` (`schematic_id`, `talos_version`, `installer_image`).
  - A talos2disk Task passes them as `SCHEMATIC_ID` and `TALOS_VERSION`.
  - Show the `dig` form for clusters without the Image Factory, for example
    `{{ dig "status" "imageFactory" "schematicID" "" .references.talos }}`.
  - For the allow rule, read how `deployment.envs.tinkController.referenceAllowListRules`
    becomes a flag in the Tinkerbell chart at v0.28.6
    (`git -C /home/appkins/src/tinkerbell-community/tinkerbell show v0.28.6:helm/tinkerbell/templates/...`),
    and write the example in that format.
- Modify: `docs/COMPATIBILITY.md`: the v1beta2 TinkerbellMachine field list adds `references`
  and `vars`.

- [ ] **Step 1: Write the docs.** Commit:
  `git commit -am "docs: describe workflow references and vars"`
- [ ] **Step 2: Bring up kind.**
  - `kind create cluster --name capt-refs --kubeconfig $SCRATCH/e2e/kubeconfig`. Define a
    shell guard that refuses any context other than `kind-capt-refs`.
  - Install the Tinkerbell chart `oci://ghcr.io/tinkerbell-community/charts/tinkerbell`
    `v0.28.6`. Set `trustedProxies` through `deployment.envs.globals`, plus one allow rule:
    group `bootstrap.cluster.x-k8s.io`, resource `talosconfigs`.
  - Set `rbac.additionalRoleRules` to `get`, `list` and `watch` on
    `bootstrap.cluster.x-k8s.io/talosconfigs`.
  - Put every test object in the namespace Tinkerbell is installed in, so that it watches
    them.
- [ ] **Step 3: Install Cluster API, the TalosConfig CRD, and CAPT.**
  - `clusterctl init --core cluster-api --bootstrap - --control-plane -` against the kind
    kubeconfig.
  - Apply CABPT's `config/crd/bases/bootstrap.cluster.x-k8s.io_talosconfigs.yaml` from the
    CABPT worktree, with no controller. Add the contract label
    `cluster.x-k8s.io/v1beta2: v1beta1` to the CRD so Cluster API resolves the `configRef`.
  - `docker build -t capt:refs .`, then `kind load docker-image capt:refs --name capt-refs`,
    then apply `kustomize build config/default` with the image set to `capt:refs`.
- [ ] **Step 4: Create the test objects.**
  - **Hardware:** one, with MAC `52:54:00:00:00:b1`, a storage device, no BMC, and
    `networkInterfaces[mac].ipam.ipv4.address`, which CAPT requires.
  - **Task `talos-install`:** one `alpine` action with `timeoutSeconds: 60` and env
    `SCHEMATIC_ID: '{{ .workflow.spec.vars.schematic_id }}'`.
  - **TalosConfig `m1-config`.** Patch its status subresource with:
    - `dataSecretName: m1-bootstrap`;
    - `initialization.dataSecretCreated: true`;
    - `imageFactory.schematicID: <64 hex chars>`, plus the other required `imageFactory`
      fields, as the CRD demands.
  - **Secret `m1-bootstrap`** with a `value` key.
  - **Cluster and TinkerbellCluster `c1`,** with no source.
  - **Machine `m1`,** with `bootstrap.configRef` set to the TalosConfig, and TinkerbellMachine
    `m1` with:
    - references as in the spec's example;
    - `vars` `schematic_id`;
    - `tasks: [{name: install, taskRef: {name: talos-install}}]`.
- [ ] **Step 5: Check the result.** No agent is needed: Tinkerbell renders the Workflow when it
  is created.
  - Workflow `m1` has `spec.references.talos.name: m1-config` with the Tinkerbell namespace.
  - Its `spec.vars` is as written.
  - `globals.templateMap` holds the machine variables.
  - `status.renderedTasks[0].actions[0].env.SCHEMATIC_ID` is the 64-character ID, and the
    Workflow has no `TemplateRenderedSuccess=False` condition.
  - Applying a TinkerbellMachine whose reference has literal namespace `default` (if that isn't
    the test namespace) is rejected.
  - Applying one with `{{ .machine_namespcae }}` is rejected, and the error lists the known
    variables.
  - `kubectl get events` shows no unexpected Warnings.
- [ ] **Step 6: Clean up.** Run `kind delete cluster --name capt-refs`. Record anything
  surprising in the hand-off, especially a CAPI refusal in Step 4, and how it was worked
  around.
