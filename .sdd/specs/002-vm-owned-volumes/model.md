# Data Model: VM-Owned Volume Attach/Detach

- **Spec**: [`spec.md`](./spec.md)
- **Plan**: [`plan.md`](./plan.md)
- **Date**: 2026-08-07

vm-operator introduces no new CRD and no new field on `VirtualMachine` for this feature (D1). This document covers the one external type it mirrors and reads/patches (`CsiVolumeInfo`), and the pre-existing `VirtualMachine` surface this feature relies on or affects.

---

## `CsiVolumeInfo` (external, CSI-owned)

- **Group/Version/Kind**: `cns.vmware.com/v1alpha1`, `CsiVolumeInfo`
- **Namespace**: always `vmware-system-csi` (`cnsv1alpha1.CVINamespace`)
- **Name**: `cvi-volume-<CNS volume ID>` (`cnsv1alpha1.CVINamePrefix + volumeID`)
- **Ownership**: created and authoritatively reconciled by CSI. vm-operator's copy in `external/vsphere-csi-driver/api/v1alpha1/csivolumeinfo_types.go` is a byte-for-byte mirror of CSI's real type, not an independently-designed schema — any drift must be resolved by re-mirroring, not by diverging.
- **Who writes what** (two-channel contract, not currently webhook-enforced):

| Field | Writer | Notes |
|-------|--------|-------|
| `spec.volumeID` | CSI (create-time) | Immutable. |
| `spec.pvcName`, `spec.pvcNamespace` | CSI | Updated by CSI on rebind. |
| `spec.pvName` | CSI | |
| `spec.diskUUID` | CSI | Empty for an `fcd-retained` volume — the capture that fills it never runs on that path. |
| `spec.diskPath` | CSI (at Unregister) **and** vm-operator (JIT refresh before a dependent-mode detach) | vm-operator only refreshes; it never invents a path CSI hasn't already written once. |
| `spec.vms[]` | vm-operator, exclusively | See below. |
| `status.ownership`, `status.phase`, `status.observedGeneration`, `status.error`, `status.conditions` | CSI, exclusively | vm-operator reads these (the "green signal") and never patches `status`. |

### `spec.vms[]` (`VirtualMachineRef`)

One entry per VM with a relationship to this volume. vm-operator is the sole writer.

| Field | Type | Written when | Notes |
|-------|------|---------------|-------|
| `vmName` | string, required | Attach (append) | The `VirtualMachine` CR name. |
| `vmInstanceUUID` | string, optional | Attach | The VM's instance UUID at attach time. |
| `diskMode` | `CVIDiskMode`, optional | Attach; updated in place if it drifts | Empty is treated as `Persistent` (`vmopv1util.NormalizeDiskMode`) — always normalize before comparing an existing entry against a freshly-computed value, or a pre-`volumeName` entry compares as different from the volume it already matches. |
| `volumeName` | string, optional | Attach; backfilled in place if missing | `vm.spec.volumes[*].name` on that VM. **This is the correlation key detach uses** to find the matching `vm.status.volumes` entry — and therefore the device slot — after the volume has left `vm.spec.volumes`. CSI does not read it. |

`CVIDiskMode` values: `Persistent` (dependent — CSI transfers FCD ownership via best-effort unregister), `IndependentPersistent`, `IndependentNonPersistent`, `NonPersistent` (all three independent — FCD stays registered and `CSIManaged`). `IsDependentMode(dm)` is `dm == "" || dm == Persistent`.

### `status.ownership`

