# Cluster API IPAM

CAPT can claim a machine's address from a [Cluster API IPAM](https://cluster-api.sigs.k8s.io/tasks/experimental-features/ipam)
pool instead of expecting it to be typed into the `Hardware` record by hand.

Name a pool on the machine template:

```yaml
apiVersion: infrastructure.cluster.x-k8s.io/v1beta2
kind: TinkerbellMachineTemplate
metadata:
  name: workers
spec:
  template:
    spec:
      addressFromPool:
        apiGroup: ipam.cluster.x-k8s.io
        kind: InClusterIPPool
        name: lab-pool
```

Any provider implementing the contract works; the reference is the pool's own group and
kind. With the [UniFi provider](https://github.com/ubiquiti-community/cluster-api-ipam-provider-unifi):

```yaml
      addressFromPool:
        apiGroup: unifi.ipam.cluster.x-k8s.io
        kind: IPPool
        name: lab-pool
```

## What happens

1. CAPT selects Hardware for the machine as usual.
2. It creates an `IPAddressClaim` named `<hardware>-<pool>` in the machine's namespace,
   controller-owned by the `TinkerbellMachine`, with `spec.clusterName` and the
   `cluster.x-k8s.io/cluster-name` label set.
3. The IPAM provider binds an `IPAddress` to the claim.
4. CAPT writes the address, prefix and gateway into
   `spec.networkInterfaces[<mac>].ipam.ipv4` on the Hardware (`ipam.ipv6` for an IPv6
   address), on the interface keyed by the MAC recorded on the claim. smee then hands the
   machine that address. (A v1alpha1 client sees it as the interface's `dhcp.ip`, with the
   prefix as a netmask, where CAPT wrote it before.)
5. `status.addresses` reports it, and provisioning continues.

Until step 3 completes the machine waits, with:

```yaml
status:
  conditions:
  - type: IPAddressClaimed
    status: "False"
    reason: WaitingForIPAddress
```

## Which interface

The primary interface, the same one CAPT reports the node address from. v1alpha2 Hardware
keys its interfaces by MAC, so they have no order; the primary interface is:

1. for Hardware created through v1alpha1, its first v1alpha1 interface that has a MAC, which
   is the interface CAPT used before (the v1alpha1 order survives in the conversion
   annotation);
2. otherwise the interface `spec.agentID` names;
3. otherwise the interface with the lowest MAC.

Its MAC is recorded on the claim as `capt.tinkerbell.org/mac-address` and is the key the
address is written back under, so a later change of primary interface does not move an
existing reservation.

## What the claim tells the provider

CAPT also annotates the claim in provider-neutral terms, so an IPAM provider that can act
on them does not have to know CAPT:

| Annotation | Value |
| --- | --- |
| `ipam.cluster.x-k8s.io/mac-address` | the interface's MAC, for a provider that can reserve by MAC |
| `ipam.cluster.x-k8s.io/hostname` | the Hardware name, which is what Tinkerbell already hands the node as its hostname |

The hostname is the Hardware's, not the Machine's: Machines are renamed by every rollout
while the box keeps its name, and CAPI links the two by `providerID`, never by name.
Claims made by an earlier CAPT get both annotations on their next reconcile.

One address per machine. A v1alpha2 interface can hold an IPv4 and an IPv6 configuration, but
CAPT claims a single address, of whichever family the pool hands out.

## Ownership of the address

When `addressFromPool` is set, CAPT owns the `address`, `prefix` and `gateway` of that
interface's `ipam.ipv4` (or `ipam.ipv6`): it writes them when the claim is bound, re-asserts
them on every reconcile, and clears them when the machine is deleted, in the same patch that
clears `spec.instance.userdata`. Do not also set them by hand. The rest of that IP
configuration (hostname, nameservers, NTP servers, VLAN, routes, search domains) and the other
address family are not CAPT's: it never writes them, and every write is a merge patch of the
three fields only.

Without `addressFromPool` nothing changes: CAPT reads the address and never writes it.

## Why the claim is named after the Hardware

The Hardware, not the machine, is the stable thing in a bare-metal cluster. Naming the claim
`<hardware>-<pool>` means the same box gets the same claim name when its machine is
replaced, which is what pool-side pinning keys on (for example `spec.preAllocations` on the
UniFi provider's `IPPool`, or metal3's index-based data names).

A same-named Hardware in two Tinkerbell namespaces claimed from the same CAPT namespace
would collide; CAPT detects it (the claim records the Hardware it was made for) and reports
`IPAddressClaimInvalid` rather than adopting the wrong claim.

## Conditions

| Reason | Meaning |
| --- | --- |
| `IPAddressClaimed` | Address reserved on the Hardware |
| `WaitingForIPAddress` | Claim exists; provider has not bound an address |
| `IPAddressClaimDeleting` | Someone deleted the claim while the machine runs. CAPT will not create a replacement, since that could change a running node's address. Recreate the machine, or restore the claim. |
| `IPAddressClaimInvalid` | The message says what is wrong: claim collision, an IPv4 address without a prefix, or a MAC no longer present on the Hardware |

## Release

On machine deletion, after the hardware is powered off, CAPT clears the reservation, removes
its finalizer from the claim, and deletes the claim. The provider releases the address
asynchronously.

## Interaction with other Hardware writers

`spec.networkInterfaces` is a map, so a merge patch of one interface's address leaves the
rest of the Hardware alone. CAPT still writes the address with an optimistic lock: a
concurrent write to the Hardware (another controller re-applying it, an interface removed
meanwhile) makes CAPT retry against the current object rather than recreate what was
removed.

Unlike v1alpha1's `spec.interfaces`, which was an atomic list under server-side apply, the
map and its fields are granular: CAPT's patches make it the owner of the fields it writes, and
an apply by another controller (the Tinkerbell BMC discovery controller, say) that does not set
them leaves them alone. A controller that does set an interface's address takes it over, and
conflicts with CAPT's ownership unless it forces the apply.

## Immutability

`addressFromPool` cannot change once `spec.hardwareName` is set.
