# Workflow references and vars on TinkerbellMachine: design

Date: 2026-11-09. Status: approved direction; this document is for review.

## Intent

A machine's Tasks should be able to read Kubernetes objects that belong to that machine, named
relative to the machine instead of literally. The first user is the Talos Image Factory schematic:
CABPT pins it in the machine's TalosConfig (`status.imageFactory.schematicID`), and the install Task
needs it. Today the only way in is a regular expression over the Hardware's user data.

Why this cannot be done today:

- A Tinkerbell Task can only reference objects by literal name, so one shared Task cannot name
  each machine's own TalosConfig.
- Tinkerbell never renders reference names, in any object, because it reads references before it
  renders anything.
- By design, Tinkerbell keeps `.references` local to each object. A Task cannot read its
  Workflow's references; data moves from a Workflow to its Tasks only through Workflow spec fields
  the Workflow renders, which the Task reads as `.workflow.spec.*`. See Tinkerbell's
  `docs/technical/v1alpha2/templating.md`, "Reference scope".

CAPT creates each machine's Workflow and knows the Machine, its Cluster and its bootstrap config.
So CAPT fills in the reference names and puts them on the Workflow, and a `vars` field carries
the referenced data on to the Tasks.

## Example

```yaml
# TinkerbellMachineTemplate spec.template.spec
references:                                  # becomes the Workflow's spec.references
  talos:
    group: bootstrap.cluster.x-k8s.io
    version: v1beta1
    resource: talosconfigs
    name: '{{ .bootstrap_config_name }}'     # CAPT fills these in
    namespace: '{{ .machine_namespace }}'
vars:                                        # becomes the Workflow's spec.vars, unchanged
  schematic_id: '{{ .references.talos.status.imageFactory.schematicID }}'   # Tinkerbell fills this in
tasks:
- name: install
  taskRef: {name: talos-install}
```

```yaml
# Task talos-install: shared by every machine, no references of its own
spec:
  actions:
  - name: talos2disk
    image: ghcr.io/sidero-community/actions/talos2disk:latest
    timeoutSeconds: 1200
    env:
      SCHEMATIC_ID: '{{ .workflow.spec.vars.schematic_id }}'
```

## Facts this design relies on (verified against Tinkerbell v0.28.6 and CAPT v0.10.0)

- **Workflow references:** `Workflow.spec.references` (`map[string]Reference`) is resolved with the
  Workflow as the source object, and the resolved objects are available as `.references` when
  Tinkerbell renders the Workflow's own spec. `spec.vars`, `spec.globals.env` and
  `spec.globals.templateMap` are rendered. `spec.references` and each task's `taskRef` and
  `hardwareRef` are not (`skipWorkflowPath` in `tink/controller/internal/workflow/render.go`).
- **What a Task sees:** the rendered Workflow as `.workflow`. Its `.references` are only its own.
- **When references are read:** Tasks get their values when Tinkerbell first renders the Workflow,
  and later changes to the referenced object do not reach them. Tinkerbell renders the Workflow
  again, and reads its references again, whenever it applies boot options before or after a task
  (`renderedBoot` → `renderedWorkflowFor`). CAPT's default boot options toggle netboot on the
  first task, so this always happens. If a reference cannot be read at that point, Tinkerbell
  retries the step, and netboot stays on until it succeeds.
- **Empty namespace:** in a Tinkerbell `Reference`, it means a cluster-scoped object.
- **Reference policy:** with no deny list configured, Tinkerbell denies every reference. Allow rules
  (chart value `deployment.envs.tinkController.referenceAllowListRules`) match the source's name and
  namespace and the reference's group, version, resource, name and namespace. The source of a
  Workflow reference is the Workflow, which CAPT creates in the Hardware's namespace
  (`status.targetNamespace`). So the policy cannot see the machine's namespace.
- **Access:** Tinkerbell reads a referenced object with its own credentials. RBAC for other kinds
  comes from the chart's `rbac.additionalRoleRules`. Tinkerbell must also watch the object's
  namespace.
- **The CAPT Workflow:** CAPT creates it only after the Machine's bootstrap data is ready, names it
  after the TinkerbellMachine, and already sets `globals.templateMap` to `{device_1: <lookup ID>}`
  (`taskWorkflow` and `workflowTemplateData`).
- **The schematic pin:** CABPT resolves `status.imageFactory` in the same reconcile that writes the
  bootstrap data (`resolveInstallerImage`, then `writeBootstrapData`), so it is set before CAPT
  creates the Workflow.
