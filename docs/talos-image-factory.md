# Talos Image Factory schematics (moved)

CAPT no longer resolves Talos Image Factory schematics. As of this change the provider carries
**zero Talos code**: no Factory client, no schematic engine, no version policy, and no
`status.{schematicID,installerImage,diskImageURL}` trio.

Image identity for Talos-on-Tinkerbell machines is owned end to end by the
**cluster-api-runtime-extensions-tinkerbell** provider:

- The `talos-image-resolver` component resolves a per-machine schematic and version and writes
  `Hardware.spec.metadata.instance.operating_system` (`slug`/`version`/`os_slug`) for claimed
  Hardware, plus the `talos.tinkerbell.org/installer-image` annotation that the Talos bootstrap
  provider reads for in-place upgrades.
- The provisioning Workflow template builds `IMG_URL` from those Hardware fields; a mandatory
  Workflow-CREATE admission gate holds the render until the block is present, because Workflows
  render exactly once.

See `docs/talos-image-resolver.md` and `docs/runtime-extensions-migration.md` in that repository.
The full previous design of the in-tree resolver — including the direct-installer template
experiment and the META `0xa` networking notes — is preserved in this file's git history
(pre-`sp9` revisions; tag `pre-sp9-resolution` redeploys a resolving CAPT if the cutover ever
needs to be rolled back).

What CAPT retains is deliberately generic: the provisioned short-circuit (a provisioned machine
is never re-imaged), `templateInline`/`templateRef` template flexibility, `hardwareAffinity`
claiming, `PROVIDER_ID`/`DISK_ID` substitution, and BMC-driven in-place recovery.
