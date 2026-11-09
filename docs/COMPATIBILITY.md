# Compatibility

This document describes the version compatibility between CAPT (Cluster API Provider Tinkerbell) and Cluster API (CAPI),
and between CAPT and Tinkerbell.

## Tinkerbell

CAPT requires Tinkerbell from [tinkerbell-community](https://github.com/tinkerbell-community/tinkerbell)
**v0.27.0 or later**. It reads and writes Hardware, Workflows, Tasks and bmc Jobs as
`tinkerbell.org/v1alpha2` and `bmc.tinkerbell.org/v1alpha2`, which Tinkerbell stores from v0.27.0 on.
Earlier CAPT releases use the deprecated `v1alpha1` kinds; Tinkerbell v0.27 still serves those
through its conversion webhook, so upgrade Tinkerbell first, then CAPT. From Tinkerbell v0.26 or
earlier, upgrade straight to v0.28.2 or later: v0.27.0 to v0.28.1 cannot finish moving the stored
objects to `v1alpha2` in place and restart until replaced (see the
[v0.28.2 release notes](https://github.com/tinkerbell-community/tinkerbell/releases/tag/v0.28.2)).

Tinkerbell v0.28.0 stores Workflow states by name (`Pending`, ...) instead of as numbers. CAPT
v0.9.0 cannot read those; CAPT v0.9.1 reads both. With Tinkerbell v0.28.0 or later, use CAPT
v0.9.1 or later, and upgrade CAPT first.

Tinkerbell can remove the CRDs of the kinds that only exist in `v1alpha1`, Template among them
(`--enable-deprecated-crd-removal`, off by default). CAPT v0.9.1 and earlier can neither provision
nor delete machines without the Template CRD. CAPT v0.9.2 works without it, but a template that
does not convert to `v1alpha2` Tasks, and a `templateRef`, need a Template: without the CRD,
creating the Workflow fails (`tinkerbell no longer serves v1alpha1 Templates`). Upgrade CAPT to
v0.9.2, and make sure every machine's template is inline and converts, before turning removal on.
Machines provisioned from `tasks` never need the Template CRD.

| CAPT | Tinkerbell (tinkerbell-community) |
| ---- | --------------------------------- |
| v0.9.2 | v0.27.0 or later; needed for a Tinkerbell without the Template CRD |
| v0.9.1 | v0.27.0 or later |
| v0.9.0 | v0.27.x |
| v0.8.x and earlier | any release that serves `v1alpha1` (v0.27 and v0.28 do) |

CAPT builds against the
fork's API module (`github.com/tinkerbell-community/tinkerbell/api` v0.28.0, replacing
`github.com/tinkerbell/tinkerbell/api`) and uses its `conversion` package, so it maps the two
versions exactly as Tinkerbell's conversion webhook does.

CAPT's own API module (`github.com/tinkerbell/cluster-api-provider-tinkerbell/api`) embeds the
fork's `v1alpha2` types in `tasks`, through the same `replace`. Go's `replace` directives only
apply in the module being built, so a Go project that imports CAPT's API must add it too:

```
replace github.com/tinkerbell/tinkerbell/api => github.com/tinkerbell-community/tinkerbell/api v0.28.0
```

Upstream `github.com/tinkerbell/tinkerbell/api` v0.23.0 has no `v1alpha2` package, and v0.25.0
has one without the types `tasks` uses (such as `Customboot`).

| Object | API version | What CAPT does with it |
|---|---|---|
| `Hardware` | `tinkerbell.org/v1alpha2` | Selects and claims it (labels, finalizer), writes `spec.instance.userdata`, the IPAM address, and the provisioned annotation, all as merge patches of those fields only |
| `Workflow` | `tinkerbell.org/v1alpha2` | Creates one per machine; follows `status.metadata.workflow.state` |
| `Task` | `tinkerbell.org/v1alpha2` | Creates the Tasks of a machine's inline `tasks` entries, `<machine>-<name>`, or of its template's tasks, `<machine>-<task>`; reads, and never changes, the Tasks `taskRef` entries and the `capt-task-refs` annotation name |
| `Template` | `tinkerbell.org/v1alpha1` | Only for the deprecated sources: reads the Templates `templateRef` and the template override annotation name; creates the machine's Template for a Workflow in compatibility mode. Not needed for `tasks` or an inline template that converts, so CAPT works when Tinkerbell has removed the Template CRD |
| `Job` | `bmc.tinkerbell.org/v1alpha2` | Creates power off (deletion) and power cycle (in-place recovery) Jobs that reference the Hardware |

`TinkerbellMachine` and `TinkerbellCluster` (and their templates) have `tasks`: `v1alpha2` Tasks,
inline or by reference, each with optional `v1alpha2` boot options, which CAPT runs in a native
Workflow exactly as written (see [Customize the provisioning template](QUICK-START.md#customize-the-provisioning-template)).
`templateInline`, `templateRef` and the template override annotation are deprecated but keep
working unchanged: the template is a `v1alpha1` Template definition, and `spec.bootOptions` keep
their `v1alpha1` boot mode and customboot actions, which CAPT converts to `v1alpha2` itself.

### Workflows: native or v1alpha1 compatibility mode

A machine provisioned from `tasks` always gets a native Workflow: CAPT creates its inline Tasks
and a Workflow that runs the entries in order, with no conversion involved.

For the deprecated template sources, CAPT builds the `v1alpha1` Workflow it has always created
(the machine's Template, its Hardware, `device_1`, the boot options) and converts it to `v1alpha2`
with Tinkerbell's conversion. Then:

- If the template converts to `v1alpha2` Tasks and the Workflow can run them, CAPT creates the
  Tasks and a Workflow that runs them, and no Template. This is what Tinkerbell itself does to a
  `v1alpha1` Workflow whose Template converts.
- Otherwise CAPT creates the machine's Template and the converted Workflow, which Tinkerbell runs
  in `v1alpha1` compatibility mode, rendering the Template exactly as before. The Workflow's
  `capt.tinkerbell.org/v1alpha1-compatibility` annotation and an event on the `TinkerbellMachine`
  say why: the template's structure depends on templating (`range`, `if`, `toYaml`, ...) or it
  uses data with no `v1alpha2` equivalent (such as `.Hardware.Interfaces`), it has several tasks (CAPT's Workflow
  always has boot options, which `v1alpha2` applies per task), its customboot actions are
  templated, a Task of the same name already exists, or the machine's Template already exists
  (a machine an earlier CAPT started).

Either way a `v1alpha1` client reads back the Workflow CAPT created before (the deprecated `iso`
boot mode reads back as `isoboot`). Deleting a machine removes its Workflow, Tasks and Template.
See [How CAPT runs the template](QUICK-START.md#how-capt-runs-the-template).

### Hardware fields CAPT uses

| v1alpha1 | v1alpha2 |
|---|---|
| `spec.disks[].device` | `spec.storageDevices[].name` |
| `spec.userData` | `spec.instance.userdata` |
| `spec.metadata.instance.id` (`device_1`, ISO URL) | `spec.instance.lookupID`, or `spec.agentID` when it is empty |
| `spec.interfaces[0]` (node address, IPAM interface) | the primary interface: for Hardware created through `v1alpha1` its first interface with a MAC, otherwise the one `spec.agentID` names, otherwise the lowest MAC |
| `spec.interfaces[].dhcp.ip` `address`, `netmask`, `gateway` | `spec.networkInterfaces[<mac>].ipam.ipv4` (or `ipv6`) `address`, `prefix`, `gateway` |
| `spec.bmcRef` | `spec.bmc.connection`, or for Hardware created through `v1alpha1` the `bmcRef` its conversion annotation keeps |

CAPT does not read any other `metadata.instance` field, and does not toggle netboot itself
(Tinkerbell does, from the Workflow's boot options). Hardware created through `v1alpha1` keeps
its `conversion.tinkerbell.org/v1alpha1` annotation, and with it every `v1alpha1`-only field,
across CAPT's writes.

### RBAC

On the Tinkerbell cluster CAPT needs, in addition to what earlier releases needed, `tasks`
(`tinkerbell.org`): get, list, watch, create, update, patch, delete. On the management cluster it
records events. See [REMOTE-TINKERBELL-KUBECONFIG.md](REMOTE-TINKERBELL-KUBECONFIG.md) for an
external Tinkerbell cluster.

## Version Matrix

| CAPT Version | CAPI Contract | CAPI Version | API Group Versions | Kubernetes | Go  |
|---|---|---|---|---|---|
| v0.7.x | v1beta2 | >= v1.12.x | `v1beta2` (storage) + `v1beta1` (served) | v1.35+ | 1.25+ |
| v0.5.x - v0.6.x | v1beta1 | v1.6.x - v1.10.x | `infrastructure.cluster.x-k8s.io/v1beta1` | v1.29 - v1.34 | 1.22 - 1.24 |
| v0.3.x - v0.4.x | v1beta1 | v1.3.x - v1.5.x | `infrastructure.cluster.x-k8s.io/v1beta1` | v1.22 - v1.28 | 1.19 - 1.21 |

## API Version Conversion

CAPT v0.7.x introduces `infrastructure.cluster.x-k8s.io/v1beta2` as the new
storage (hub) API version. The previous `v1beta1` version remains served for
backward compatibility. CRD conversion webhooks handle automatic bi-directional
conversion between versions.

- **v1beta2** — The hub (storage) version. New fields like `tasks`, `templateInline`,
  `templateRef`, and `TinkerbellMachineConfig` live here. All resources are
  stored as v1beta2 in etcd.
- **v1beta1** — The spoke (served) version. Existing v1beta1 manifests and
  clients continue to work. Resources are converted on the fly by the webhook.

### Conversion Details

| v1beta1 Field | v1beta2 Field | Notes |
|---|---|---|
| `templateOverride` | `templateInline` | Renamed |
| `templateOverrideRef` | `templateRef` | Renamed, uses `ObjectRef` |
| *(none)* | `tasks` | v1beta2-only; kept in a conversion annotation, so a v1beta1 round trip preserves it |
| `status.instanceStatus` | `status.state` | Renamed, same underlying type |
| `imageLookup*` fields | *(removed)* | Dropped during up-conversion; not round-trippable |
| `TinkerbellMachineSpec` (in templates) | `TinkerbellMachineConfig` | `hardwareName`/`providerID` excluded from templates |
| *(none)* | `addressFromPool` | v1beta2-only; preserved across a v1beta1 round-trip through the conversion annotation. See [IPAM.md](IPAM.md) |

The conversion functions are plain exported functions in the `api/v1beta1`
package (e.g. `ConvertClusterToHub`, `ConvertClusterFromHub`). The conversion
webhook registration uses controller-runtime's registry-based mechanism in the
main module, keeping the API module free of controller-runtime dependencies.

## Contract vs API Version

These are independent concepts:

- **Contract version** (`v1beta2`): Defines the behavior and status fields CAPI expects from an infrastructure provider (e.g. `Initialization.Provisioned` status, typed webhook interfaces).
- **API group version** (`v1beta2`): The Kubernetes API version used in CRD definitions and manifests. This is the version used in `apiVersion:` fields of YAML resources.

The CRD labels map contract versions to API versions. For CAPT v0.7.x:

```yaml
# config/crd/kustomization.yaml
labels:
  - pairs:
      cluster.x-k8s.io/v1beta1: v1beta1
      cluster.x-k8s.io/v1beta2: v1beta2  # v1beta2 contract → v1beta2 API version
```

The label **key** is the contract version, the label **value** is the CRD API version that CAPI should use when accessing this provider's resources.

## What Changed in v0.7.x (v1beta2 Contract)

### API Type Changes

| Change | Before (v0.6.x) | After (v0.7.x) |
|---|---|---|
| CAPI import path | `sigs.k8s.io/cluster-api/api/v1beta1` | `sigs.k8s.io/cluster-api/api/core/v1beta2` |
| `ControlPlaneEndpoint` | required, value type | optional, pointer (`*clusterv1.APIEndpoint`) |
| `Machine.Spec.Version` | `*string` | `string` |
| `Cluster.Spec.InfrastructureRef` | `*corev1.ObjectReference` | `clusterv1.ContractVersionedObjectReference` |
| `Cluster.Spec.Paused` | `bool` | `*bool` |
| Webhook interfaces | `webhook.CustomValidator` / `webhook.CustomDefaulter` | `admission.Validator[T]` / `admission.Defaulter[T]` |
| Webhook builder | `ctrl.NewWebhookManagedBy(mgr).For(obj)` | `ctrl.NewWebhookManagedBy(mgr, obj)` |

### New Status Fields (Required by v1beta2 Contract)

Both `TinkerbellCluster` and `TinkerbellMachine` now include `Initialization` status:

```go
// TinkerbellClusterStatus
Initialization *TinkerbellClusterInitializationStatus

// TinkerbellClusterInitializationStatus
Provisioned *bool  // Set to true when infrastructure is ready

// TinkerbellMachineStatus
Initialization *TinkerbellMachineInitializationStatus

// TinkerbellMachineInitializationStatus
Provisioned *bool  // Set to true when machine is provisioned
```

The `Initialization.Provisioned` field is part of the CAPI contract and is used to orchestrate cluster/machine provisioning.

### Controller Changes

- `ClusterPausedTransitionsOrInfrastructureReady` predicate renamed to `ClusterPausedTransitionsOrInfrastructureProvisioned`
- Cluster readiness now checks both `Status.Ready` and `Status.Initialization.Provisioned`

### Manifests and Templates

New manifests should use `v1beta2` API versions:

```yaml
apiVersion: infrastructure.cluster.x-k8s.io/v1beta2
kind: TinkerbellCluster
```

Existing `v1beta1` manifests continue to work — the conversion webhook
automatically converts them to `v1beta2` for storage:

```yaml
# Still accepted
apiVersion: infrastructure.cluster.x-k8s.io/v1beta1
kind: TinkerbellCluster
```

CAPI v1.12 uses the CRD labels to determine which API version to use. Users
upgrading from v0.6.x do **not** need to update existing manifests, though
new manifests should prefer `v1beta2`.

### metadata.yaml

The `metadata.yaml` file maps release series to contracts for `clusterctl`:

```yaml
releaseSeries:
  - major: 0
    minor: 7
    contract: v1beta2  # New
  - major: 0
    minor: 6
    contract: v1beta1
```

### Dependencies

| Dependency | v0.6.x | v0.7.x |
|---|---|---|
| `sigs.k8s.io/cluster-api` | v1.10.4 | v1.12.5 |
| `sigs.k8s.io/controller-runtime` | v0.21.0 | v0.23.3 |
| `k8s.io/api` | v0.33.x | v0.35.x |
| `tinkerbell/api` | v0.22.x | v0.23.x |
| Go | 1.24.1 | 1.25.0 |
