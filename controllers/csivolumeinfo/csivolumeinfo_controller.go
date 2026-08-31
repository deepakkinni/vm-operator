// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

// Package csivolumeinfo implements the CsiVolumeInfo sweeper (attach/detach
// §13.5.3). It lives outside controllers/virtualmachine/ — and outside the
// vmoperator.vmware.com group entirely — because CsiVolumeInfo is a
// cns.vmware.com type CSI owns; this controller only removes stale
// vm-operator-written entries from it.
package csivolumeinfo

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	vmopv1 "github.com/vmware-tanzu/vm-operator/api/v1alpha6"
	cnsv1alpha1 "github.com/vmware-tanzu/vm-operator/external/vsphere-csi-driver/api/v1alpha1"
	pkgcfg "github.com/vmware-tanzu/vm-operator/pkg/config"
	pkgctx "github.com/vmware-tanzu/vm-operator/pkg/context"
	pkglog "github.com/vmware-tanzu/vm-operator/pkg/log"
	"github.com/vmware-tanzu/vm-operator/pkg/patch"
	"github.com/vmware-tanzu/vm-operator/pkg/record"
	vmopv1util "github.com/vmware-tanzu/vm-operator/pkg/util/vmopv1"
)

const (
	controllerName = "csivolumeinfo"

	// orphanSuspectedAtAnnotation records when this controller first
	// observed the current set of orphaned VM entries on a CsiVolumeInfo. A
	// live VM CR that is merely not yet cached must not be treated as
	// absent, so an entry is removed only after this annotation has aged
	// past orphanGracePeriod on a live re-check — never on the first pass
	// (attach/detach §13.5.3).
	orphanSuspectedAtAnnotation = "vmoperator.vmware.com/orphan-suspected-at"

	// orphanSuspectedEntriesAnnotation records the comma-joined, sorted set
	// of VM names suspected orphaned as of orphanSuspectedAtAnnotation. If
	// the live-observed set changes between passes, the grace period
	// restarts rather than acting on a possibly-transient state.
	orphanSuspectedEntriesAnnotation = "vmoperator.vmware.com/orphan-suspected-entries"

	// orphanGracePeriod is the minimum time an orphan suspicion must persist,
	// re-confirmed on a live read, before this controller removes an entry.
	orphanGracePeriod = 5 * time.Minute
)

// AddToManager adds this package's controller to the provided manager.
func AddToManager(ctx *pkgctx.ControllerManagerContext, mgr manager.Manager) error {
	controllerNameShort := fmt.Sprintf("%s-controller", strings.ToLower(controllerName))

	r := NewReconciler(
		ctx,
		mgr.GetClient(),
		mgr.GetAPIReader(),
		ctrl.Log.WithName("controllers").WithName(controllerName),
		record.New(mgr.GetEventRecorder(controllerNameShort)),
	)

	c, err := controller.New(controllerName, mgr, controller.Options{
		Reconciler:              r,
		MaxConcurrentReconciles: ctx.GetMaxConcurrentReconciles(controllerNameShort, 1),
		LogConstructor:          pkglog.ControllerLogConstructor(controllerNameShort, &cnsv1alpha1.CsiVolumeInfo{}, mgr.GetScheme()),
	})
	if err != nil {
		return err
	}

	return c.Watch(source.Kind(
		mgr.GetCache(),
		&cnsv1alpha1.CsiVolumeInfo{},
		&handler.TypedEnqueueRequestForObject[*cnsv1alpha1.CsiVolumeInfo]{},
	))
}

// NewReconciler returns a new reconciler for the CsiVolumeInfo sweeper.
func NewReconciler(
	ctx context.Context,
	c client.Client,
	apiReader client.Reader,
	logger logr.Logger,
	recorder record.Recorder) *Reconciler {

	return &Reconciler{
		Context:   ctx,
		Client:    c,
		APIReader: apiReader,
		Logger:    logger,
		Recorder:  recorder,
	}
}

var _ reconcile.Reconciler = &Reconciler{}

// Reconciler sweeps CsiVolumeInfo.spec.vms entries whose VM no longer
// exists. It is a backstop, not a control path: the volume controller
// removes an entry itself on every normal detach or VM deletion; this
// controller only catches what that path missed (e.g. a VM CR deleted while
// bypassing the finalizer, or an operator gap before this feature existed).
type Reconciler struct {
	client.Client
	APIReader client.Reader
	Context   context.Context
	Logger    logr.Logger
	Recorder  record.Recorder
}

// +kubebuilder:rbac:groups=cns.vmware.com,resources=csivolumeinfos,verbs=get;list;watch;patch;update
// +kubebuilder:rbac:groups=vmoperator.vmware.com,resources=virtualmachines,verbs=get;list;watch