- **Namespaces:** a TinkerbellMachine is always in its Machine's namespace, because Cluster API
  references are same-namespace.

## Goals

- Add `references` and `vars` to `TinkerbellMachineConfig`. That struct is the spec of
  TinkerbellMachine and of TinkerbellMachineTemplate's template.
- `references` follows the shape of Tinkerbell's v1alpha2 `Reference`: `group`, `version`,
  `resource`, `name`, `namespace`. CAPT fills in `name` and `namespace` from machine variables.
- `vars` is copied unchanged to the Workflow's `spec.vars`, where Tinkerbell renders it.
- Works with any Tinkerbell CAPT already supports (v0.27.0 or later), with no Tinkerbell change.

## Non-goals

- `references` and `vars` on TinkerbellCluster(Template). That is a possible follow-up; tasks from
  the cluster or a Hardware annotation still get the machine's references and vars.
- Cluster-scoped references, and references into any namespace other than the machine's.
- External Tinkerbell (`--external-tinkerbell` mode).
- Machines provisioned from the deprecated `templateInline`, `templateRef` or the
  `capt-template-override` annotation.
- Filling in `group`, `version` or `resource`.
- CAPT rendering `vars`, or checking them. Tinkerbell renders them with its own functions and
  reports errors as it does today; CAPT shows those as `WorkflowNotRendered`.
- Changing what Tasks see after the Workflow first renders.
- Restricting Tasks that a `taskRef` names, including Tasks that carry their own `references`.
  Whoever writes those Tasks writes them in the Tinkerbell namespace and is governed by
  Tinkerbell's policy. CAPT's namespace rule covers only the references CAPT writes.

## Anti-goals (failure even if it works)

- Someone who can write a TinkerbellMachine in one namespace reading objects in another namespace.
- CAPT creating a Workflow whose references it could not fill in correctly.
- CAPT touching Tinkerbell's templates, or Tinkerbell touching CAPT's. `name` and `namespace`
  belong to CAPT; everything else belongs to Tinkerbell.

## API (CAPT `api/v1beta2`)

```go
// In TinkerbellMachineConfig:

// References are added to the machine's Workflow as spec.references, so that templates in the
// Workflow's spec, such as vars, can read the objects they name as {{ .references.<key> }}.
// CAPT renders each reference's name and namespace as Go templates with the machine's
// variables (see WorkflowReference) when it creates the Workflow. Each must resolve into the
// machine's namespace. They apply only to a machine provisioned from tasks.
//
// +optional
// +kubebuilder:validation:MaxProperties=16
References map[string]WorkflowReference `json:"references,omitempty"`

// Vars are added to the machine's Workflow as spec.vars, unchanged. Tinkerbell renders them
// with the Workflow's references and Hardware, and Tasks read them as
// {{ .workflow.spec.vars.<key> }}. They apply only to a machine provisioned from tasks.
//
// +optional
// +kubebuilder:validation:MaxProperties=32
Vars map[string]string `json:"vars,omitempty"`
```

```go
// WorkflowReference names an object the machine's Workflow reads, in the format of a Tinkerbell
// v1alpha2 Reference. Name and Namespace are Go templates CAPT renders with the machine's
// variables: machine_name, machine_namespace, cluster_name, bootstrap_config_name,
// tinkerbell_machine_name, hardware_name and hardware_namespace.
type WorkflowReference struct {
    // Group of the object; empty for the core API group. Not templated.
    // +optional
    Group string `json:"group,omitempty"`

    // Version of the object's API. Not templated.
    // +kubebuilder:validation:MinLength=1
    Version string `json:"version"`

    // Resource is the object's lowercase plural resource name. Not templated.
    // +kubebuilder:validation:MinLength=1
    Resource string `json:"resource"`

    // Name of the object, as a template.
    // +kubebuilder:validation:MinLength=1
    // +kubebuilder:validation:MaxLength=512
    Name string `json:"name"`

    // Namespace of the object, as a template. Defaults to the machine's namespace, and must
    // resolve to it.
    // +optional
    // +kubebuilder:validation:MaxLength=512
    Namespace string `json:"namespace,omitempty"`
}
```

Add a rule to `TinkerbellMachineConfig` next to the existing one. The webhook checks the same
thing, because a v1beta1 write that restores stashed fields never reaches the v1beta2 CEL rules.

