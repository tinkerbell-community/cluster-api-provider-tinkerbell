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

### Alternative: run the installer directly

> **Unvalidated.** The template below has not been run against real hardware. It is recorded
> here because the argument surface has been confirmed against the Talos source, but the part
> that can only be proven on metal — whether the installer runs cleanly under HookOS's
> container runtime — has not been. Treat it as a starting point for a hardware test, not a
> working recipe.

The Talos installer is a container image, and it is the same image Talos runs against itself
during `talosctl upgrade`. Running it as a Tink action rather than streaming a raw disk image
means one artefact installs *and* upgrades a machine, so the two cannot drift:

```yaml
actions:
  - name: install-talos
    image: factory.talos.dev/metal-installer/{{ .schematicID }}:v1.14.0-rc.1
    command:
      - /bin/installer
      - install
      - --disk=/dev/nvme0n1
      - --platform=metal
      - --config=http://tootles.tinkerbell.svc/2009-04-04/user-data
```

`--config` is **not** the machine configuration. It is the value of the `talos.config` kernel
parameter — a URL the installed system fetches from at boot. Pointing it at Tootles works
because CAPT already writes the Talos machine configuration into `Hardware.spec.userData`,
and Tootles serves exactly that at `/2009-04-04/user-data`.

The installer *does* accept a machine configuration, but on **stdin**
(`configloader.NewFromStdin`), where it is used to validate the config and to read
`machine.install` settings. Tink actions have no mechanism to pipe data to a container, and
the installer image is minimal, so this template does not use it. The installer tolerates the
absence — it logs `machine configuration missing, skipping validation` and continues — but
note that the Talos source treats that as an upgrade-in-maintenance-mode path, so it is
tolerated rather than intended. The trade-off is losing install-time config validation; the
configuration is still validated when Talos applies it.

Serving the configuration rather than baking it in is deliberate. The in-place update path
regenerates the machine configuration and applies it over the Talos API, so a copy frozen onto
the disk at install time would become a second source of truth and go stale the first time
anyone changed a `configPatch`.

The full installer flag set, for reference:

| Flag | Meaning |
|---|---|
| `--disk` | Target disk |
| `--config` | Value of the `talos.config` kernel parameter |
| `--platform` | Value of the `talos.platform` kernel parameter |
| `--arch` | Target architecture, defaults to the runtime architecture |
| `--extra-kernel-arg` | Additional kernel argument, repeatable |
| `--upgrade` | The install is being performed by an upgrade |
| `--force` | Forcefully format the partition |
| `--zero` | Zero the disk before installing |
| `--meta` | A key/value pair for META |

`status.diskImageURL` is retained rather than removed so the `image2disk` route above remains
available as a fallback until this path has been proven on hardware.

## Networking before the machine configuration exists

Delivering the machine configuration over the network — whether by `talos.config=` or by
applying it in maintenance mode — assumes the machine already has working networking. On DHCP
that holds. It does not hold for a machine that needs a static address, a VLAN, or a bond to
reach anything, because that networking is described in the configuration it cannot yet
fetch.

Talos breaks that cycle with the `META` partition. Writing a serialized network configuration
to META key `0xa` gives the machine its networking before any configuration is fetched:

```
0x6  Upgrade                       0xb  DownloadURLCode
0x7  StagedUpgradeImageRef         0xc  UserReserved1
0x8  StagedUpgradeInstallOptions   0xd  UserReserved2
0x9  StateEncryptionConfig         0xe  UserReserved3
0xa  MetalNetworkPlatformConfig    0xf  UUIDOverride
```

(from `pkg/machinery/meta/constants.go`; the block starts at `iota + 6`.)

Key `0xa` holds a YAML-serialized `network.PlatformConfigSpec` — the same structure every
Talos platform produces from `NetworkConfiguration()`:

```yaml
addresses: []      # AddressSpecSpec
links: []          # LinkSpecSpec — bonds, VLANs, MTU
routes: []         # RouteSpecSpec
hostnames: []
resolvers: []
timeServers: []
operators: []      # e.g. DHCP per link
externalIPs: []
```

### Writing it

On the installer path, pass it directly — the installer takes repeatable `--meta` key/value
pairs:

```yaml
command:
  - /bin/installer
  - install
  - --disk=/dev/nvme0n1
  - --platform=metal
  - --config=http://tootles.tinkerbell.svc/2009-04-04/user-data
  - --meta=0xa=<serialized PlatformConfigSpec>
```

On the `image2disk` path the same value has to be written to the META partition by a
subsequent action, since nothing in the raw image knows about this machine.

Either way the value is per-machine, so it belongs in the Workflow template rather than in a
shared image, and it is not part of the schematic — the schematic describes the image, not the
machine.

> Note that the Image Factory also accepts `customization.meta` in a schematic. That is not a
> substitute: a schematic is shared by every machine resolving to the same ID, so it cannot
> carry per-machine networking. It is also ignored for installer and initramfs images.

### Why not write the machine configuration into STATE instead

Seeding `STATE` with the machine configuration from a Workflow removes the boot-time fetch
entirely, which is appealing for the same reason META is. Two things argue against it:

- It couples the Workflow to Talos' on-disk layout, which the installer otherwise owns.
- It breaks outright once `STATE` encryption is in use — which is precisely why
  `StateEncryptionConfig` exists as its own META key at `0x9`. A Workflow writing plaintext
  into a partition Talos intends to encrypt is writing something Talos will not read.

Staleness is *not* one of the arguments. Talos owns `STATE` after first boot, and an in-place
update applying a regenerated configuration over the Talos API persists it there, so an
install-time write would be a seed rather than a competing copy.

The combination that avoids both problems is META `0xa` for networking plus `talos.config=`
for the machine configuration: networking arrives before anything is fetched, the
configuration stays current because it is served rather than baked, and nothing reaches into
the STATE partition. Talos supports per-machine config URLs directly — META key `0xb`
(`DownloadURLCode`) supplies the `${code}` variable in a `talos.config=` URL.

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
