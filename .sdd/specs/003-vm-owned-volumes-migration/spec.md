# Feature Specification: VM-Owned Volume Brownfield Migration

- **Feature branch**: `topic/dk016388/vmown-impl-v2`
  - **Fork**: `vmware-tanzu/vm-operator`
  - **PR target**: `vmware-tanzu/vm-operator`
- **Created**: 2026-08-07
- **Status**: Implemented
- **Epic**: vmop-TBD
- **Design docs**: `cns-specs/VGL-62908/vm-owned-volume-migration-spec.md` (full cross-component spec), `cns-specs/VGL-62908/implementation/vmop.md` §15–16 (V11, V12 — the implementation plan this spec tracks).
- **Depends on**: [002-vm-owned-volumes](../002-vm-owned-volumes/) (the attach/detach steady state this migrates a VM onto).

---

## Summary

A brownfield VM (no `vmoperator.vmware.com/vm-owned-volumes` annotation) has every PVC-backed disk attached as an FCD through `CnsNodeVMBatchAttachment` (BA). When the `VMOwnedVolumes` feature gate is on, the first attach or detach on such a VM — or an explicit admin-set trigger — converts **every** existing disk on that VM to the CsiVolumeInfo (CVI) path in place, without detaching anything, then flips the VM to VM-owned. A dependent disk becomes a plain VMDK (or stays a retained FCD when in-place unregister is blocked); an independent disk is re-homed onto the CVI while remaining a registered FCD. A VKS (guest-cluster node) VM's non-boot disks are additionally converted from dependent to independent mode first, so the guest's own paravirtual CSI keeps working.

This spec covers vm-operator's orchestration only — CSI's per-CVI unregister/defer/auto-complete state machine is specified in the migration spec's CSI sections and implemented in the CSI driver, not this repository.

---

## Goals

- vm-operator MUST detect a migration candidate: `VMOwnedVolumes` enabled, the VM lacks the `vm-owned-volumes` annotation, and either it has at least one PVC-backed volume (the lazy trigger — indistinguishable, in a level-triggered reconciler, from "an attach or detach just happened") or carries the explicit `vmoperator.vmware.com/migrate-to-vm-owned: "true"` annotation.
- vm-operator MUST freeze the VM's `CnsNodeVMBatchAttachment` (Stage 1) by annotating it `cns.vmware.com/vm-owned-migration: InProgress` before making any other change, and MUST confirm that patch landed before touching `BA.spec.volumes` — CSI's BA controller honors no attach or detach for a frozen BA, which is what prevents a live disk from being detached mid-handoff.
- For every disk currently on the BA, vm-operator MUST resolve or create its CVI and append/update `{vmName, vmInstanceUUID, diskMode, volumeName}`, using the disk's current mode (except on a VKS VM, where the mode is rewritten to independent-persistent first). vm-operator MUST NOT issue a `ReconfigVM_Task` to add or remove the disk as part of this — the disk is already on the VM.
- vm-operator MUST remove a disk from `BA.spec.volumes` only after its CVI entry is confirmed present (a read-back, not statement order) — reversing this order would let the BA's PVC finalizer release race ahead of the CVI-side finalizer that replaces it.
- vm-operator MUST treat both a clean dependent disk (`VMManaged`, no annotation) and a deferred one (`VMManaged` + `fcd-retained`) as migrated for the purpose of completing the VM's migration. It MUST NOT wait for a deferred disk's FCD to actually be unregistered, and MUST NOT surface a permanently-deferred disk as an error — it is a valid, intentional steady state.
- Once every disk on the VM is on the CVI path, vm-operator MUST set `vmoperator.vmware.com/vm-owned-volumes: "true"` on the VM as the commit point, and this write MUST be the last one before the BA is retired (annotated `Complete` and deleted) — never after.
- vm-operator MUST NOT verify that CSI has placed its PVC-local protection finalizer on a re-homed independent disk before retiring the BA; the entry-before-BA-removal ordering above already guarantees it is in place by then.
- For a VKS node VM (`kubeutil.HasCAPILabels`), vm-operator MUST precheck that the VM has no vSphere snapshot (managed or unmanaged) before converting any of its non-boot disks, since the host rejects a disk-mode change VM-wide if any snapshot exists. A violation MUST stall migration (retry) rather than create a new state — deferred-unregister is a dependent-disk-only concept.
- vm-operator MUST rewrite a VKS disk's `vm.spec.volumes[*].diskMode` to `IndependentPersistent` and reconfigure the live device to match, in that order, before appending its CVI entry — so the CVI never advertises a mode the device does not yet have.
- Every reconfigure vm-operator issues against a VM undergoing or having undergone migration MUST NOT set the VM-level `changeTrackingEnabled` flag — doing so would force a retained FCD's CBT off and destroy its changeId.
- Migration MUST be forward-only and idempotent: a crash or repeated reconcile at any point resumes from observed state with no rollback.

---

## Non-goals

- CSI's per-CVI state machine: the deferred-unregister branch, the auto-complete watch on `VolumeSnapshot` deletion, and the skip-register branch on detach of a deferred disk. These are CSI-side (migration spec §8–§11, §13) and out of scope for this repository.
- The `CnsQueryUnregisterFeasibility` CNS API this whole design leans on for fault classification. It does not exist yet; CSI treats every unregister fault as structural in the interim (migration spec §5.7, §19 Q8). vm-operator's orchestration does not depend on it directly.
- Rollback. Migration is forward-only; a best-effort rollback design is sketched (not implemented) in the migration spec's Appendix A.
- Fleet-wide migration sequencing or a bulk migration job. Only the lazy and explicit per-VM triggers are implemented.
- Independent-mode device attach for a *newly* attached (non-migrated) volume — that gap belongs to [002-vm-owned-volumes](../002-vm-owned-volumes/) and is unaffected by this spec. Migration's independent re-home never adds a device; it only re-labels an already-attached one.

