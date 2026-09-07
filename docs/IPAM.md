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
4. CAPT writes the address, netmask (from the prefix) and gateway into
   `spec.interfaces[].dhcp.ip` on the Hardware, on the interface whose `dhcp.mac` matches the
   MAC recorded on the claim. smee then hands the machine that address.
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

The primary interface, `spec.interfaces[0]`, the same one CAPT already reports the node
address from. Its `dhcp.mac` must be set; that MAC is recorded on the claim as
`capt.tinkerbell.org/mac-address` and is the key the address is written back under. A
provider that can reserve by MAC may use the annotation.

One address per machine. A Tinkerbell interface holds one `dhcp.ip`, so dual-stack is not
expressible on the Hardware side today.

## Ownership of `dhcp.ip`

When `addressFromPool` is set, CAPT owns `dhcp.ip` on that interface: it writes it when the
claim is bound, re-asserts it on every reconcile, and clears it when the machine is deleted,
in the same patch that clears `spec.userData`. Do not also set it by hand.

Without `addressFromPool` nothing changes: CAPT reads `dhcp.ip` and never writes it.

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

`spec.interfaces` is an atomic list under server-side apply. A controller that applies the
interfaces list (the Tinkerbell BMC discovery controller does) must carry the live `dhcp.ip`
forward, as it already does for `netboot`, or each re-apply drops the reservation until
CAPT's next reconcile.

## Immutability

`addressFromPool` cannot change once `spec.hardwareName` is set.
