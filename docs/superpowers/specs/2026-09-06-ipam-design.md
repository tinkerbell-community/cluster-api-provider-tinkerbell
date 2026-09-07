# Cluster API IPAM support for TinkerbellMachine

**Status:** approved design, 2026-09-06
**Scope:** `cluster-api-provider-tinkerbell` (CAPT)

## Problem

A Tinkerbell `Hardware` record is the DHCP reservation smee serves to a machine: the
address in `spec.interfaces[].dhcp.ip` is the address the machine boots with, provisions
with, and joins the cluster with. Today that address is typed by hand (or by a discovery
tool) before CAPT ever sees the Hardware, so the addresses of an entire cluster are managed
outside Cluster API.

Cluster API defines an IPAM contract: a consumer creates an `IPAddressClaim` against a pool,
and an IPAM provider (in-cluster, UniFi, Infoblox, …) answers with an `IPAddress` carrying
`address`, `prefix` and `gateway`. metal3 and CAPV both consume it. CAPT should too.

## Goal

A `TinkerbellMachine` can name an IPAM pool. CAPT claims an address from it for the
selected Hardware's primary interface, writes the result into that interface's DHCP
reservation (matched by MAC address), reports it as the node address, and releases the
claim when the machine goes away.

## Non-goals

- Multiple interfaces or more than one address per interface. A Tinkerbell `Interface` has
  one `dhcp.ip`; dual-stack cannot be expressed on the Hardware side today.
- DNS servers, hostnames, VLANs or routes from IPAM. The CAPI `IPAddress` type carries none of
  them.
- Changing a live machine's pool. The field is frozen once Hardware is selected.
- Exposing the allocated address to Workflow templates (a natural follow-up for writing a
  static Talos network config into META; see "Follow-ups").

## Design

### API (`api/v1beta2`)

One new field on `TinkerbellMachineConfig`, so it is settable on `TinkerbellMachine` and on
`TinkerbellMachineTemplate.spec.template.spec`:

```go
// AddressFromPool names a Cluster API IPAM pool an address is claimed from for the
// machine's primary network interface (spec.interfaces[0] on the selected Hardware).
// The allocated address, netmask and gateway are written to that interface's DHCP
// reservation and reported as the machine's address. Immutable once Hardware is selected.
// +optional
AddressFromPool *ipamv1.IPPoolReference `json:"addressFromPool,omitempty"`
```

`ipamv1.IPPoolReference` is `sigs.k8s.io/cluster-api/api/ipam/v1beta2.IPPoolReference`
(`apiGroup`, `kind`, `name`, all required). Reusing it means the field is literally the
claim's `spec.poolRef`, and the CRD inherits upstream validation.

Singular rather than a list: a Tinkerbell interface has one `dhcp.ip`. A list would promise
something the target cannot hold.

A new condition type and reasons live next to the finalizer constants:

| Condition | Status | Reason | Meaning |
|---|---|---|---|
| `IPAddressClaimed` | True | `IPAddressClaimed` | Address allocated and written to the Hardware |
| `IPAddressClaimed` | False | `WaitingForIPAddress` | Claim exists, provider has not bound an address yet |
| `IPAddressClaimed` | False | `IPAddressClaimDeleting` | Claim has a deletion timestamp; CAPT will not recreate it |
| `IPAddressClaimed` | False | `IPAddressClaimInvalid` | Claim name collision, missing prefix, MAC not on Hardware, … (message says which) |

The condition is absent when `addressFromPool` is unset.

New finalizer: `infrastructure.cluster.x-k8s.io/ipaddressclaim`, placed on claims CAPT
creates.

### Claim

Created in the `TinkerbellMachine`'s namespace on the management cluster (IPAM providers run
there; this is independent of external-Tinkerbell mode).

