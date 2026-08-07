# Tasks: VM-Owned Volume Brownfield Migration

- **Spec**: [`spec.md`](./spec.md)
- **Plan**: [`plan.md`](./plan.md)
- **Epic**: vmop-TBD

Tasks within a Phase marked `[P]` may run in parallel. This list reflects what shipped in one commit on `topic/dk016388/vmown-impl-v2`, per `cns-specs/VGL-62908/implementation/vmop.md` §15–16 (V11, V12).

---

## Phase 1 — Migration trigger, BA freeze → retire, annotation flip (V11)

Dependencies: [002-vm-owned-volumes](../002-vm-owned-volumes/) Phases 1–9 (the CVI type, the CVI helpers, and the attach/detach steady state this migrates a VM onto).

- [x] T001 [vmop-TBD] `isMigrationCandidate`: gate on + annotation absent + (PVC volume present or explicit `migrate-to-vm-owned` trigger) — `controllers/virtualmachine/volumeattachdetach/volumeattachdetach_migration.go`
- [x] T002 [vmop-TBD] Wire the migration branch into `ReconcileNormal` ahead of the existing VM-owned/legacy branches — `controllers/virtualmachine/volumeattachdetach/volumeattachdetach_controller.go`
- [x] T003 [vmop-TBD] `reconcileMigration` Stage 1 — freeze the BA (`cns.vmware.com/vm-owned-migration: InProgress`), confirmed before any `BA.spec` edit — `controllers/virtualmachine/volumeattachdetach/volumeattachdetach_migration.go`
- [x] T004 [vmop-TBD] `reconcileMigration` per-disk loop: resolve/create the CVI, append or update the VM's entry (mode converted from the BA's own `DiskMode` via `cnsDiskModeToCVIDiskMode`), confirm the entry via a read-back, then release the disk from `BA.spec.volumes` — never before the entry is confirmed — `controllers/virtualmachine/volumeattachdetach/volumeattachdetach_migration.go`
- [x] T005 [P] [vmop-TBD] Per-disk readiness tracking: a dependent entry needs `VMManaged` (clean or `fcd-retained`, both count); an independent entry is ready as soon as its entry is confirmed — `controllers/virtualmachine/volumeattachdetach/volumeattachdetach_migration.go`
- [x] T006 [vmop-TBD] `completeMigration` Stage 2 — the VM annotation flip is the commit point, patched directly (not via the outer reconcile's deferred patch) so it is guaranteed to land before the BA annotation flips to `Complete` and the BA is deleted — `controllers/virtualmachine/volumeattachdetach/volumeattachdetach_migration.go`
- [x] T007 [vmop-TBD] Unit tests: freeze + entry + BA-removal + requeue; completion once `VMManaged`; `fcd-retained` counts as migrated; independent re-home completes without waiting on CSI; explicit trigger with no existing BA; an already-annotated VM is not treated as a candidate — `controllers/virtualmachine/volumeattachdetach/volumeattachdetach_migration_unit_test.go`

## Phase 2 — VKS disk-mode conversion (V12)

Dependencies: Phase 1 (runs inside `reconcileMigration`, before the per-disk loop).

- [x] T010 [vmop-TBD] `providers.VirtualMachineProviderInterface.HasAnySnapshot` and `ConvertDiskToIndependentPersistent` — `pkg/providers/vm_provider_interface.go`
- [x] T011 [P] [vmop-TBD] vSphere implementation: `HasAnySnapshot` (moVM.Snapshot presence), `ConvertDiskToIndependentPersistent` (device edit via `findVirtualDiskDeviceAtSlot` + `diskModeToVim`, guarded by `assertNoVMLevelCBT`) — `pkg/providers/vsphere/vmprovider_vm_ownedvolumes_migration.go`
- [x] T012 [P] [vmop-TBD] Fake provider overrides — `pkg/providers/fake/fake_vm_provider.go`
- [x] T013 [vmop-TBD] `convertVKSDiskModes`: precheck no VM snapshot (stall, don't defer, on violation); per non-boot disk rewrite `vm.spec.volumes[*].diskMode`, reconfigure the device, update the in-memory BA volume spec so the per-disk loop sees the new mode — `controllers/virtualmachine/volumeattachdetach/volumeattachdetach_migration.go`
- [x] T014 [vmop-TBD] Unit tests: conversion happens before CVI-entry write and re-home; correct controller/bus/unit passed to the provider call; snapshot precheck stalls the whole conversion with a requeue, no CVI or spec write — `controllers/virtualmachine/volumeattachdetach/volumeattachdetach_migration_unit_test.go`

---

## Phase Final — Polish (blocked on ticket filing)

- [ ] T100 [vmop-TBD] File the JIRA epic; replace every `vmop-TBD` in this spec's artifacts with the real `vmop-NNN`.
- [ ] T101 [vmop-TBD] `docs/concepts/workloads/vm-owned-volumes.md`: add a migration section once this spec's user-facing behavior (the lazy trigger firing on an existing brownfield VM's first attach/detach) needs documenting for operators.

---

## Out of scope for this spec

- CSI's deferred-unregister branch, auto-complete watch, and skip-register-on-detach (migration spec §8–§13) — CSI-repository work, not tracked here.
- Independent-mode device attach for a newly-attached (non-migrated) volume — tracked in [002-vm-owned-volumes](../002-vm-owned-volumes/)'s Non-goals.
- Rollback (migration spec Appendix A) — not implemented; forward-only by design.
