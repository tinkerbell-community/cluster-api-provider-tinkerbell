# v1alpha2 Task fields for TinkerbellMachine and TinkerbellCluster

Status: draft for approval, 2026-11-08.

## Intent

CAPT v0.9 runs machines on Tinkerbell v1alpha2, but its own API still speaks v1alpha1:
users describe provisioning as a v1alpha1 Template (`templateInline`, a YAML string with
v1alpha1 template syntax, or `templateRef`, a v1alpha1 Template object). CAPT then
translates that template into v1alpha2 Tasks behind the user's back, or falls back to
v1alpha1 compatibility mode when it can't. `templateRef` also keeps the deprecated Template
CRD alive.

This change lets users describe provisioning directly as v1alpha2 Tasks, the same two ways
as before:

- **inline:** Task definitions written in the machine or cluster spec. CAPT creates them as
  Task objects owned by the machine, the way it creates the machine's Template today.
- **by reference:** an ordered list of Task objects the user manages. CAPT points the
  machine's Workflow at them and never creates, changes, or deletes them.

Both kinds live in one ordered `tasks` list, so a machine can mix them, and each entry can
carry its own v1alpha2 boot options, as each task of an upstream v1alpha2 Workflow does. The
goal is to stay as close to the upstream Workflow spec as possible, so what users write maps
one to one onto what Tinkerbell runs.

The old template fields stay and keep working, but they are deprecated.

## Goals

1. A new `tasks` field on TinkerbellMachine (and so TinkerbellMachineTemplate) and
   TinkerbellCluster (and so TinkerbellClusterTemplate): an ordered list whose entries are
   each an inline Task or a Task reference, with optional v1alpha2 boot options.
2. A Task equivalent of the Hardware template override annotation.
3. When the new fields are used, CAPT builds a native v1alpha2 Workflow directly. It doesn't
   go through v1alpha1 Template conversion, and compatibility mode is never involved.
4. Existing objects using `templateInline`, `templateRef`, the template override annotation,
   or the machine-level `bootOptions` behave exactly as they do in v0.9.2.
5. CABPT's in-place update hook treats all the new TinkerbellMachine fields as
   provisioning-time only, so changing them on a running machine never causes a rollout.

## Non-goals

- Removing `templateInline` or `templateRef`, or adding a new CAPT API version. They are
  deprecated only.
- Changing the machine-level `bootOptions` field (it keeps its v1alpha1-shaped types).
- Rewriting existing objects from templates to Tasks, automatically or by a tool.
- Exposing other v1alpha2 Workflow knobs (Workflow or per-task timeouts, globals, vars,
  references at Workflow level). Actions keep their own `timeoutSeconds` inside the Task. The
  entry types below leave room to add a per-entry `timeoutSeconds` later.
- Changing the user's live clusters or the cluster-bootstrap repo. The `tinkerbell/talos`
  Cluster is on an owner hold.
- Cutting a release. That is a separate request (recommended version: CAPT v0.10.0, because
  the API grows).

## Anti-goals (a failure even if it technically works)

- An existing TinkerbellMachineTemplate or TinkerbellCluster loses or changes behaviour after
  the upgrade, including after a v1beta1 round trip.
- Editing the new fields on a running machine re-images it, or makes CAPI roll it out when
  the CABPT extension is installed.
- Moving a MachineDeployment from templates to Tasks makes CAPI roll out its machines when
  the CABPT extension is installed.
- CAPT edits or deletes a Task the user referenced.
- A Workflow is created that can never render because a referenced Task doesn't exist, with
  nothing on the TinkerbellMachine saying why.

## Constraints and facts

- CAPT `main` is 0e7e65b, the same controller code as v0.9.2. Work happens in the worktree
  `../cluster-api-provider-tinkerbell.tasks` on branch `feat/v1alpha2-task-fields`. Other
  sessions use the main CAPT checkout and the shared Tinkerbell checkout: don't stash or
  commit there.
- CAPT's v1beta2 API is the hub (storage) version; v1beta1 converts to it. Fields v1beta1
  doesn't have are stashed in the `dataAnnotation` annotation via `marshalData`/`unmarshalData`
  in `api/v1beta1/conversion.go` (as `templateRef` already is).
- CAPT has validating webhooks for all four kinds in `webhooks/`. They already return
  `admission.Warnings` (always nil today), and `matchPolicy=Equivalent` routes v1beta1 writes
  to them too.