---

## User stories / acceptance criteria

### US1 — Platform operator: a brownfield VM migrates on its first attach or detach (Priority: P1)

**Given** a brownfield VM with an existing dependent-persistent disk in a conducive state, **when** the user adds or removes any PVC on that VM, **then** vm-operator freezes the BA, migrates the existing disk to the CVI path, waits for CSI to reach `VMManaged`, flips the VM annotation, and deletes the BA — after which the triggering attach or detach is processed as an ordinary VM-owned Workflow A/B.

**Acceptance scenarios:**

1. **Given** a brownfield VM with one clean-conducive dependent disk, **when** migration runs, **then** the CVI gains a `Persistent`-mode entry, the disk is removed from `BA.spec.volumes`, and once CSI reports `VMManaged` the VM gets the `vm-owned-volumes` annotation and the BA is deleted.
2. **Given** a brownfield VM with one independent-persistent disk, **when** migration runs, **then** the CVI gains an `IndependentPersistent`-mode entry and the VM completes migration in the same reconcile — no wait on CSI, since an independent entry never changes ownership.
3. **Given** a dependent disk whose in-place unregister CSI cannot complete (a pre-existing `VolumeSnapshot`), **when** CSI marks the CVI `VMManaged` + `fcd-retained`, **then** vm-operator treats the disk as migrated and completes the VM's migration without waiting further.
4. **Given** an administrator sets `vmoperator.vmware.com/migrate-to-vm-owned: "true"` on a brownfield VM with no existing BA, **when** vm-operator reconciles, **then** the VM annotation is set immediately, with nothing to freeze or retire.

### US2 — Platform operator: a VKS node VM's non-boot disks convert to independent mode (Priority: P1)

**Given** a VKS node VM with a dependent-persistent non-boot PVC disk and no vSphere snapshots, **when** migration runs, **then** vm-operator rewrites the disk's mode to `IndependentPersistent` in `vm.spec.volumes`, reconfigures the live device to match, appends an independent CVI entry, and re-homes it off the BA — the FCD is never unregistered and the guest's paravirtual CSI keeps working.

**Acceptance scenarios:**

1. **Given** a VKS VM with a dependent non-boot disk and no snapshots, **when** migration runs, **then** the device's `VirtualDiskMode` becomes `independent_persistent`, `vm.spec.volumes[*].diskMode` matches, and the CVI entry is `IndependentPersistent`.
2. **Given** the same VM has a vSphere snapshot (managed or unmanaged), **when** migration attempts the conversion, **then** it stalls with a message naming the VM, and the VM is not treated as having a deferred-unregister disk — retried, not defaulted into a new state.
3. **Given** a crash lands between rewriting `vm.spec.volumes[*].diskMode` and reconfiguring the device, **when** vm-operator reconciles again, **then** it compares the two and issues the reconfigure, converging rather than repeating the spec write.

### US3 — Platform operator: migration never races a live detach (Priority: P1)

**Given** a brownfield VM undergoing migration, **when** any step between the freeze and the retire fails or crashes, **then** the BA remains frozen and no disk is ever detached as a side effect of the handoff.

**Acceptance scenarios:**

1. **Given** the freeze patch has not yet landed, **when** vm-operator crashes before removing any disk from `BA.spec`, **then** the BA is unfrozen and nothing has changed — safe to retry from the top.
2. **Given** the freeze patch has landed, **when** vm-operator removes a disk from `BA.spec.volumes`, **then** the BA controller — observing `InProgress` — takes no detach action regardless.
3. **Given** every disk has a confirmed CVI entry but the VM annotation has not yet been set, **when** vm-operator reconciles again, **then** it re-derives readiness from the CVI states and completes migration idempotently — no duplicate CVI entries, no duplicate BA-removal errors.

---

## Edge cases

- A dependent RWM disk shared by two VMs migrates only for the VM currently being migrated; the other VM's attachment stays on its own BA until its own migration runs. The resulting cross-VM window (one VM migrated, the other still on a BA for the same FCD) is deliberately left undefined by the cross-component spec rather than given ad hoc bookkeeping (migration spec §19 Q2) — vm-operator's contribution is limited to appending only its own VM's entry, never touching another VM's.
- A VM with zero existing PVC-backed disks and no BA is a migration candidate the moment its first PVC is added; migration for it is a no-op flip since there is nothing to freeze or retire.
- A CVI entry written before `volumeName` existed, or drifted by a prior partial migration attempt, is corrected in place on the next reconcile — the same self-healing the attach/detach spec's Workflow A already performs.
- A permanently-deferred disk (a linked clone) never converges to a clean plain VMDK; this is a valid resting state, not a stalled migration, and vm-operator does not retry the CVI entry write once it is confirmed present.

---

## Open questions

- [NEEDS CLARIFICATION: the vpxd change that lets a raw device-add's `vDiskId` drive the linked-clone precheck (attach/detach §7.1.5) does not exist yet. Every re-homed independent disk and every VKS conversion produces a registered FCD later attached by raw reconfigure — until that vpxd change lands, the linked-clone precheck simply does not run on those attaches (migration spec §19 Q6).]
- [NEEDS CLARIFICATION: `CnsQueryUnregisterFeasibility` does not exist yet (migration spec §5.7, §19 Q8); CSI's interim behavior of treating every unregister fault as structural is outside this repository's control but affects how quickly a transient fault is distinguished from a real defer on the CVI vm-operator reads.]