```go
// +kubebuilder:validation:XValidation:rule="!(has(self.references) || has(self.vars)) || !(has(self.templateInline) || has(self.templateRef))",message="references and vars apply to tasks and cannot be set with templateInline or templateRef"
```

## Machine variables

| Variable | Value |
| --- | --- |
| `machine_name` | the Machine's name |
| `machine_namespace` | the Machine's namespace, which is also the TinkerbellMachine's |
| `cluster_name` | the Machine's `spec.clusterName` |
| `bootstrap_config_name` | the Machine's `spec.bootstrap.configRef.name`, or `""` when it has none |
| `tinkerbell_machine_name` | the TinkerbellMachine's name |
| `hardware_name` | the claimed Hardware's name |
| `hardware_namespace` | the claimed Hardware's namespace, where the Workflow is created |

For the TalosConfig, docs recommend `bootstrap_config_name`. It is the Machine's own pointer to its
bootstrap config, so it does not depend on providers naming the config after the Machine.

CAPT also adds these variables to the Workflow's `globals.templateMap`, next to `device_1`, for
Workflows that run tasks. Tasks can then read, for example,
`{{ .workflow.spec.globals.templateMap.cluster_name }}`. Workflows built from templates keep
`{device_1}` only. *(My call.)*

## Rendering (CAPT)

- **Engine:** Go `text/template` with `missingkey=error`, the built-in functions only, and the
  variables above as a `map[string]string`.
- **What is rendered:** `name` and `namespace` only. The result is used as is, with no trimming.
- **Name:** must be non-empty and a valid DNS-1123 subdomain. Otherwise the reference is not
  rendered (`ReferenceNotRendered`). An unset `bootstrap_config_name` produces an empty name, which
  is reported this way.
- **Namespace:** empty after rendering means the machine's namespace. Anything other than the
  machine's namespace is refused (`ReferenceOutsideNamespace`). A cluster-scoped reference is not
  possible.
- **On the Workflow:** each reference is written as a Tinkerbell `Reference` with the rendered name
  and namespace and the literal group, version and resource.

## Admission (webhooks for TinkerbellMachine and TinkerbellMachineTemplate)

For each reference, at `spec.references[<key>]` (TinkerbellMachineTemplate:
`spec.template.spec.references[<key>]`):

- **Group, version and resource:** must not contain `{{` (Invalid: these are not templated).
- **Name and namespace:**
  - They must parse as templates.
  - They must execute without error against sample data that has every variable set to a valid
    placeholder, such as `x`. A misspelled variable is therefore rejected, and the message lists
    the known variables.
  - A namespace with no `{{`, which is a literal, must be empty or equal to the object's own
    namespace (Forbidden). Templated namespaces are checked when the Workflow is created.
- **Source:** `references` or `vars` set together with `templateInline` or `templateRef` is
  Forbidden, with the CEL message.

`vars` are not checked by CAPT.

## Controller

The changes are in `controller/machine`, in `createTaskWorkflow` and before anything is created,
following the existing rule that nothing is created until everything the Workflow needs is known.

1. **No references:** nothing changes apart from the `globals.templateMap` additions.
2. **External Tinkerbell:** CAPT records Warning `ReferencesUnsupported` and returns
   `ErrReferencesUnsupported`. No Tasks or Workflow are created. `vars` alone are allowed in
   external mode.
3. **Rendering:** CAPT renders every reference. Each failure is recorded as its own Warning event,
   `ReferenceNotRendered` or `ReferenceOutsideNamespace`, naming the key and the reason. If any
   reference failed, it returns `ErrReferenceNotRendered`, wrapped with all the failed keys, and
   creates nothing.
4. **The Workflow:** `taskWorkflow` sets `spec.references` from the rendered references,
   `spec.vars` as a copy of `vars`, and `globals.templateMap` as `device_1` plus the machine
   variables.

When the machine sets `references` or `vars` but its source is a template, CAPT records Warning
`ReferencesIgnored` and builds the Workflow exactly as before. The source is a template when
`taskSource` returns none and a template comes from the Hardware annotation or the cluster.

Errors are returned like the existing Task errors, so the reconcile is retried with backoff and
the events explain why.

## Conversion (`api/v1beta1`)

- v1beta1 has neither field. Both are kept in the conversion annotation and restored by
  `ConvertMachineToHub` and `ConvertMachineTemplateToHub`, exactly like `Tasks`.
- The conversion fuzz tests must keep passing.
- The webhook rejects a v1beta1 write whose restored `references` or `vars` would sit next to a
  `templateOverride`, mirroring the existing tasks test.