- `CSIManaged` — steady state: a registered FCD managed by CSI. An independent-mode volume that is attached stays in this state forever (it never transitions).
- `VMManaged` — steady state: a plain VMDK managed by the VM (dependent mode only, once CSI's unregister succeeds).

### The green signal

`vmopv1util.IsGreenSignal(cvi)` — true when `status.ownership == VMManaged && status.observedGeneration >= metadata.generation && status.phase == Succeeded`. Independent of whether `FcdRetainedAnnotation` is set: an `fcd-retained` volume is still green once CSI has finished processing it, even though its FCD was never actually unregistered.

### `csi.vsphere.vmware.com/fcd-retained` annotation

Set by CSI on the CVI when a `VMManaged` volume's FCD could **not** be unregistered (best-effort unregister was blocked). The FCD, its CNS DB row, and its FCD snapshots all still exist. vm-operator must consult this annotation (`vmopv1util.IsFcdRetained`), not assume CNS will return `NotFound`, to decide whether the attach needs the observed `vDiskId`.

### Finalizers CSI writes on the bound PVC

- `csi.vsphere.vmware.com/volume-protection` — present while `status.ownership == VMManaged`.
- `csi.vsphere.vmware.com/pvc-volume-protection` — present whenever `spec.vms` is non-empty (covers an attached independent-mode volume, which `VMManaged` alone misses). vm-operator does not write either finalizer and must not be surprised by their presence.

---

## `VirtualMachine` surface this feature relies on (no schema change)

### `vmoperator.vmware.com/vm-owned-volumes` annotation

Pre-existing constant (`pkgconst.VMOwnedVolumesAnnotation`). Stamped `"true"` by the mutation webhook on create when `Features.VMOwnedVolumes` is enabled. Immutable once set: the validating webhook rejects any change to an already-non-empty value, for any caller — the transition is scoped to the *value*, not the *principal*, so the same absent → `"true"` write remains legal for the mutation webhook on create and, later, for a migration controller on an existing VM.

### `vm.status.volumes[]` (`VirtualMachineVolumeStatus`)

No new field. For a VM-owned-volumes VM's dependent-mode disk, the attach path (`attachReadyDisks`) writes an entry directly, at attach time:

| Field | Value at attach |
|-------|------------------|
| `name` | `vm.spec.volumes[*].name` — the same string CVI's `spec.vms[*].volumeName` carries. |
| `type` | `Managed` — populated directly by the attach path, not inferred later by `updateVolumeStatus`'s generic scan (which only handles non-FCD disks it discovers on its own; see the Non-goals / independent-mode gap in `spec.md`). |
| `diskUUID` | The observed UUID from the reconfigure result, or the CVI's `spec.diskUUID` as fallback for `fcd-retained` (whose `spec.diskUUID` is never populated). |
| `attached` | `true` — set unconditionally, since `AttachVolumeDisks` only returns on a successful `ReconfigVM_Task` for the whole batch. |
| `controllerType`, `controllerBusNumber`, `unitNumber` | The observed device slot from the reconfigure placement. **This is what makes `volumeName`-based detach pairing possible** — the slot must exist in status before the volume can ever leave `vm.spec.volumes`. |

`updateVolumeStatus` (in `pkg/providers/vsphere/vmlifecycle/update_status.go`) separately promotes an existing `Classic`-typed, non-FCD, PVC-backed entry to `Managed` — the fix for a registration-race window where the disk enters status before `unmanagedvolumes_register` adds the PVC-backed spec entry. This is orthogonal to the attach-time write above and applies regardless of the VM's annotation, because it must also produce the right answer for a disk that is already on the CVI path by shape alone (e.g. mid-migration, before the annotation flips — see `plan.md`'s feature-gate audit).

---

## Provenance discriminator (registration pass)

Not a CVI or VM field, but load-bearing for this feature's correctness: `pkg/vmconfig/volumes/unmanaged/register/unmanagedvolumes_register.go`'s `filterOutManagedPVCDisks` tells a genuine unmanaged classic disk apart from an already-VM-owned dependent disk (both are, by hardware shape, a non-FCD disk with a PVC-backed spec entry) by **provenance**, not **state**: a disk's PVC is retained as an unmanaged-registration candidate only if it is absent, not-yet-found, or is *this VM's own registration placeholder* (`isRegistrationPlaceholderPVC` — a `dataSourceRef` pointing at the VM). Any other real, bound PVC on a non-FCD disk is excluded — this is what keeps migration (which produces this exact shape in bulk) from being mistaken for classic-disk registration.