| Field | Value | Why |
|---|---|---|
| `name` | `<hardware.name>-<pool.name>` | The Hardware is the stable slot, as `Metal3Data` is in metal3. The same box keeps the same claim name across machine replacement, which is what pool-side pinning (e.g. UniFi `preAllocations`) keys on. |
| `spec.poolRef` | `addressFromPool` verbatim | |
| `spec.clusterName` | owning `Cluster` name | Providers pause on a paused cluster |
| label `cluster.x-k8s.io/cluster-name` | owning `Cluster` name | `clusterctl move`, provider filtering |
| labels `capt.tinkerbell.org/machine-name`, `…/machine-namespace` | the TinkerbellMachine | same convention as Workflows and Jobs |
| annotation `capt.tinkerbell.org/hardware-name`, `…/hardware-namespace` | selected Hardware | collision detection (below) |
| annotation `capt.tinkerbell.org/mac-address` | primary interface MAC | the join key back to the Hardware; also available to providers that can reserve by MAC |
| controller `ownerReference` | the TinkerbellMachine | garbage collection; owner-based watch |
| finalizer | `infrastructure.cluster.x-k8s.io/ipaddressclaim` | see "Claim lifetime" |

Primary interface MAC is `spec.interfaces[0].dhcp.mac`. An interface without a MAC cannot
be reserved for, so a missing MAC is an error, not a silent skip.

### Reconcile flow

`machineReconcileScope.Reconcile` becomes:

```
hw := ensureHardware()        // select, claim, providerID, targetNamespace, userData (unchanged)
reconcileIPAM(hw)             // new; may return errWaitingForIPAddress
setStatus(hw)                 // moved out of ensureHardware; needs the address
reconcile(hw)                 // unchanged
```

`reconcileIPAM(hw)`:

1. No `addressFromPool` → return.
2. Get the claim by its deterministic name.
   - Not found → create it (all metadata above) and treat as pending.
   - Found with a deletion timestamp → condition `IPAddressClaimDeleting`, return
     `errWaitingForIPAddress`. Never recreate: the machine may be running on that address.
   - Found but its controller owner is a different UID, or its hardware annotations name a
     different Hardware → condition `IPAddressClaimInvalid`, return an error (surfaces a
     same-name Hardware in two Tinkerbell namespaces, or an orphan from a force-deleted
     machine).
3. `status.addressRef.name` empty → condition `WaitingForIPAddress`, return
   `errWaitingForIPAddress`.
4. Get the `IPAddress`. Translate to `tinkv1.IP`:
   - `Address` verbatim.
   - IPv4: `Netmask` dotted-decimal from `prefix` (`/24` → `255.255.255.0`), `Family: 4`.
     A nil prefix is `IPAddressClaimInvalid`: smee refuses an IPv4 reservation without a
     netmask, so writing one would break DHCP for the machine.
   - IPv6: `Netmask` empty (smee rejects it), `Family: 6`.
   - `Gateway` verbatim (may be empty).
5. Find the Hardware interface whose `dhcp.mac` equals the MAC recorded on the claim
   (case-insensitive). None → `IPAddressClaimInvalid`, return an error. This is the
   "matching the MAC address" step: the write goes to the NIC the claim was made for, never
   to whatever happens to be first in the list.
6. If `dhcp.ip` already equals the desired value → no write. Otherwise patch the Hardware
   with the same `patch.Helper` idiom `ensureHardwareUserData` uses.
7. Condition `IPAddressClaimed=True`.

The top-level `Reconcile` maps `errWaitingForIPAddress` to `RequeueAfter: 30s` as a backstop;
the fast path is an owner-based watch on `IPAddressClaim`, which fires when the provider
sets `status.addressRef`.

`reconcileIPAM` runs on every reconcile, including for provisioned machines, so a foreign
write that drops `dhcp.ip` is re-asserted at the next reconcile (see "Known interaction").

### Release

In `DeleteMachineWithDependencies`, after power-off and before the machine finalizer is
removed, on every path (Hardware present or not):

- `releaseHardware` (Hardware present): when `addressFromPool` is set, clear `dhcp.ip` on
  the primary interface in the same patch that clears `userData` and the owner labels. CAPT
  is the sole writer of that field whenever `addressFromPool` is set, so release and scrub
  stay atomic, exactly as for `userData`.
- `releaseIPAddressClaim`: remove CAPT's finalizer from the claim and delete it.
  Not-found is success. CAPT does not wait for the provider's own finalizer; the address is
  released asynchronously, the same as CAPV.

### Claim lifetime

The CAPT finalizer exists for one reason: if someone deletes the claim while the machine is
running, the provider releases the address, and without the finalizer the claim object
vanishes and CAPT would create a new one, possibly receiving a different address and
rewriting the running node's reservation. With the finalizer the claim lingers with a
deletion timestamp, CAPT reports `IPAddressClaimDeleting` and does nothing. The operator
decides.