## CABPT (sidero-community/cluster-api-bootstrap-provider-talos)

- **Allowlist:** add `spec.references` and `spec.vars` to the TinkerbellMachine in-place allowlist
  in `internal/inplace/policy.go`, beside `spec.tasks`.
- **Why it is safe:** like tasks, they are read only when CAPT creates a Workflow, so editing them
  never re-images a running machine.
- **Test:** add a `canupdate_test.go` case.
- **Release:** not part of this work.

## Documentation (CAPT `docs/QUICK-START.md`)

Add a section, "Read the machine's objects from Tasks", covering:

- the example above;
- the variables table;
- the two-engine rule: CAPT fills in reference names and namespaces, and Tinkerbell renders
  `vars` and Tasks;
- the Tinkerbell prerequisites:
  - an allow rule, for example `{"reference":{"group":["bootstrap.cluster.x-k8s.io"],"resource":["talosconfigs"]}}`
    (verify the value format against the chart);
  - `rbac.additionalRoleRules` granting `get` on `talosconfigs`;
  - Tinkerbell watching the machine's namespace;
  - Tinkerbell in the same cluster as the machines;
- the security note:
  - anyone who can write a TinkerbellMachine in a namespace can read, through its Workflow, any
    object in that namespace whose kind your allow rule permits;
  - the allow rule applies to all of Tinkerbell, not just CAPT's Workflows. Any Task, Hardware or
    Workflow in a namespace Tinkerbell watches can then read objects of that kind in every
    watched namespace. Narrow the rule by source and reference namespace where you can;
  - a TalosConfig's spec holds its config patches, which can contain secrets;
- the timing note:
  - Tasks get their values when the Workflow first renders;
  - Tinkerbell reads the references again around each task's boot options, so keep the
    referenced object readable until the Workflow finishes;
  - otherwise that step is retried and netboot stays on.

## Verification

- **Unit tests:**
  - rendering, covering each variable, an unknown variable, an empty name, an invalid name, a
    namespace defaulted, a namespace outside, and group/version/resource kept literal;
  - webhook rules for both kinds;
  - controller behaviour with the fake client, covering the Workflow fields, nothing created on
    any failure, external mode, the template-source event and the templateMap additions;
  - the conversion round trip.
- **envtest CRD validation:** the new CEL rule and the `MaxProperties`/`MinLength` markers.
- **kind, end to end, run once by hand as for v0.10.0 (not committed; CAPT has no e2e harness or
  CI job):** Tinkerbell v0.28.6 with an allow rule and RBAC for `talosconfigs`, plus
  CABPT's TalosConfig CRD with no controller. Create a TalosConfig whose status holds a schematic
  ID, then a machine whose `vars` read it. The Workflow's rendered task env must contain the
  schematic ID.

## Decisions

Decided by the user:

- references on `TinkerbellMachineConfig` in Tinkerbell's v1alpha2 `Reference` format;
- templated with machine variables such as `machine_name`, `machine_namespace` and `cluster_name`;
- references limited to the machine's own namespace;
- write a spec and a plan.

My calls, proposed in chat before the user approved the write-up:

- `vars` as the bridge into Tasks, following Tinkerbell's documented pattern;
- only `name` and `namespace` are templated;
- unknown variables are rejected at admission;
- an empty or invalid rendered name stops Workflow creation;
- references are rejected next to deprecated template fields and ignored, with an event, when a
  template comes from elsewhere;
- external Tinkerbell is unsupported;
- the machine variables are also published in `globals.templateMap`;
- the CABPT allowlist additions and the v1beta1 stash.

Made while writing this document:

- the exact variable names, including `tinkerbell_machine_name`, `hardware_name` and
  `hardware_namespace`;
- the `MaxProperties` limits (16 references, 32 vars);
- a DNS-1123 check on the rendered name;
- `vars` allowed without references, including in external mode;
- the event reason names.

Made after the builder check:

- the cross-namespace exposure through Tinkerbell's global allow rule is handled in the docs, with
  no CAPT change to `taskRef` Tasks. Refusing referenced Tasks that carry references would break
  existing shared Tasks, and their authors already need write access in the Tinkerbell namespace;
- the kind run is a one-off check by hand, as for v0.10.0;
- the timing note is corrected: references are read again around boot options.

## Left to the builder

- Helper and file names, for example `controller/machine/references.go` and
  `webhooks/references.go`.
- Event message wording.
- How sample data is built for admission.
- Test names.
