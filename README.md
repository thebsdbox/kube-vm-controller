# kube-vm-controller

A Kubernetes controller that runs virtual machines with [kube-vm](https://github.com/thebsdbox/kube-vm).
Apply a `VirtualMachine` object and the controller picks a node, starts the VM through that node's
`kube-vm --server` daemon, and keeps it in the desired state.

> **Status:** alpha (`vm.kube-vm.io/v1alpha1`). The controller logic is covered by unit tests and was
> exercised end to end against a real API server with an in-memory fake daemon. Booting real guests
> requires KVM and has **not** been tested here.

## How it works

One binary, two controllers:

| Component | Runs as | Job |
|---|---|---|
| **Scheduler** (`--scheduler`) | Deployment, leader-elected | Assigns an unplaced VM to the Ready node with the fewest VMs. Candidates are nodes with a Ready agent pod, Ready and schedulable, matching `spec.nodeSelector`. Result goes to `status.nodeName`. |
| **Agent** (`--agent`) | DaemonSet on nodes labelled `kube-vm.io/enabled=true`, as root | Talks to the node's kube-vm control socket (`/run/kube-vm/control.sock`) to start, pause, resume, stop and delete the VMs assigned to its node, and writes observed state back to `status`. |

A VM is bound to its node for life, because its state (disks, overlays, RAM) lives on that host.
Pin a VM with `spec.nodeName` to skip the scheduler.

### Lifecycle

- A finalizer (`vm.kube-vm.io/finalizer`) is added before the first start.
- **Run strategy:** `Always` (default) restarts a VM whenever it stops, at most once per 10s; `Once` starts it once and leaves it stopped (it relaunches only if launch settings change); `Halted` keeps it stopped.
- **Launch settings** (cpus, memory, boot, disks, network, vsock, ioUring, autoCOW) are fixed at start. Changing them restarts the VM (`updateStrategy: Restart`, default) or sets the `RestartRequired` condition (`Manual`). A restart is shutdown → wait for stopped → start. It never calls the daemon's `delete`, so disk overlays survive.
- **Idle policy** (`spec.idle`) and **`paused`** apply live without a restart. The controller only resumes VMs it paused itself; VMs paused by the daemon's idle monitor wake on network traffic as usual. To turn idle pausing off, set `pauseAfter: 0s` (removing the block leaves the daemon's current setting in place until the next restart).
- **Provisioning** (`spec.provisioning`: files and SSH access) is applied on the first start only, because kube-vm writes it into a fresh copy-on-write overlay. It needs a `baseDisk` or an `overlay` disk. File content can be inline, or come from a Secret or ConfigMap key in the VM's namespace.
- **SSH access:** with `provisioning.ssh` set, the generated key pair is stored in Secret `<name>-ssh` (`ssh-privatekey`, `ssh-publickey`, `username`), owned by the VM.
- **Deletion:** shutdown → wait for stopped → daemon `delete` (removes managed overlays) → finalizer removed. If the node or its daemon is permanently gone, remove the finalizer by hand: `kubectl patch kvm NAME --type merge -p '{"metadata":{"finalizers":null}}'`.
- A VM already present in the daemon under the controller's id is adopted rather than restarted.
- The id inside the daemon is `<namespace>-<name>` (sanitised, truncated to 24 chars) plus 8 hex chars of the object's UID, exposed as `status.vmID`.

## Example

```yaml
apiVersion: vm.kube-vm.io/v1alpha1
kind: VirtualMachine
metadata:
  name: web
spec:
  cpus: 2
  memoryMiB: 2048
  boot:
    kernel: /var/lib/kube-vm/kernels/bzImage
  disks:
  - baseDisk: ubuntu          # image from the node's kube-vm base-disk catalog
  network:
  - type: NAT
  idle:
    pauseAfter: 5m
  provisioning:
    ssh:
      user: ubuntu
```

```console
$ kubectl get kvm
NAME   PHASE     NODE     IP              AGE
web    Running   node-1   192.168.101.2   30s
```

More in [`config/samples`](config/samples/vm_v1alpha1_virtualmachine.yaml).

### Spec reference

| Field | Description |
|---|---|
| `nodeName` | Pin to a node (immutable). |
| `nodeSelector` | Restrict scheduler placement by node labels. |
| `runStrategy` | `Always` (default), `Once`, `Halted`. |
| `updateStrategy` | `Restart` (default) or `Manual`. |
| `paused` | Pause the VM in memory. |
| `cpus`, `memoryMiB` | Defaults 1 and 1024. |
| `memoryBalancing` | `{minMiB}`; presence enables virtio balloon balancing. |
| `boot` | `kernel` or `firmware` (one is required), `initrd`, `cmdline`, `iso`, `serialConsole`. |
| `disks` | Up to 16. Each has exactly one of `baseDisk` (at most one per VM), `path` (optionally with `overlay` for copy-on-write), `url` (http/https, read-only) or `memory` (RAM disk). `readOnly` for path/memory disks. |
| `autoCOW` | Wrap writable local disks in managed copy-on-write overlays. |
| `network` | Up to 8. `type: NAT` or `TAP` (`tapName` required), optional `mac`, `switch`. |
| `vsock` | `{port, cid}` |
| `ioUring` | `{queueDepth}` (experimental). |
| `idle` | `pauseAfter`, `diskPauseAfter`, `wakeOnNetwork`. |
| `provisioning` | `files[]` (one of `content`, `data`, `secretKeyRef`, `configMapKeyRef`; or `directory: true`; plus `mode`, `uid`, `gid`) and `ssh{user\|home}`. |

Status: `phase` (Pending, Scheduled, Running, Idle, Paused, Stopped, Failed, Terminating), `nodeName`, `vmID`, `ipAddress`, `interfaces`, `startedAt`, `lastError`, and conditions `Scheduled`, `Ready`, `RestartRequired`. Snapshots are not supported yet.

## Install

Each VM host needs kube-vm installed, `kube-vm --server` running (it owns `/run/kube-vm/control.sock`), and a label:

```sh
kubectl label node NODE kube-vm.io/enabled=true
```

Then:

```sh
make docker-build IMG=ghcr.io/thebsdbox/kube-vm-controller:latest   # or use your own registry
make install deploy
```

`config/default` installs the CRD, RBAC (separate ServiceAccounts for scheduler and agent), the scheduler Deployment and the agent DaemonSet in `kube-vm-system`. The agent runs as root with a read-only root filesystem, no capabilities, and only the `/run/kube-vm` host directory mounted.

### Flags

`--scheduler`, `--agent`, `--node-name` (default `$NODE_NAME`), `--kube-vm-socket`, `--leader-elect`, `--sync-period` (default 10s), `--agent-namespace`, `--agent-selector`, `--metrics-bind-address`, `--health-probe-bind-address`. The agent's `/readyz` pings the daemon, so the scheduler only places VMs on nodes whose daemon answers.

## Development

```sh
make test                 # unit tests (fake kube-vm daemon + fake client)
make generate manifests   # after editing api/v1alpha1
make build
```

`internal/kubevm` holds the control-socket client and an in-memory fake daemon (`cmd/fake-daemon`). `config/e2e` deploys the controller with the fake daemon as an agent sidecar, for clusters without KVM.