- The CAPT `api/` directory is its own Go module. It requires
  `github.com/tinkerbell/tinkerbell/api v0.23.0`, which has no v1alpha2 package. The main
  module replaces that with the fork `github.com/tinkerbell-community/tinkerbell/api v0.28.0`
  (has v1alpha2 `TaskSpec`, `BootOptions`, `Customboot`, `bmc.Operations`). Replace
  directives only apply in the main module, so the `api` module needs the same replace (and
  a require bump) to build and test on its own.
- Tinkerbell v1alpha2 behaviour this relies on (Tinkerbell v0.28.6):
  - A Workflow copies its Tasks into `status.renderedTasks` once, on the first reconcile, and
    never re-reads them. Nothing re-runs a finished Workflow.
  - Boot options live on each Workflow task (`spec.tasks[].hardware.bootOptions`). The "before"
    part (netboot on, boot mode, customboot `preOperations`) runs before that task; the
    "after" part (netboot off, ISO eject, customboot `postOperations`) runs after it succeeds.
  - Boot options are rendered with the same template data as the Workflow (`.hardware` etc.),
    so an `isoURL` can be templated with Hardware data.
  - A native Task renders with `.hardware` (the v1alpha2 Hardware), `.workflow`,
    `.references`, `.task` and `.metadata_url`. The Workflow's `globals.templateMap` is *not*
    top-level data; it's reachable as `.workflow.spec.globals.templateMap`. v1alpha1 syntax
    such as `{{ index .Hardware.Disks 0 }}` becomes
    `{{ (index .hardware.spec.storageDevices 0).name }}`.
  - Tinkerbell resolves Task references across namespaces.
- CAPT only creates a machine's Tasks and Workflow once (`ensureWorkflow` →
  `createWorkflow`). Once the Hardware has `HardwareProvisionedAnnotation=true`, reconcile
  short-circuits. Boot options, machine-level or per-task, are only read when building the
  Workflow.
- CABPT (`sidero-community/cluster-api-bootstrap-provider-talos`) keeps the in-place
  allowlist in `internal/inplace/policy.go` (`infraPolicies`, TinkerbellMachine entry). Paths
  are relative to `spec` and get `spec.template.spec` prefixed for MachineSet templates.
- Lint: `make lint` needs `GOTOOLCHAIN=go1.26.1` (system Go is newer than the lint binary).
- Never run kubectl or helm against the default `admin@talos` context. Test with envtest and
  kind clusters that have their own kubeconfigs.

## Decided by the user

- Both inline Tasks and Task references, mirroring `templateInline`/`templateRef`.
- `templateInline` and `templateRef` stay for backwards compatibility, deprecated.
- Each inline Task and each Task reference can specify its own boot options, like the tasks
  of an upstream v1alpha2 Workflow.
- Inline Tasks and Task references are consolidated into one `tasks` list: each entry has
  either a `taskRef` or a `taskInline`.
- The CABPT in-place hook includes the new attributes.

## The design

### API (v1beta2)

On `TinkerbellMachineConfig` (so TinkerbellMachine and TinkerbellMachineTemplate) and on
`TinkerbellClusterSpec` (so TinkerbellCluster and TinkerbellClusterTemplate):

```go
// Tasks are run in order on the machine's Hardware. Each is a Task CAPT creates for the
// machine from TaskInline, or an existing Task named by TaskRef. Mutually exclusive with
// TemplateInline and TemplateRef.
// +optional
// +listType=map
// +listMapKey=name
// +kubebuilder:validation:MinItems=1
Tasks []MachineTask `json:"tasks,omitempty"`
```

```go
// MachineTask is one Task the machine's Workflow runs: inline or by reference.
// +kubebuilder:validation:XValidation:rule="has(self.taskRef) != has(self.taskInline)",message="exactly one of taskRef and taskInline must be set"
type MachineTask struct {
    // Name identifies the entry. A Task CAPT creates from TaskInline is named
    // <machine name>-<name>.
    // +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
    // +kubebuilder:validation:MaxLength=63
    Name string `json:"name"`

    // TaskRef names an existing Task. CAPT never creates, changes or deletes it.
    // +optional
    TaskRef *TaskRef `json:"taskRef,omitempty"`

    // TaskInline is a Task definition. CAPT creates it as a Task owned by the machine and
    // deletes it with the machine.
    // +optional
    TaskInline *tinkv2.TaskSpec `json:"taskInline,omitempty"`

    // BootOptions are applied around this Task, as in a v1alpha2 Workflow task.
    // +optional
    BootOptions *tinkv2.BootOptions `json:"bootOptions,omitempty"`
}

// TaskRef references an existing Task.
type TaskRef struct {
    // Name is the name of the Task.
    Name string `json:"name"`

    // Namespace is the namespace of the Task. Defaults to the Tinkerbell namespace, where
    // the Workflow is created.
    // +optional
    Namespace string `json:"namespace,omitempty"`
}
```

