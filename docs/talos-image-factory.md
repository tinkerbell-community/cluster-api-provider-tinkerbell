# Talos Image Factory schematics

CAPT can resolve a [Talos Image Factory](https://factory.talos.dev) schematic from a machine's
actual hardware and publish the resulting image references on `TinkerbellMachine.status`.

This answers two questions that otherwise have to be answered by hand, and kept in sync:

- **Initial installation** — which disk image should the provisioning Workflow write?
- **Upgrading** — which installer image should Talos upgrade itself to?

Both derive from one schematic ID, so a machine is installed and upgraded with the same set
of system extensions.

## What gets resolved

| Status field | Purpose |
|---|---|
| `status.schematicID` | The resolved Image Factory schematic |
| `status.installerImage` | `factory.talos.dev/metal-installer/<id>:<version>` — belongs in `machine.install.image` |
| `status.diskImageURL` | `https://factory.talos.dev/image/<id>/<version>/metal-<arch>.raw.xz` — written by the Workflow |

Schematic IDs are content-addressed, so identical hardware always resolves to the same ID and
a machine's image reference does not churn between reconciles.

## Rules

Built-in, derived from signals Tinkerbell actually records on `Hardware`:

- Any disk device under `/dev/nvme*` adds `siderolabs/nvme-cli`
- `spec.interfaces[].dhcp.arch` selects the disk image architecture

> `siderolabs/nvme-cli` provides NVMe *tooling*. Talos has NVMe support in the kernel, so a
> node boots from NVMe without it — this makes such a node debuggable, it does not enable it.

Anything that depends on knowledge `Hardware` does not carry — GPU models, specific NIC
modules, CPU vendor — is declared explicitly rather than guessed:

```yaml
metadata:
  annotations:
    talos.tinkerbell.org/system-extensions: siderolabs/gvisor,siderolabs/intel-ucode
```

The annotation is read from both `Hardware` and `TinkerbellMachine`, and unioned with the
built-in rules, so a workload can request an extension without editing a shared hardware
record.

## Talos version

The OS version comes from `talosVersion` on the machine's bootstrap config, read generically
so any bootstrap provider exposing `spec.talosVersion` works.

It must be a **complete** version — `v1.14.0` or `v1.14.0-rc.1`. A bare minor like `v1.13` is
a Talos config contract, not an OS version, and cannot be used to pull an image. When no full
version is available, resolution is skipped and status is left empty rather than guessing:
pulling the wrong OS version is worse than letting the template's own default apply.

## Using it in a Workflow template

The resolved values are exposed as Workflow template data:

```yaml
actions:
  - name: stream-talos
    image: quay.io/tinkerbell/actions/image2disk
    environment:
      IMG_URL: "{{ .diskImageURL }}"
      DEST_DISK: /dev/nvme0n1
      COMPRESSED: "true"
```

A template that does not reference these keys is unaffected — they are simply absent when
resolution has not run.

## Feeding the machine configuration

CABPT reads `status.installerImage` from the referenced InfraMachine and injects it as
`machine.install.image`. It does this generically via unstructured access, so the bootstrap
provider never imports Tinkerbell and any infrastructure provider publishing the same status
field gets identical behaviour.

The injected value is applied *before* any `strategicPatches` in the TalosConfig, so an
explicit patch still wins. The resolved image is a good default, not an override of intent.

Once `machine.install.image` is set, the in-place update extension uses it to decide whether
a Talos upgrade is required — see `docs/in-place-updates.md` in the bootstrap provider.

### Ordering

CAPT waits for bootstrap data before provisioning, so the machine configuration is generated
*before* hardware is selected, and the schematic is not knowable at first generation. This is
not a problem in practice because the two paths differ:

- **Initial install** — the Workflow writes the disk using `{{ .diskImageURL }}`, by which
  point hardware is known.
- **Upgrade** — comes from `machine.install.image`, which the in-place regeneration path
  rewrites once the InfraMachine publishes the image.

The configuration therefore catches up on the first reconcile after hardware selection.

## Configuration

```
--image-factory-url   Image Factory to use (default https://factory.talos.dev)
```

Resolution requires registering the schematic with the factory before images can be pulled —
the ID is deterministic, but the factory will not serve an unknown one. That means an
outbound call from the management cluster. Point `--image-factory-url` at a self-hosted
factory for airgapped installs. Results are cached on content hash, so a steady-state
reconcile loop makes no calls at all.

## Limitations

- Installer and initramfs images support **system extensions only**; the Image Factory
  ignores `extraKernelArgs` and `meta` for them. Those apply to boot media (ISO/raw) built
  from the same schematic ID.
- Talos version detection requires a full version in `talosVersion`.
- Only official extensions are supported; custom extension images are not modelled.
- Not validated against real hardware or a live Image Factory — the factory interaction is
  covered by tests against a fake HTTP server.