### Webhook

- `addressFromPool` cannot change once `spec.hardwareName` is set (a claim may exist).
- Required sub-fields are enforced by the CRD schema inherited from `IPPoolReference`.

### Conversion

`addressFromPool` is v1beta2-only. `ConvertMachineToHub` and `ConvertMachineTemplateToHub`
restore it from the stashed hub data, like `templateRef`. The existing fuzz round-trip
tests cover it.

### Wiring

- `main.go`: `ipamv1.AddToScheme`.
- Machine controller: `Watches(&ipamv1.IPAddressClaim{}, EnqueueRequestForOwner(TinkerbellMachine, OnlyControllerOwner))`.
- RBAC markers: `ipam.cluster.x-k8s.io` `ipaddressclaims` (get, list, watch, create, update,
  patch, delete) and `ipaddresses` (get, list, watch).
- The IPAM CRDs ship with core Cluster API (≥ v1.2), which CAPT already requires, so the
  watch does not need a CRD-presence guard.

### Documentation

`docs/IPAM.md`: what it does, an example `TinkerbellMachineTemplate` with a UniFi pool and
one with `InClusterIPPool`, the ownership rule for `dhcp.ip`, claim naming, conditions, and
the known interaction below. Linked from `README.md`.

## Known interaction: discovery controller re-applies `spec.interfaces`

The Hardware CRD's `spec.interfaces` has no `x-kubernetes-list-type`, so it is atomic under
server-side apply. The discovery syncer (`tinkerbell-bmc-discovery-controller`,
`internal/sync/syncer.go`) applies `interfaces[0]` with `dhcp.mac` and `dhcp.hostname` and
deliberately carries the live `netboot` forward because of exactly this atomicity. It does
not carry `dhcp.ip`, so its periodic re-apply will drop the address CAPT wrote.

CAPT re-asserts the address on its next reconcile, but that is bounded by `--sync-period`.
The correct fix is in the syncer's `carry` closure: carry `dhcp.ip` forward alongside
`netboot`. That is a companion change in that repository, outside this spec.

## Testing

Unit (`controller/machine`, fake client, gomega, matching the existing style):

- `reconcileIPAM` creates the claim with the expected name, poolRef, clusterName, labels,
  annotations, controller owner and finalizer, sets `WaitingForIPAddress`, and returns
  `errWaitingForIPAddress`.
- Bound claim: the Hardware interface matching the recorded MAC (placed at index 1 to prove
  matching is by MAC, not position) receives address, dotted netmask, gateway and family;
  condition is True.
- IPv6 address: empty netmask, family 6.
- Idempotent: a second pass with the Hardware already correct does not bump its
  `resourceVersion`.
- Nil prefix on an IPv4 address, MAC not present on Hardware, claim owned by a different
  machine, claim naming a different Hardware: each yields `IPAddressClaimInvalid` and an
  error.
- Terminating claim: not recreated, `IPAddressClaimDeleting`, wait error.
- `prefixToNetmask` table test.
- End-to-end through `TinkerbellMachineReconciler.Reconcile`: first pass creates the claim
  and requeues without error; after simulating the provider (create `IPAddress`, set
  `status.addressRef`), second pass writes the Hardware, sets `status.addresses`, and
  creates Template and Workflow as before.
- Deletion: claim finalizer removed and claim deleted; `dhcp.ip` cleared on the Hardware;
  existing release assertions still hold.
- Without `addressFromPool`: no claim, no condition, existing tests unchanged.

Webhook: rejects a pool change once `hardwareName` is set; allows it before.

Conversion: existing fuzz round-trip.

`make generate` output (CRDs, RBAC, deepcopy) is committed; `make lint` and `make test`
pass.

## Follow-ups (not in this change)

- Discovery syncer: carry `dhcp.ip` forward (see above).
- Expose the allocated address, netmask and gateway as Workflow template data so a template
  can write a static Talos network config into META 0xa.
- A UniFi IPAM provider option to reserve on the MAC from `capt.tinkerbell.org/mac-address`
  instead of a synthetic one.
- Per-interface pools once the Hardware API can express more than one address per interface.