Example:

```yaml
tasks:
  - name: install
    bootOptions:
      toggleNetboot: true
      bootMode: netboot
    taskInline:
      actions:
        - name: stream-image
          image: ghcr.io/siderolabs/talos2disk:latest
          env:
            DEST_DISK: '{{ (index .hardware.spec.storageDevices 0).name }}'
  - name: cleanup
    taskRef:
      name: shared-cleanup
```

Notes:

- `taskRef` mirrors upstream's `spec.tasks[].taskRef`. `bootOptions` sits directly on the
  entry rather than under upstream's `hardware:` because CAPT always fills in the Hardware
  reference itself. *(my call)*
- `name` is required on every entry, references included. It gives each entry a stable
  identity in the list, in events and in errors, and names the Task created from an inline
  entry. *(my call)*
- The v1alpha2 `BootOptions` type brings its own validation (`bootMode` enum
  `netboot`/`isoboot`/`customboot`, `isoURL` format).

Validation: replace the existing two-field CEL rule on both types with "at most one of
`tasks`, `templateInline`, `templateRef`".

`templateInline` and `templateRef` doc comments say "Deprecated: use tasks." The Hardware annotation `hardware.tinkerbell.org/capt-template-override` is
documented as deprecated too. The machine-level `bootOptions` doc comment says it applies to
template sources, and to Task sources whose entries set no boot options.

### Hardware annotation

New annotation `hardware.tinkerbell.org/capt-task-refs`: comma-separated Task names in the
Hardware's namespace, run in order. An annotation can't carry boot options, so these use the
default boot options (below). It sits at the same level as the template override
annotation. If a Hardware has both, the Task annotation wins and CAPT emits a warning event
on the TinkerbellMachine.

### Which source a machine uses

Same levels and precedence as today; the most specific level wins whatever style it uses:

1. the TinkerbellMachine: `tasks` | `templateInline` | `templateRef`
2. the Hardware: `capt-task-refs` | `capt-template-override`
3. the TinkerbellCluster: `tasks` | `templateInline` | `templateRef`

A template source goes through today's path unchanged (native rewrite when it converts,
compatibility mode otherwise). The "Hardware without storage devices" check applies to both
kinds of source.

### Native Workflow for Task sources

CAPT builds the v1alpha2 Workflow directly (not through `v1alpha1Workflow`/conversion):

- name: the machine's name; namespace: the Tinkerbell namespace; ownership labels via
  `setResourceOwnership`.
- one `spec.tasks` entry per Task, in order, each with `agentID` = `hardwareLookupID(hw)`,
  `taskRef`, and `hardware.hardwareRef` = the selected Hardware.
- `spec.globals.templateMap` = `{"device_1": hardwareLookupID(hw)}`, so Tasks can still read
  it (as `.workflow.spec.globals.templateMap.device_1`).

**Boot options.** There are two modes, chosen per machine:

- **Explicit:** if *any* entry in the list sets `bootOptions`, every entry's
  `bootOptions` are copied verbatim into its `spec.tasks[].hardware.bootOptions`, and entries
  without them get none. CAPT adds nothing: no implicit netboot toggle, no BMC check, no ISO
  URL rewrite. This is the upstream behaviour: what the user writes is what runs. A user who
  needs a per-machine ISO URL templates it, e.g. with the Hardware's MAC. *(my call: no CAPT
  magic once the user takes control)*
- **Default:** if no entry sets `bootOptions` (including Task references from the Hardware
  annotation), CAPT puts today's boot options on the **first** task: always
  `toggleNetboot: true`, plus the boot mode when the Hardware has a BMC, translated from the
  machine-level `bootOptions` as `v1alpha1Workflow` does today:
  - `netboot` → `bootMode: netboot`
  - `isoboot`/`iso` → `bootMode: isoboot` with the per-MAC ISO URL rewrite (`iso` is sent as
    `isoboot`; a native Workflow has no legacy value)
  - `customboot` → `bootMode: customboot`, with `customboot` converted from
    `custombootConfig` by the Tinkerbell conversion package. A `custombootConfig` that
    contains template expressions is refused with an error and a Warning event, because a
    native Workflow would render it with v1alpha2 data; the message says to set
    `bootOptions` on the entries instead.

  For one Task, this matches today exactly. With several Tasks, the "after" work runs after
  the first one; the docs say so, and point to explicit boot options for anything else.

  If the machine-level `bootOptions` is set *and* the entries set boot options, the entries
  win and CAPT emits a warning event saying the machine-level `bootOptions` was ignored.

