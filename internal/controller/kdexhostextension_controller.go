package controller

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	extensionConditionAttached   = "Attached"
	extensionReasonAttached      = "Attached"
	extensionReasonNotSelected   = "NotSelected"
	extensionReasonHostNotFound  = "HostNotFound"
	extensionReasonLimitExceeded = "LimitExceeded"
)

// KDexHostExtensionReconciler reports, on each KDexHostExtension, whether the
// host it names applies it. It only writes status; the KDexHost reconciler
// owns what the internal host carries. Both use selectExtensions, so the
// condition and the applied set never disagree.
type KDexHostExtensionReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

func (r *KDexHostExtensionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ext := &kdexv1alpha1.KDexHostExtension{}
	if err := r.Get(ctx, req.NamespacedName, ext); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !ext.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	status, reason := metav1.ConditionFalse, extensionReasonHostNotFound
	message := fmt.Sprintf("KDexHost %q not found", ext.Spec.HostRef.Name)

	host := &kdexv1alpha1.KDexHost{}
	err := r.Get(ctx, types.NamespacedName{Namespace: ext.Namespace, Name: ext.Spec.HostRef.Name}, host)
	switch {
	case apierrors.IsNotFound(err):
	case err != nil:
		return ctrl.Result{}, err
	default:
		siblings := &kdexv1alpha1.KDexHostExtensionList{}
		if err := r.List(ctx, siblings, client.InNamespace(host.Namespace), client.MatchingFields{hostIndexKey: host.Name}); err != nil {
			return ctrl.Result{}, err
		}
		applied, overflow, selErr := selectExtensions(host, siblings.Items)
		switch {
		case selErr != nil:
			reason, message = extensionReasonNotSelected, fmt.Sprintf("KDexHost %q extensionSelector is invalid: %v", host.Name, selErr)
		case containsExtension(applied, ext.Name):
			status, reason, message = metav1.ConditionTrue, extensionReasonAttached, fmt.Sprintf("applied to KDexHost %q at weight %d", host.Name, ext.Spec.Weight)
		case containsExtension(overflow, ext.Name):
			reason, message = extensionReasonLimitExceeded, fmt.Sprintf("KDexHost %q already applies %d extensions", host.Name, maxHostExtensions)
		case host.Spec.ExtensionSelector == nil:
			reason, message = extensionReasonNotSelected, fmt.Sprintf("KDexHost %q has no extensionSelector", host.Name)
		default:
			reason, message = extensionReasonNotSelected, fmt.Sprintf("KDexHost %q extensionSelector does not select this extension", host.Name)
		}
	}

	patch := client.MergeFrom(ext.DeepCopy())
	meta.SetStatusCondition(&ext.Status.Conditions, metav1.Condition{
		Type: extensionConditionAttached, Status: status, Reason: reason, Message: message,
		ObservedGeneration: ext.Generation,
	})
	ext.Status.ObservedGeneration = ext.Generation
	return ctrl.Result{}, r.Status().Patch(ctx, ext, patch)
}

func containsExtension(es []kdexv1alpha1.KDexHostExtension, name string) bool {
	for _, e := range es {
		if e.Name == name {
			return true
		}
	}
	return false
}

// extensionsForHost enqueues every extension naming a host, so a host's
// creation, deletion or selector change re-evaluates their conditions.
func (r *KDexHostExtensionReconciler) extensionsForHost(ctx context.Context, obj client.Object) []reconcile.Request {
	list := &kdexv1alpha1.KDexHostExtensionList{}
	if err := r.List(ctx, list, client.InNamespace(obj.GetNamespace()), client.MatchingFields{hostIndexKey: obj.GetName()}); err != nil {
		return nil
	}
	out := make([]reconcile.Request, 0, len(list.Items))
	for _, e := range list.Items {
		out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: e.Namespace, Name: e.Name}})
	}
	return out
}

// SetupWithManager must run after KDexHostReconciler.SetupWithManager, which
// registers the hostIndexKey index on KDexHostExtension.
func (r *KDexHostExtensionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&kdexv1alpha1.KDexHostExtension{}).
		Watches(&kdexv1alpha1.KDexHost{}, handler.EnqueueRequestsFromMapFunc(r.extensionsForHost)).
		Named("kdexhostextension").
		Complete(r)
}