// Reconcile removes CsiVolumeInfo.spec.vms entries whose VirtualMachine no
// longer exists, once that has been true for orphanGracePeriod on a live
// (uncached) read.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (_ ctrl.Result, reterr error) {
	ctx = pkgcfg.JoinContext(ctx, r.Context)

	if !pkgcfg.FromContext(ctx).Features.VMOwnedVolumes {
		return ctrl.Result{}, nil
	}

	cvi := &cnsv1alpha1.CsiVolumeInfo{}
	if err := r.Get(ctx, req.NamespacedName, cvi); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if len(cvi.Spec.VMs) == 0 {
		return ctrl.Result{}, nil
	}

	logger := pkglog.FromContextOrDefault(ctx).WithValues("csiVolumeInfo", cvi.Name)

	orphaned, err := r.findOrphanedEntries(ctx, cvi)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to check CsiVolumeInfo %s for orphaned entries: %w", cvi.Name, err)
	}

	patchHelper, err := patch.NewHelper(cvi, r.Client)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to init patch helper for %s: %w", cvi.Name, err)
	}
	defer func() {
		if err := patchHelper.Patch(ctx, cvi); err != nil {
			if reterr == nil {
				reterr = err
			}
			logger.Error(err, "patch failed")
		}
	}()

	if len(orphaned) == 0 {
		clearOrphanSuspicion(cvi)
		return ctrl.Result{}, nil
	}

	confirmed, wait := checkGracePeriod(cvi, orphaned)
	if !confirmed {
		logger.Info("Suspected orphaned CsiVolumeInfo entries; waiting out the grace period before acting",
			"vmNames", orphaned)
		return ctrl.Result{RequeueAfter: wait}, nil
	}

	for _, vmName := range orphaned {
		cvi.Spec.VMs = vmopv1util.RemoveVMEntry(cvi.Spec.VMs, vmName)
		logger.Info("Removed orphaned VM entry from CsiVolumeInfo", "vmName", vmName)
	}
	clearOrphanSuspicion(cvi)

	return ctrl.Result{}, nil
}

// findOrphanedEntries returns the VM names in cvi.Spec.VMs that do not
// resolve to an existing VirtualMachine CR, checked on a live (uncached)
// read so a VM that merely has not reached this controller's cache yet is
// never mistaken for absent.
func (r *Reconciler) findOrphanedEntries(ctx context.Context, cvi *cnsv1alpha1.CsiVolumeInfo) ([]string, error) {
	var orphaned []string

	for _, entry := range cvi.Spec.VMs {
		vm := &vmopv1.VirtualMachine{}
		err := r.APIReader.Get(ctx, client.ObjectKey{
			Namespace: cvi.Spec.PVCNamespace,
			Name:      entry.VMName,
		}, vm)
		switch {
		case err == nil:
			continue
		case apierrors.IsNotFound(err):
			orphaned = append(orphaned, entry.VMName)
		default:
			return nil, fmt.Errorf("failed to get VirtualMachine %s/%s: %w", cvi.Spec.PVCNamespace, entry.VMName, err)
		}
	}

	return orphaned, nil
}

// checkGracePeriod reports whether the given orphaned set has been
// consistently suspected for at least orphanGracePeriod, stamping (or
// resetting) the suspicion annotations as a side effect. Returns the
// duration to wait before the next check when not yet confirmed.
func checkGracePeriod(cvi *cnsv1alpha1.CsiVolumeInfo, orphaned []string) (confirmed bool, wait time.Duration) {
	key := strings.Join(orphaned, ",")

	suspectedAt, hasTimestamp := cvi.Annotations[orphanSuspectedAtAnnotation]
	suspectedEntries := cvi.Annotations[orphanSuspectedEntriesAnnotation]

	if !hasTimestamp || suspectedEntries != key {
		stampOrphanSuspicion(cvi, key)
		return false, orphanGracePeriod
	}

	since, err := time.Parse(time.RFC3339, suspectedAt)
	if err != nil {
		// Malformed annotation — restart the clock rather than acting on it.
		stampOrphanSuspicion(cvi, key)
		return false, orphanGracePeriod
	}

	elapsed := timeSince(since)
	if elapsed < orphanGracePeriod {
		return false, orphanGracePeriod - elapsed
	}

	return true, 0
}

// timeSince and timeNow exist so this file has one seam for the current
// time, consistent with how the rest of the reconcile loop is otherwise a
// pure function of live-read state.
var (
	timeSince = time.Since
	timeNow   = time.Now
)

func stampOrphanSuspicion(cvi *cnsv1alpha1.CsiVolumeInfo, key string) {
	if cvi.Annotations == nil {
		cvi.Annotations = map[string]string{}
	}
	cvi.Annotations[orphanSuspectedAtAnnotation] = timeNow().Format(time.RFC3339)
	cvi.Annotations[orphanSuspectedEntriesAnnotation] = key
}

func clearOrphanSuspicion(cvi *cnsv1alpha1.CsiVolumeInfo) {
	if cvi.Annotations == nil {
		return
	}
	delete(cvi.Annotations, orphanSuspectedAtAnnotation)
	delete(cvi.Annotations, orphanSuspectedEntriesAnnotation)
}