Before creating the Workflow (a list can mix both kinds of entry):

- **`taskInline` entries:** create or update the machine's Tasks with the existing
  `taskConflict` and `ensureTasks` helpers (Task name `<machine>-<name>`, Tinkerbell
  namespace, ownership labels). Deleting the machine removes them, as `removeDependencies`
  already does.
- **`taskRef` entries:** get every referenced Task. If any is missing, don't create the Workflow; return
  an error and record a Warning event on the TinkerbellMachine naming the missing Task. A
  returned error alone only reaches the logs. No new condition, so no API addition. *(my
  call)* Referenced Tasks never get ownership labels, so `removeTasks` can't delete them.
  Task references in other namespaces are handled as `templateRef` handles them today (same
  client, cache, and RBAC); if CAPT can't read the namespace, it's the same error path.
  *(builder's call)*
- **stale machine Template:** a v1alpha1 Template named after the machine, left by an
  earlier attempt, forces compatibility mode on the template path today. A Task source
  ignores it. `removeDependencies` still deletes it with the machine. *(my call)*

### Deprecation signal

The validating webhooks:

- return an admission warning when `templateInline` or `templateRef` is set, naming the
  replacement;
- also enforce the source exclusivity, so a v1beta1 write that restores a stashed Task field
  next to a template field is caught. CEL stays as the first line.

CAPT doesn't validate Tinkerbell objects, so the deprecated Hardware annotation gets a Normal
event (reason `DeprecatedTemplateSource`) on the TinkerbellMachine when CAPT creates its
Workflow.

### v1beta1 conversion

v1beta1 doesn't gain the field. `tasks` round-trips through the
existing `dataAnnotation` stash for TinkerbellMachine, TinkerbellMachineTemplate and
TinkerbellCluster, like `templateRef`.

### CABPT in-place allowlist

The TinkerbellMachine entry in `internal/inplace/policy.go` becomes:

- `spec.templateInline`, `spec.templateRef` (unchanged)
- `spec.tasks` (new; this covers the per-entry `bootOptions`)
- `spec.hardwareAffinity` (unchanged)
- `spec.bootOptions` — the whole field, replacing today's `spec.bootOptions.isoURL`. *(my
  call)*

The reason is the same as for `templateInline`: CAPT only reads them when it creates a
Workflow, so they take effect the next time the machine is actually provisioned.

The current comment excludes `spec.bootOptions.bootMode` as "changes how the machine boots,
not inert". That isn't what CAPT does: it reads `bootOptions` only in `v1alpha1Workflow`,
when it creates the Workflow. Keeping that exclusion would break the migration anti-goal:
moving boot options from the machine level onto the entries clears `spec.bootOptions` and
would roll out every machine.

Update the comment, and the tests for both the Machine and MachineSet (template-prefixed)
paths.

### Docs and templates

- `templates/cluster-template.yaml`: switch to `tasks` with a `taskInline` entry in v1alpha2 syntax, with explicit
  boot options on its entry.
- `docs/QUICK-START.md` and `docs/COMPATIBILITY.md`: show the new fields. Add a short "from
  templateInline to tasks" section with the syntax mapping above, the two boot-option
  modes, and a note that existing objects keep working.
- API reference comments carry the deprecations.

## Left to the builder

- Package layout, helper names, and how the source resolution is factored.
- The exact event and warning message text.
- Test structure, as long as it covers:
  - CEL and webhook exclusivity (envtest), including a v1beta1 write;
  - each source at each level, and the precedence between them;
  - both boot-option modes: explicit per-entry options copied verbatim across several Tasks,
    and the default mode's placement and BMC/no-BMC cases, including a templated
    `custombootConfig`;
  - missing Task references;
  - Task ownership and deletion (referenced Tasks untouched);
  - v1beta1 round trips;
  - the CABPT allowlist for both paths;
  - a kind run against Tinkerbell v0.28.6 where one machine provisions from an inline entry
    and one from a mixed list (inline plus a reference), one of them with explicit boot
    options.
- Regenerating CRDs, deepcopy, and conversion code with the repo's make targets.
