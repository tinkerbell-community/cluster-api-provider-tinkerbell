# Quick Start

In this tutorial we’ll cover the basics of how to use Cluster API to create one or more Kubernetes clusters.

## Try it with the Playground

If you want to quickly experiment with CAPT in a local virtual environment, see the
[CAPT Playground](https://github.com/tinkerbell/playground/tree/main/capt). It automates
the entire setup — including a KinD management cluster, Tinkerbell stack, virtual machines,
and a virtual BMC — so you can create a workload cluster with a single command:

```bash
task create-playground
```

The rest of this guide covers manual setup on real (or pre-existing virtual) infrastructure.

## Installation

### Prerequisites

- [kubectl] >= v1.33
- [clusterctl] >= v1.12
- [Helm] >= v3.13
- A running [Tinkerbell stack] from [tinkerbell-community] >= v0.27.0. CAPT reads and writes
  Hardware, Workflows, Tasks and bmc Jobs as `v1alpha2`, which earlier Tinkerbell releases do
  not serve (see [Compatibility](COMPATIBILITY.md#tinkerbell)).

### Initialize the management cluster

Now that we've got clusterctl installed and all the prerequisites in place, let's transform the Kubernetes cluster
into a management cluster by using `clusterctl init`. This only needs to be run once. For this quick start, run this against the same
Kubernetes cluster where the Tinkerbell stack is installed.

The command accepts as input a list of providers to install; when executed for the first time, `clusterctl init`
automatically adds to the list the `cluster-api` core provider, and if unspecified, it also adds the `kubeadm` bootstrap
and `kubeadm` control-plane providers.

`clusterctl` doesn't include the Tinkerbell CAPI provider (CAPT) so we need to configure this:

```sh
cat >> ~/.cluster-api/clusterctl.yaml <<EOF
providers:
  - name: "tinkerbell"
    url: "https://github.com/tinkerbell/cluster-api-provider-tinkerbell/releases/v0.7.0/infrastructure-components.yaml"
    type: "InfrastructureProvider"
EOF

# Initialize the management cluster
clusterctl init --infrastructure tinkerbell
```

The output of `clusterctl init` is similar to the following:

```shell
Fetching providers
Installing cert-manager Version="v1.17.2"
Waiting for cert-manager to be available...
Installing Provider="cluster-api" Version="v1.12.5" TargetNamespace="capi-system"
Installing Provider="bootstrap-kubeadm" Version="v1.12.5" TargetNamespace="capi-kubeadm-bootstrap-system"
Installing Provider="control-plane-kubeadm" Version="v1.12.5" TargetNamespace="capi-kubeadm-control-plane-system"
Installing Provider="infrastructure-tinkerbell" Version="v0.7.0" TargetNamespace="capt-system"

Your management cluster has been initialized successfully!

You can now create your first workload cluster by running the following:

  clusterctl generate cluster [name] --kubernetes-version [version] | kubectl apply -f -
```

### Create Hardware resources to make Tinkerbell Hardware available

Cluster API Provider Tinkerbell does not assume all hardware configured in Tinkerbell is available for provisioning.
To make Tinkerbell Hardware available create a `Hardware` resource in the management cluster.
Hardware objects should live in the same namespace as the Tinkerbell stack
(specifically where `tink-controller` lives). CAPT will automatically create
the Workflow, and the Tasks or Template it runs, in the Hardware's namespace,
even if the CAPI objects are in a different namespace.

CAPT works with `tinkerbell.org/v1alpha2` Hardware. Hardware written as `v1alpha1` is still
accepted: Tinkerbell converts and stores it as `v1alpha2`, and keeps what `v1alpha2` cannot
represent in its `conversion.tinkerbell.org/v1alpha1` annotation, which CAPT never touches.
The fields below are `v1alpha2` paths; the `v1alpha1` field each one converts from is in
[Compatibility](COMPATIBILITY.md#hardware-fields-capt-uses).

CAPT validates the following fields on each Hardware object during provisioning:

- `spec.networkInterfaces` — at least one interface must be defined
- `spec.networkInterfaces[<mac>].ipam.ipv4.address` (or `ipam.ipv6.address`) — the primary
  interface must have an IP address. The primary interface is the one `spec.agentID` names,
  or the lowest MAC (for Hardware created as `v1alpha1`: its first interface, as before).
- `spec.storageDevices` — at least one storage device must be defined

CAPT also requires the following field (used but not explicitly validated at reconciliation time):

- `spec.instance.lookupID` (or, when it is empty, `spec.agentID`) — must be set. CAPT maps this value to the `device_1` workflow variable, which identifies the Agent that runs every task of the Workflow. It is also used to construct per-hardware ISO URL paths when using ISO boot modes.

The `ipam.ipv4.hostname` field is optional but recommended.
The interface's `netboot` block and `labels` field are also optional. Labels are used
for [hardware affinity](#select-your-hardware) matching.

**NOTE:** If the Hardware has a BMC — `spec.bmc.connection`, or for Hardware created as
`v1alpha1`, a `spec.bmcRef` to a bmc `Machine` — CAPT uses it for boot mode
operations (netboot, ISO, custom) and for powering off the machine during
deletion, with bmc Jobs that reference the Hardware. If the Hardware has **no** BMC,
the `bootOptions.bootMode` setting on the `TinkerbellMachineTemplate` is silently
ignored and no power management is performed.

An example of a valid Hardware resource:

```yaml
apiVersion: tinkerbell.org/v1alpha2
kind: Hardware
metadata:
  name: node-1
  namespace: tink-system
  labels:
    tinkerbell.org/role: worker
spec:
  agentID: "aa:bb:cc:dd:ee:ff"
  arch: x86_64
  storageDevices:
    - name: /dev/sda
  instance:
    lookupID: "aa:bb:cc:dd:ee:ff"
  networkInterfaces:
    "aa:bb:cc:dd:ee:ff":
      dhcp:
        ipv4:
          leaseTimeSeconds: 86400
      ipam:
        ipv4:
          address: 192.168.1.10
          prefix: "24"
          gateway: 192.168.1.1
          hostname: node-1
          nameservers:
            - 8.8.8.8
      netboot: {}
  # Optional: a BMC enables boot modes and power off on deletion.
  # bmc:
  #   bootMode: UEFI
  #   connection:
  #     host: 192.168.1.100
  #     credentialsRef:
  #       name: node-1-bmc
  #       namespace: tink-system
```

**NOTE:** The `metadata.name` and the interface MACs must be unique across all Hardware objects.

### Create your first workload cluster

Once the management cluster is ready, you can create your first workload cluster.

#### Preparing the workload cluster configuration

The `clusterctl generate cluster` command returns a YAML template for creating a [workload cluster].

#### Required configuration for the Tinkerbell provider

Depending on the infrastructure provider you are planning to use, some additional prerequisites should be satisfied
before configuring a cluster with Cluster API. Instructions are provided for the Tinkerbell provider below.

Otherwise, you can look at the `clusterctl generate cluster` [command][clusterctl generate cluster] documentation for details about how to
discover the list of variables required by a cluster templates.

To see all required Tinkerbell environment variables execute:

```bash
clusterctl generate cluster --infrastructure tinkerbell --list-variables capi-quickstart
```

```bash
# Set CONTROL_PLANE_VIP to an available IP address for the network
# the machines will be provisioned on
export CONTROL_PLANE_VIP=192.168.1.110

# POD_CIDR is overridden here to avoid conflicting with the assumed
# Machine network of 192.168.1.0/24, this can be omitted if the
# Machine network does not conflict with the default of
# 192.168.0/0/16 or can be set to a different value if needed
export POD_CIDR=172.25.0.0/16

# SERVICE_CIDR can be overridden if the default of 172.26.0.0/16
# would interfere with the Machine network
#export SERVICE_CIDR=10.10.0.0/16
```

#### Generating the cluster configuration

For the purpose of this tutorial, we'll name our cluster capi-quickstart.

The `--target-namespace` specifies where CAPI objects (Cluster, Machine, etc.)
are created. CAPT will automatically create Tinkerbell resources (Template,
Workflow) in the same namespace as the selected Hardware, even if that differs
from the CAPI namespace. For the simplest setup, use the namespace where both
the Tink stack and Hardware objects live.

##### Cross-namespace support

CAPI objects and Tinkerbell objects do not need to be in the same namespace.
For example, you can have CAPI objects in a `capi-cluster` namespace while
Hardware lives in the `tinkerbell` namespace. CAPT will create Template,
Workflow, and Job resources in the Hardware's namespace and use label-based
watches to reconcile them back to the `TinkerbellMachine`.

When resources are in different namespaces, Kubernetes owner references are
not set (they cannot cross namespace boundaries). Cleanup relies on CAPT's
finalizer-driven deletion logic and ownership labels
(`capt.tinkerbell.org/machine-name`, `capt.tinkerbell.org/machine-namespace`).
If the CAPT controller is not running when a `TinkerbellMachine` is
force-deleted, orphaned resources may remain. See
[REMOTE-TINKERBELL.md](REMOTE-TINKERBELL.md#orphaned-resources) for cleanup
instructions.

```bash
clusterctl generate cluster capi-quickstart \
  --kubernetes-version v1.35.2 \
  --control-plane-machine-count=3 \
  --worker-machine-count=3 \
  --target-namespace=tink-system \
  > capi-quickstart.yaml
```

This creates a YAML file named `capi-quickstart.yaml` with a predefined list of Cluster API objects; Cluster, Machines,
Machine Deployments, etc.

The file can be eventually modified using your editor of choice.

See [clusterctl generate cluster] for more details.

#### Select your hardware

In the `capi-quickstart.yaml`, you'll see a `TinkerbellMachineTemplate` type where you can edit the `hardwareAffinity`
to enforce which CAPI machines will get provisioned on particular hardware.

```yaml
apiVersion: infrastructure.cluster.x-k8s.io/v1beta1
kind: TinkerbellMachineTemplate
metadata:
  name: capi-quickstart-md-0
  namespace: tink-system
spec:
  template:
    spec:
      hardwareAffinity:
        required:  # 'required' entries are OR'd
        - labelSelector:
            matchLabels:
              manufacturer: dell
            matchExpressions:
            - key: idracVersion
              operator: In
              values: ["7", "8"]
        preferred: # 'preferred' entries are sorted by weight and then sequentially evaluated
        - weight: 50
          hardwareAffinityTerm:
            labelSelector:
              matchLabels:
                rack: 1
                room: 2
```

#### Customize the provisioning template

CAPT needs to know what to run to provision each machine. There is no default: set `tasks` at
the cluster or machine level (or name Tasks on the Hardware, see below).

`tasks` is an ordered list of Tinkerbell `v1alpha2` Tasks. Each entry has a `name` and exactly
one of:

- `taskInline`: a Task definition. CAPT creates it as a Task named `<machine>-<name>`, owned by
  the machine and deleted with it.
- `taskRef`: an existing Task you manage (`name`, optional `namespace`, defaulting to the
  namespace the machine's Workflow is created in). CAPT never creates, changes or deletes it.

Inline Tasks and references can be mixed. CAPT creates one Workflow per machine that runs the
entries in order on the machine's Hardware, so what you write maps one to one onto a `v1alpha2`
Workflow. A list holds at most 32 entries.

Here is an example that streams an OS image to disk, configures cloud-init for the
Tinkerbell metadata service, and kexecs into the installed OS:

```yaml
apiVersion: infrastructure.cluster.x-k8s.io/v1beta2
kind: TinkerbellMachineTemplate
metadata:
  name: capi-quickstart-control-plane
  namespace: tink-system
spec:
  template:
    spec:
      hardwareAffinity:
        required:
        - labelSelector:
            matchLabels:
              tinkerbell.org/role: control-plane
      tasks:
      - name: provision
        bootOptions:
          toggleNetboot: true
          bootMode: netboot
        taskInline:
          volumes:
          - /dev:/dev
          actions:
          - name: "stream image"
            image: quay.io/tinkerbell/actions/oci2disk
            timeoutSeconds: 1200
            env:
              # Replace 'amd64' with the target node architecture (amd64 or arm64)
              IMG_URL: ghcr.io/tinkerbell/cluster-api-provider-tinkerbell/ubuntu:2404-v1.35.2-amd64.gz
              DEST_DISK: '{{ (index .hardware.spec.storageDevices 0).name }}'
              COMPRESSED: "true"
          - name: "add tink cloud-init config"
            image: quay.io/tinkerbell/actions/writefile
            timeoutSeconds: 90
            env:
              DEST_DISK: '{{ formatPartition (index .hardware.spec.storageDevices 0).name 3 }}'
              FS_TYPE: ext4
              DEST_PATH: /etc/cloud/cloud.cfg.d/10_tinkerbell.cfg
              UID: "0"
              GID: "0"
              MODE: "0600"
              DIRMODE: "0700"
              CONTENTS: |
                datasource:
                  Ec2:
                    metadata_urls: ["http://<TINKERBELL_VIP>:7080"]
                    strict_id: false
                system_info:
                  default_user:
                    name: tink
                    groups: [wheel, adm]
                    sudo: ["ALL=(ALL) NOPASSWD:ALL"]
                    shell: /bin/bash
                manage_etc_hosts: localhost
                warnings:
                  dsid_missing_source: off
          - name: "add tink cloud-init ds-config"
            image: quay.io/tinkerbell/actions/writefile
            timeoutSeconds: 90
            env:
              DEST_DISK: '{{ formatPartition (index .hardware.spec.storageDevices 0).name 3 }}'
              FS_TYPE: ext4
              DEST_PATH: /etc/cloud/ds-identify.cfg
              UID: "0"
              GID: "0"
              MODE: "0600"
              DIRMODE: "0700"
              CONTENTS: |
                datasource: Ec2
          - name: "kexec image"
            image: ghcr.io/jacobweinstock/waitdaemon:latest
            timeoutSeconds: 90
            namespaces:
              pid: host
            env:
              BLOCK_DEVICE: '{{ formatPartition (index .hardware.spec.storageDevices 0).name 1 }}'
              FS_TYPE: vfat
              IMAGE: quay.io/tinkerbell/actions/kexec
              WAIT_SECONDS: "5"
            volumes:
            - /var/run/docker.sock:/var/run/docker.sock
```

Tinkerbell renders the Tasks' Go templates with `v1alpha2` data when the Workflow starts:

| Expression | Value |
|---|---|
| `{{ (index .hardware.spec.storageDevices 0).name }}` | First disk device path (e.g. `/dev/sda`) |
| `{{ formatPartition (index .hardware.spec.storageDevices 0).name N }}` | Nth partition of the first disk |
| `{{ dig "spec" "instance" "userdata" "" .hardware }}` | The Hardware's user data, which CAPT sets to the machine's bootstrap data |
| `{{ .metadata_url }}` | The metadata URL Tinkerbell is configured with |
| `{{ .workflow.spec.globals.templateMap.device_1 }}` | The machine's Agent ID (the Hardware's `spec.instance.lookupID`, or `spec.agentID`) |

Set `timeoutSeconds` on every action: Tinkerbell's agent (v0.28.6) gives an action without one
no time at all, so it fails at once and the Workflow ends in `Timeout`.

A Task does not name a worker: CAPT runs every entry on the machine's Hardware and Agent. If
Tinkerbell cannot render a Task (for example, because it still uses `v1alpha1` syntax), CAPT
records a `WorkflowNotRendered` warning on the `TinkerbellMachine` with Tinkerbell's reason.

##### Boot options

Each entry can carry `bootOptions`, as each task of a `v1alpha2` Workflow does: the boot mode
and netboot toggle apply before that entry runs, and turning netboot off, ejecting the ISO and
customboot `postOperations` apply after it succeeds.

- **If any entry sets `bootOptions`**, every entry's are used exactly as written and entries
  without get none. CAPT adds nothing: no netboot toggle, no BMC check, no ISO URL rewrite.
  Template a per-machine ISO URL with Hardware data if you need one.
- **If no entry sets them**, the first entry gets the boot options CAPT has always applied:
  netboot toggled, plus the machine's `spec.bootOptions` boot mode when the Hardware has a BMC
  (see [Boot modes](#boot-modes)). With several entries, the "after" steps then run after the
  first one; set `bootOptions` on the entries to place them yourself. A `custombootConfig` with
  template expressions is refused here (`TemplatedCustombootConfig`): its `v1alpha1` data would
  render as `v1alpha2`.

When the entries set boot options, `spec.bootOptions` is ignored, and CAPT records a
`BootOptionsIgnored` warning if it is set.

##### Task and template selection precedence

When CAPT resolves what a machine runs, it evaluates these sources in order and uses the first
one found, whatever its style:

1. **`TinkerbellMachineTemplate.spec.template.spec.tasks`** (or the deprecated
   `templateInline`/`templateRef`) — set directly in the CAPI manifest.
2. **Hardware annotation `hardware.tinkerbell.org/capt-task-refs`** — comma-separated names of
   Tasks in the Hardware's namespace, run in order with the default boot options. The
   deprecated `hardware.tinkerbell.org/capt-template-override` names a `Template` instead; if
   both are set, the Tasks win and CAPT records a `ConflictingHardwareAnnotations` warning.
3. **`TinkerbellCluster.spec.tasks`** (or the deprecated `templateInline`/`templateRef`) —
   applied to all machines that don't match a higher-priority source.

If none of these sources provides Tasks or a template, the machine reconciliation fails with an
error. A referenced Task that does not exist stops the machine before anything is created, with
a `TaskNotFound` warning on the `TinkerbellMachine`.

##### From templateInline to tasks

`templateInline`, `templateRef` and the template override annotation are deprecated: the API
server returns a warning when they are set, but they keep working exactly as before (see
[How CAPT runs the template](#how-capt-runs-the-template)). To move a template to `tasks`,
make each template task an entry with `taskInline`, and:

| `v1alpha1` template | `taskInline` |
|---|---|
| `worker: "{{.device_1}}"` | remove it; CAPT assigns the Agent |
| `timeout: 90` | `timeoutSeconds: 90` |
| `environment:` | `env:` (values are strings: quote numbers and booleans) |
| `pid: host` | `namespaces: {pid: host}` |
| `command: [a, b, c]` | `command: a` and `args: [b, c]` |
| `global_timeout` | none; use each action's `timeoutSeconds` |
| `{{ index .Hardware.Disks 0 }}` | `{{ (index .hardware.spec.storageDevices 0).name }}` |
| `{{ formatPartition (index .Hardware.Disks 0) 3 }}` | `{{ formatPartition (index .hardware.spec.storageDevices 0).name 3 }}` |
| `{{ .Hardware.UserData }}` | `{{ dig "spec" "instance" "userdata" "" .hardware }}` |
| `{{ .Hardware.Metadata.Instance.ID }}` | `{{ dig "spec" "instance" "lookupID" "" .hardware }}` |
| `{{ .device_1 }}` (as a value) | `{{ .workflow.spec.globals.templateMap.device_1 }}` |

Move the machine's `spec.bootOptions` onto the first entry as `bootOptions` with
`toggleNetboot: true`, or leave it where it is and set no `bootOptions` on the entries. With the
CABPT in-place update extension installed, changing `tasks`, the templates or `bootOptions` of a
running machine does not roll it out: CAPT only reads them when it provisions a machine.

##### How CAPT runs the template

This applies to the deprecated `templateInline`, `templateRef` and template override
annotation only. A `tasks` list always runs natively, exactly as written.

A `v1alpha2` Workflow runs Tasks, not a Template. For each machine CAPT converts the template
the way Tinkerbell converts a `v1alpha1` Template (see Tinkerbell's
[v1alpha2 migration guide](https://github.com/tinkerbell-community/tinkerbell/blob/v0.27.0/docs/technical/v1alpha2/MIGRATION.md)):

- **Natively**, when the template converts: CAPT creates one `Task` per template task, named
  `<machine>-<task>`, and a Workflow that runs them. No `Template` is created. A template
  converts when its structure does not depend on templating: every `{{ }}` is a single
  expression inside a string value, there is no `range`, `if`, `with` or `define` around
  tasks or actions, no `toYaml`/`nindent`, and every variable it uses has a `v1alpha2`
  equivalent (`.device_1`, `index .Hardware.Disks N` (also inside `formatPartition`), `.Hardware.UserData`,
  `.Hardware.Metadata.Instance.ID`, ...). The template may have several tasks only when the
  machine has no boot options (CAPT always toggles netboot, so in practice: a single task).
- **In `v1alpha1` compatibility mode** otherwise: CAPT creates the machine's `Template`, as it
  did before, and a Workflow that runs it, which Tinkerbell renders exactly as it rendered
  `v1alpha1` Workflows. CAPT records why on the Workflow (annotation
  `capt.tinkerbell.org/v1alpha1-compatibility`) and as an event on the `TinkerbellMachine`.

Both modes provision the machine the same way; nothing needs to change in existing templates.
A machine whose `Template` an earlier CAPT created keeps running it.

##### Cloud-init integration

CAPT expects the provisioned OS to use [cloud-init](https://cloud-init.io/) with an
EC2-compatible metadata datasource. The provisioning template must write two configuration
files so that cloud-init discovers the Tinkerbell metadata service:

| File | Purpose |
|---|---|
| `/etc/cloud/cloud.cfg.d/10_tinkerbell.cfg` | Configures the `Ec2` datasource with the Tinkerbell metadata URL (port `7080`) and any default user settings. |
| `/etc/cloud/ds-identify.cfg` | Tells `ds-identify` to use the `Ec2` datasource (`datasource: Ec2`). |

Both files are shown in the example template above. Without them, cloud-init will not
contact the Tinkerbell metadata service and bootstrap data (kubeadm join tokens, etc.)
will not be applied to the machine.

**`PROVIDER_ID` placeholder:** CAPT looks for the literal string `PROVIDER_ID` in the
bootstrap cloud-config (from the CAPI `Machine`'s bootstrap `Secret`) and replaces it with
the actual provider ID (`tinkerbell://<namespace>/<hardware-name>`) before writing the
data to `Hardware.spec.instance.userdata` (`spec.userData` in `v1alpha1`). If you are using the default CAPI Kubeadm bootstrap
provider, this is handled automatically and no action is needed. This is only relevant
if you are using a custom (non-Kubeadm) bootstrap provider that generates its own
cloud-config — in that case, include the literal string `PROVIDER_ID` wherever the
provider ID is needed and CAPT will substitute the real value at provisioning time.
If you are unsure which bootstrap provider you are using, it is almost certainly the
Kubeadm bootstrap provider and you can ignore this.

##### Boot modes

The `bootOptions.bootMode` field controls how the machine boots into the provisioning
environment (HookOS). Available modes:

| Boot Mode | Description |
|---|---|
| `netboot` | PXE/iPXE network boot (default) |
| `isoboot` | Boot from an ISO image served by the Tinkerbell stack |
| `iso` | Deprecated alias for `isoboot`. Use `isoboot` instead. |
| `customboot` | Custom BMC boot actions |

**NOTE:** Boot modes only take effect when the Hardware has a BMC (see
[Hardware requirements](#create-hardware-resources-to-make-tinkerbell-hardware-available)).
When using `iso` or `isoboot`, the `bootOptions.isoURL` field is required. CAPT inserts
the Hardware's `spec.instance.lookupID` (with `:` replaced by `-`) into the ISO URL
path to enable per-hardware ISO customization. The `customboot` actions keep their `v1alpha1`
form in the `TinkerbellMachineTemplate`; CAPT converts each to the `v1alpha2` bmc operations
it sets. Customboot actions whose values are templated (for example a media URL that uses
`.Hardware.Interfaces`) make the machine's Workflow run in compatibility mode, where they are
rendered with `v1alpha1` Hardware data as before.

##### Provisioned annotation

When provisioning completes, CAPT sets the annotation
`v1alpha1.tinkerbell.org/provisioned: "true"` on the Hardware object. This is how CAPT
determines that the machine is ready. Do not set or remove this annotation manually
unless you are troubleshooting a stuck machine.

For a complete working example including kube-vip, SSH keys, and kustomize overlays,
see the [CAPT Playground templates](https://github.com/tinkerbell/playground/tree/main/capt/templates).

#### Apply the workload cluster

When ready, run the following command to apply the cluster manifest.

```bash
kubectl apply -f capi-quickstart.yaml
```

The output is similar to this:

```bash
cluster.cluster.x-k8s.io/capi-quickstart created
tinkerbellcluster.infrastructure.cluster.x-k8s.io/capi-quickstart created
kubeadmcontrolplane.controlplane.cluster.x-k8s.io/capi-quickstart-control-plane created
tinkerbellmachinetemplate.infrastructure.cluster.x-k8s.io/capi-quickstart-control-plane created
machinedeployment.cluster.x-k8s.io/capi-quickstart-md-0 created
tinkerbellmachinetemplate.infrastructure.cluster.x-k8s.io/capi-quickstart-md-0 created
kubeadmconfigtemplate.bootstrap.cluster.x-k8s.io/capi-quickstart-md-0 created
```

#### Accessing the workload cluster

The cluster will now start provisioning. You can check status with:

```bash
kubectl get cluster
```

You can also get an "at glance" view of the cluster and its resources by running:

```bash
clusterctl describe cluster capi-quickstart
```

To verify the first control plane is up:

```bash
kubectl get kubeadmcontrolplane
```

You should see an output is similar to this:

```bash
NAME                            INITIALIZED   API SERVER AVAILABLE   VERSION   REPLICAS   READY   UPDATED   UNAVAILABLE
capi-quickstart-control-plane   true                                 v1.35.2   3                  3         3
```

**NOTE** The control plane won't be `Ready` until we install a CNI in the next step.

After the first control plane node is up and running, we can retrieve the [workload cluster] Kubeconfig:

```bash
clusterctl get kubeconfig capi-quickstart > capi-quickstart.kubeconfig
```

### Deploy a CNI solution

For the purposes of this quick start we will deploy Cilium as a CNI solution,
if deploying another CNI solution, you may need to make configuration changes
either in the CNI deployment or in the Cluster deployment manifests to ensure
a working cluster.

```bash
helm repo add cilium https://helm.cilium.io/
helm install cilium cilium/cilium --version 1.17.3 \
  --namespace kube-system \
  --kubeconfig=capi-quickstart.kubeconfig
```

### Clean Up

Delete workload cluster.

```bash
kubectl delete cluster capi-quickstart
```

**IMPORTANT:** Always delete the Cluster object (`kubectl delete cluster <name>`) rather than
deleting the full manifest (`kubectl delete -f capi-quickstart.yaml`). CAPI's Cluster controller
orchestrates cascading deletion in the correct order — Machines are drained and deprovisioned
before infrastructure is torn down, and Tinkerbell resources (Workflows, Tasks, Templates) are cleaned
up by CAPT's finalizers. Deleting with `-f` sends parallel delete requests that can bypass this
ordering and leave Hardware stuck with finalizers or orphaned Workflows.

**NOTE:** The OS images used in this quick start are available in the [Tinkerbell container registry](https://github.com/orgs/tinkerbell/packages?repo_name=cluster-api-provider-tinkerbell).

<!-- links -->
[bootstrap cluster]: https://cluster-api.sigs.k8s.io/reference/glossary.html#bootstrap-cluster
[clusterctl generate cluster]: https://cluster-api.sigs.k8s.io/clusterctl/commands/generate-cluster.html
[clusterctl get kubeconfig]: https://cluster-api.sigs.k8s.io/clusterctl/commands/get-kubeconfig.html
[clusterctl]: https://cluster-api.sigs.k8s.io/user/quick-start.html#install-clusterctl
[Docker]: https://docs.docker.com/get-docker/
[infrastructure provider]: https://cluster-api.sigs.k8s.io/reference/glossary.html#infrastructure-provider
[kind]: https://kind.sigs.k8s.io/docs/user/quick-start
[KubeadmControlPlane]: https://cluster-api.sigs.k8s.io/developer/architecture/controllers/control-plane.html
[kubectl]: https://kubernetes.io/docs/tasks/tools/#kubectl
[management cluster]: https://cluster-api.sigs.k8s.io/reference/glossary.html#management-cluster
[provider]: https://cluster-api.sigs.k8s.io/reference/providers.html
[provider components]: https://cluster-api.sigs.k8s.io/reference/glossary.html#provider-components
[workload cluster]: https://cluster-api.sigs.k8s.io/reference/glossary.html#workload-cluster
[Helm]: https://helm.sh/docs/intro/install/
[Tinkerbell]: https://tinkerbell.org
[Tinkerbell stack]: https://github.com/tinkerbell/tinkerbell/tree/main/helm/tinkerbell#readme
[tinkerbell-community]: https://github.com/tinkerbell-community/tinkerbell
