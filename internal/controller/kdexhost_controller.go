/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/kdex-tech/nexus-manager/internal/utils"
	nexuswebhook "github.com/kdex-tech/nexus-manager/internal/webhook"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
	"kdex.dev/crds/configuration"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	kdexWeb           = "kdex-host"
	hostFinalizerName = "kdex.dev/kdex-nexus-host-finalizer"
	hostIndexKey      = "spec.hostRef.name"
	// helmTeardownPoll is how soon a teardown waiting on an in-flight helm
	// operation looks again. It is its own, shorter than RequeueDelay, since
	// nothing else wakes the wait: an operation that stops because its host
	// is terminating writes no status, so no event follows its end. Each look
	// costs two cached reads of the host and a status patch (the waiting
	// condition), which a couple of seconds apart, for the minutes a wait can
	// last, is cheap enough.
	helmTeardownPoll = 2 * time.Second
	// helmTeardownWaitCap is how long after a host's deletion began its
	// teardown waits on an in-flight helm operation before going ahead
	// without it. Once the host is terminating the operation starts no
	// further Helm call, so it is past this only if the call it is in has
	// outlived every deadline it has: a chart fetch -- at most
	// RegistryFetchBudget from a registry, where a request under way when
	// that runs out is cut off then, or from a classic repository at most
	// three requests -- the index, the archive, its provenance file -- of at
	// most ChartFetchTimeout each -- then the call itself, at most
	// HelmCallTimeout. The minute is margin.
	helmTeardownWaitCap = max(utils.RegistryFetchBudget, 3*utils.ChartFetchTimeout) + utils.HelmCallTimeout + time.Minute
)

type helmOperation struct {
	cancel context.CancelFunc
	hash   string
	// id tells one registration from another of the same hash: a spec
	// edited and edited back repeats a hash while the first run of it may
	// still be going.
	id uint64
}

// cachedHelmClient pairs a Helm client with a hash of the secrets it was
// created with, so a credential change can be detected and the client rebuilt.
type cachedHelmClient struct {
	client utils.HelmClientInterface
	hash   string
}

// KDexHostReconciler reconciles a KDexHost object
type KDexHostReconciler struct {
	client.Client
	// APIReader reads straight from the API server, bypassing the informer
	// cache Client reads from. A write guarded by an optimistic lock must read
	// what it guards live: under informer lag a cached read returns the same
	// stale resourceVersion on every retry, and every retry conflicts. So must
	// teardown, which acts on the companion record as it stands now. Nil
	// falls back to Client.
	APIReader     client.Reader
	ControllerID  string
	Ctx           context.Context
	Configuration configuration.NexusConfiguration
	// HelmClientFactory builds a host's Helm client, which acts as the
	// namespace's installer and as nothing else (utils.NewHelmClient).
	HelmClientFactory func(namespace string, secrets kdexv1alpha1.Secrets, logger logr.Logger) (utils.HelmClientInterface, error)
	RequeueDelay      time.Duration
	Scheme            *runtime.Scheme

	// HelmRenderConcurrency bounds the number of in-process Helm renders that
	// may run simultaneously across the whole fleet. Helm renders load and
	// decompress the chart into the operator heap, so peak memory scales with
	// the number of concurrent renders, not the fleet size — left unbounded a
	// cluster-wide config change stampedes every host's render at once and can
	// OOM-kill the operator. A value <= 0 means unbounded (legacy behavior).
	// See issue #24.
	HelmRenderConcurrency int

	activeHelmOperations map[types.NamespacedName]helmOperation
	// helmRuns counts the runAsyncHelmReconcile goroutines still running for
	// each host. Unlike activeHelmOperations, which a superseding operation or
	// a teardown clears while the old goroutine may still be inside an
	// uncancellable Helm call, an entry here lasts until the goroutine returns.
	helmRuns          map[types.NamespacedName]int
	helmRunSeq        uint64
	mu                sync.RWMutex
	helmClients       map[string]cachedHelmClient
	helmRenderSem     chan struct{}
	helmRenderSemOnce sync.Once
}

// ConditionReferenceNamespacesDisabled is set on a host whose
// spec.referenceNamespaces is non-empty: cross-namespace references are
// disabled in this release, so the list is ignored.
const ConditionReferenceNamespacesDisabled = "ReferenceNamespacesDisabled"

func setReferenceNamespacesCondition(host *kdexv1alpha1.KDexHost) {
	if len(host.Spec.ReferenceNamespaces) == 0 {
		meta.RemoveStatusCondition(&host.Status.Conditions, ConditionReferenceNamespacesDisabled)
		return
	}

	meta.SetStatusCondition(&host.Status.Conditions, metav1.Condition{
		Type:               ConditionReferenceNamespacesDisabled,
		Status:             metav1.ConditionTrue,
		Reason:             "CrossNamespaceReferencesDisabled",
		Message:            "cross-namespace references are disabled in this release; spec.referenceNamespaces is ignored",
		ObservedGeneration: host.Generation,
	})
}

// nolint:gocyclo
func (r *KDexHostReconciler) Reconcile(ctx context.Context, req ctrl.Request) (res ctrl.Result, err error) {
	log := logf.FromContext(ctx)
	log.Info("reconciling", "host", req.Name, "namespace", req.Namespace)

	var host kdexv1alpha1.KDexHost
	if err := r.Get(ctx, req.NamespacedName, &host); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if host.Status.Attributes == nil {
		host.Status.Attributes = make(map[string]string)
	}

	// Defer status update
	defer func() {
		// If the object was not found, we don't need to update status
		if apierrors.IsNotFound(err) {
			return
		}

		latestHost := &kdexv1alpha1.KDexHost{}
		if getErr := r.Get(ctx, req.NamespacedName, latestHost); getErr != nil {
			if !apierrors.IsNotFound(getErr) {
				log.Error(getErr, "failed to get latest host for status update")
			}
			return
		}

		patch := client.MergeFrom(latestHost.DeepCopy())

		// The Helm status attributes are the goroutine's: take them as last
		// read, so this patch never writes a helm.release. key.
		if host.Status.Attributes == nil {
			host.Status.Attributes = make(map[string]string)
		}
		takeHelmAttributes(host.Status.Attributes, latestHost.Status.Attributes)

		// Update conditions carefully - if helm failed in the cluster, don't overwrite it with something else
		currentHelmStatus := latestHost.Status.Attributes[AttributeHelmReleaseStatus]
		if currentHelmStatus == HelmStatusFailed {
			if degraded := meta.FindStatusCondition(latestHost.Status.Conditions, string(kdexv1alpha1.ConditionTypeDegraded)); degraded != nil && degraded.Status == metav1.ConditionTrue {
				// Helm failed in cluster, ensure our memory copy reflects this
				// to avoid overwriting. The reason, like the helm.release.
				// attributes, is the helm run's: it says why the run failed
				// (setHelmOutcome), e.g. a refused chart.
				reason := kdexv1alpha1.ConditionReason(degraded.Reason)
				if reason == "" {
					reason = kdexv1alpha1.ConditionReasonReconcileError
				}
				kdexv1alpha1.SetConditions(
					&host.Status.Conditions,
					kdexv1alpha1.ConditionStatuses{
						Progressing: metav1.ConditionFalse,
						Degraded:    metav1.ConditionTrue,
						Ready:       metav1.ConditionFalse,
					},
					reason,
					latestHost.Status.Attributes[AttributeHelmReleaseError],
				)
			}
		}

		latestHost.Status = host.Status
		latestHost.Status.ObservedGeneration = host.Generation

		if updateErr := r.Status().Patch(ctx, latestHost, patch); updateErr != nil {
			if apierrors.IsConflict(updateErr) {
				res = ctrl.Result{RequeueAfter: 50 * time.Millisecond}
			} else if !apierrors.IsNotFound(updateErr) {
				log.Error(updateErr, "failed to patch status")
				err = updateErr
				res = ctrl.Result{}
			}
		}

		log.V(2).Info("status", "status", host.Status, "err", err, "res", res)
	}()

	// A teardown waits out an in-flight helm operation before it reads
	// anything more: a pass that only finds it still waiting reads no Secrets,
	// lists nothing, and makes no live read.
	if !host.DeletionTimestamp.IsZero() && controllerutil.ContainsFinalizer(&host, hostFinalizerName) {
		if requeue, wait := r.awaitHelmRunBeforeTeardown(&host, log); wait {
			return requeue, nil
		}
	}

	secrets, err := ResolveSecrets(ctx, r.Client, &host.Status, host.Namespace, host.Spec.SecretSelector)
	if err != nil {
		kdexv1alpha1.SetConditions(
			&host.Status.Conditions,
			kdexv1alpha1.ConditionStatuses{
				Degraded:    metav1.ConditionTrue,
				Progressing: metav1.ConditionFalse,
				Ready:       metav1.ConditionFalse,
			},
			kdexv1alpha1.ConditionReasonReconcileSuccess,
			err.Error(),
		)
		return ctrl.Result{}, err
	}

	if host.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(&host, hostFinalizerName) {
			controllerutil.AddFinalizer(&host, hostFinalizerName)
			if err := r.Update(ctx, &host); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: r.RequeueDelay}, nil
		}
	} else {
		if controllerutil.ContainsFinalizer(&host, hostFinalizerName) {
			// Teardown works on the host as the cluster holds it: it acts on
			// the companion record, which the helm operation just waited out
			// may have written after the cache last saw the host, and it
			// removes the finalizer by an update the cache's resourceVersion
			// would make conflict. And it acts only on the host the cache
			// showed terminating: under that name the cluster may now hold a
			// new host, whose releases and finalizer are its own. Read into an
			// object of its own: decoded over the cached host, whatever the
			// cluster's copy leaves out -- a deletion timestamp, a label, a
			// map key -- would survive from the cache.
			live := &kdexv1alpha1.KDexHost{}
			if err := r.reader().Get(ctx, req.NamespacedName, live); err != nil {
				return ctrl.Result{}, client.IgnoreNotFound(err)
			}
			if live.UID != host.UID || live.DeletionTimestamp.IsZero() {
				log.V(1).Info("the cluster does not hold this host terminating; nothing to tear down", "host", host.Name)
				return ctrl.Result{}, nil
			}
			host = *live

			internalHost := &kdexv1alpha1.KDexInternalHost{}
			err := r.Get(ctx, req.NamespacedName, internalHost)
			if err == nil {
				if internalHost.DeletionTimestamp.IsZero() {
					if err := r.Delete(ctx, internalHost); err != nil {
						return ctrl.Result{}, err
					}
				}
				// KDexInternalHost still exists. We wait.
				return ctrl.Result{RequeueAfter: r.RequeueDelay}, nil
			}
			if !apierrors.IsNotFound(err) {
				return ctrl.Result{}, err
			}

			// Wait for internal utility pages to be gone
			internalUtilityPages := &kdexv1alpha1.KDexInternalUtilityPageList{}
			if err := r.List(ctx, internalUtilityPages, client.InNamespace(host.Namespace), client.MatchingFields{hostIndexKey: host.Name}); err != nil {
				return ctrl.Result{}, err
			}
			if len(internalUtilityPages.Items) > 0 {
				for _, iup := range internalUtilityPages.Items {
					if iup.DeletionTimestamp.IsZero() {
						if err := r.Delete(ctx, &iup); err != nil {
							return ctrl.Result{}, err
						}
					}
				}
				// KDexInternalUtilityPage still exists. We wait.
				return ctrl.Result{RequeueAfter: r.RequeueDelay}, nil
			}

			// Wait for internal translations to be gone
			internalTranslations := &kdexv1alpha1.KDexInternalTranslationList{}
			if err := r.List(ctx, internalTranslations, client.InNamespace(host.Namespace), client.MatchingFields{hostIndexKey: host.Name}); err != nil {
				return ctrl.Result{}, err
			}
			if len(internalTranslations.Items) > 0 {
				for _, it := range internalTranslations.Items {
					if it.DeletionTimestamp.IsZero() {
						if err := r.Delete(ctx, &it); err != nil {
							return ctrl.Result{}, err
						}
					}
				}
				// KDexInternalTranslation still exists. We wait.
				return ctrl.Result{RequeueAfter: r.RequeueDelay}, nil
			}

			// Each step below is idempotent, and a failure keeps the finalizer
			// for the next pass to retry. Releasing it early would delete the
			// CR while what it owns -- releases, the cluster-reader subject,
			// the namespace's installer -- is still there, with nothing left
			// in the cluster that references it.
			if err := r.teardownHost(ctx, &host, secrets, log); err != nil {
				// Keeping the finalizer is correct, but it leaves the host in
				// Terminating with no visible reason -- and a terminating
				// object is exactly when an operator has the fewest places to
				// look. Put the cause on the CR so `kubectl describe` explains
				// the wait, and says which release to unblock.
				kdexv1alpha1.SetConditions(
					&host.Status.Conditions,
					kdexv1alpha1.ConditionStatuses{
						Degraded:    metav1.ConditionTrue,
						Progressing: metav1.ConditionFalse,
						Ready:       metav1.ConditionFalse,
					},
					kdexv1alpha1.ConditionReasonReconcileError,
					fmt.Sprintf("Deletion is blocked: %v. The finalizer is held until the teardown completes.", err),
				)
				return ctrl.Result{}, err
			}

			err = r.deleteHelmClient(host.Name, host.Namespace)
			if err != nil {
				return ctrl.Result{}, err
			}

			controllerutil.RemoveFinalizer(&host, hostFinalizerName)
			if err := r.Update(ctx, &host); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	kdexv1alpha1.SetConditions(
		&host.Status.Conditions,
		kdexv1alpha1.ConditionStatuses{
			Degraded:    metav1.ConditionFalse,
			Progressing: metav1.ConditionTrue,
			Ready:       metav1.ConditionUnknown,
		},
		kdexv1alpha1.ConditionReasonReconciling,
		"Reconciling",
	)
	setReferenceNamespacesCondition(&host)

	if err := r.healClusterReaderSubject(ctx, &host); err != nil {
		log.Error(err, "failed to ensure the host-manager cluster-reader subject", "host", host.Name, "namespace", host.Namespace)
	}

	utilityPageRefs, consentUnresolved, shouldReturn, r1, err := r.resolveUtilityPages(ctx, &host)
	if shouldReturn {
		log.Info("resolveUtilityPages requested return", "requeueAfter", r1.RequeueAfter, "err", err)
		if err == nil && r1.RequeueAfter > 0 {
			return r1, nil
		}

		message := "failed to resolve utility pages"
		if err != nil {
			message = err.Error()
		}

		kdexv1alpha1.SetConditions(
			&host.Status.Conditions,
			kdexv1alpha1.ConditionStatuses{
				Degraded:    metav1.ConditionTrue,
				Progressing: metav1.ConditionFalse,
				Ready:       metav1.ConditionFalse,
			},
			kdexv1alpha1.ConditionReasonReconcileSuccess,
			message,
		)
		return r1, err
	}

	// Optional references (ThemeRef, ScriptLibraryRef) are resolved for status
	// tracking, but a missing or not-yet-ready reference must NOT abort the
	// reconcile: the rest of the spec still has to be mirrored to the
	// KDexInternalHost so unrelated changes (e.g. a host-manager version bump
	// carrying security fixes) reach the downstream controller. We record the
	// dependency in status and requeue after mirroring instead of returning
	// early here. The Watches on KDexTheme / KDexClusterTheme drive the
	// event-driven recovery once the reference appears. See issue #2.
	refsUnresolved := false
	refsRequeue := ctrl.Result{}
	noteUnresolvedRef := func(message string, r1 ctrl.Result) {
		refsUnresolved = true
		if r1.RequeueAfter > refsRequeue.RequeueAfter {
			refsRequeue = r1
		}
		kdexv1alpha1.SetConditions(
			&host.Status.Conditions,
			kdexv1alpha1.ConditionStatuses{
				Degraded:    metav1.ConditionFalse,
				Progressing: metav1.ConditionTrue,
				Ready:       metav1.ConditionFalse,
			},
			kdexv1alpha1.ConditionReasonReconciling,
			message,
		)
	}

	// A Consent page that does not resolve falls back to host-manager's
	// built-in consent page; it is recorded like an unresolved optional ref.
	if consentUnresolved != nil {
		noteUnresolvedRef(consentUnresolved.message, consentUnresolved.requeue)
	}

	var themeObj runtime.Object
	if host.Spec.ThemeRef != nil {
		themeObj, shouldReturn, r1, err = ResolveKDexObjectReference(ctx, r.Client, &host, &host.Status.Conditions, host.Spec.ThemeRef, r.RequeueDelay)
		if shouldReturn {
			message := "failed to resolve theme"
			if err != nil {
				message = err.Error()
			}
			noteUnresolvedRef(message, r1)
			// Swallow the resolution error so reconcile continues to the spec
			// mirror below; recovery is event-driven via Watches plus requeue.
			err = nil
		} else if themeObj != nil {
			if host.Status.Attributes == nil {
				host.Status.Attributes = make(map[string]string)
			}
			host.Status.Attributes["theme.generation"] = fmt.Sprintf("%d", themeObj.(client.Object).GetGeneration())
		}
	}

	var scriptLibraryObj runtime.Object
	if host.Spec.ScriptLibraryRef != nil {
		var r1 ctrl.Result
		scriptLibraryObj, shouldReturn, r1, err = ResolveKDexObjectReference(ctx, r.Client, &host, &host.Status.Conditions, host.Spec.ScriptLibraryRef, r.RequeueDelay)
		if shouldReturn {
			message := "failed to resolve script library"
			if err != nil {
				message = err.Error()
			}
			noteUnresolvedRef(message, r1)
			err = nil
		} else if scriptLibraryObj != nil {
			if host.Status.Attributes == nil {
				host.Status.Attributes = make(map[string]string)
			}
			host.Status.Attributes["scriptLibrary.generation"] = fmt.Sprintf("%d", scriptLibraryObj.(client.Object).GetGeneration())
		}
	}

	translationRefs, translationCollision, shouldReturn, r1, err := r.resolveTranslations(ctx, &host)
	if shouldReturn {
		log.Info("resolveTranslations requested return", "requeueAfter", r1.RequeueAfter, "err", err)
		if err == nil && r1.RequeueAfter > 0 {
			return r1, nil
		}

		message := "failed to resolve translations"
		if err != nil {
			message = err.Error()
		}

		kdexv1alpha1.SetConditions(
			&host.Status.Conditions,
			kdexv1alpha1.ConditionStatuses{
				Degraded:    metav1.ConditionTrue,
				Progressing: metav1.ConditionFalse,
				Ready:       metav1.ConditionFalse,
			},
			kdexv1alpha1.ConditionReasonReconcileSuccess,
			message,
		)
		return r1, err
	}

	// An unparseable extensionSelector fails closed: no extensions apply (nil),
	// the internal host is still written, and the host is marked Degraded at
	// the end of the readiness ladder so later steps cannot overwrite it.
	var extensionSelectorErr error
	extensions, err := r.resolveExtensions(ctx, &host)
	if errors.Is(err, errInvalidExtensionSelector) {
		extensionSelectorErr = err
		err = nil
	}
	if err != nil {
		kdexv1alpha1.SetConditions(
			&host.Status.Conditions,
			kdexv1alpha1.ConditionStatuses{
				Degraded:    metav1.ConditionTrue,
				Progressing: metav1.ConditionFalse,
				Ready:       metav1.ConditionFalse,
			},
			kdexv1alpha1.ConditionReasonReconcileError,
			err.Error(),
		)
		return ctrl.Result{}, err
	}

	log.Info("calling reconcileHelmReleases", "host", host.Name)
	helmOp, err := r.reconcileHelmReleases(ctx, &host, secrets, log)
	if err != nil {
		kdexv1alpha1.SetConditions(
			&host.Status.Conditions,
			kdexv1alpha1.ConditionStatuses{
				Degraded:    metav1.ConditionTrue,
				Progressing: metav1.ConditionFalse,
				Ready:       metav1.ConditionFalse,
			},
			kdexv1alpha1.ConditionReasonReconcileSuccess,
			err.Error(),
		)
		return ctrl.Result{}, err
	}

	// Always update the internal host resource, even if Helm is in progress or failed.
	// This ensures that utility page references and other attributes are updated in a timely manner,
	// which is especially important for tests that monitor these attributes.
	var internalHostOp controllerutil.OperationResult
	var internalHost *kdexv1alpha1.KDexInternalHost
	internalHostOp, internalHost, err = r.createOrUpdateInternalHostResource(ctx, &host, utilityPageRefs, translationRefs, extensions)
	if err != nil {
		kdexv1alpha1.SetConditions(
			&host.Status.Conditions,
			kdexv1alpha1.ConditionStatuses{
				Degraded:    metav1.ConditionTrue,
				Progressing: metav1.ConditionFalse,
				Ready:       metav1.ConditionFalse,
			},
			kdexv1alpha1.ConditionReasonReconcileSuccess,
			err.Error(),
		)
		return ctrl.Result{}, err
	}

	log.Info("reconciled", "host", host.Name, "namespace", host.Namespace, "helmOp", helmOp, "internalHostOp", internalHostOp)

	// If an optional reference (theme, script library) was unresolved, the spec
	// has now been mirrored to the KDexInternalHost. Stop here with the
	// Progressing condition set above and requeue; the host is not Ready until
	// the reference resolves, but its spec changes have already propagated. See
	// issue #2.
	if refsUnresolved {
		log.Info("host has unresolved references; mirrored spec to internal host, requeuing",
			"host", host.Name, "namespace", host.Namespace)
		return refsRequeue, nil
	}

	// Sequential readiness checks:
	// 1. Helm Operation Status
	helmStatus := host.Status.Attributes[AttributeHelmReleaseStatus]
	if helmStatus == HelmStatusInProgress {
		log.V(2).Info("helm in progress", "host", host.Name, "namespace", host.Namespace)
		return ctrl.Result{RequeueAfter: r.RequeueDelay}, nil
	}
	if helmStatus == HelmStatusFailed {
		log.V(2).Info("helm failed", "host", host.Name, "namespace", host.Namespace)
		return ctrl.Result{RequeueAfter: r.RequeueDelay}, nil
	}

	// 2. Deployment Status
	// Since we are using Helm, we'll query the deployment created by Helm.
	deployment := &appsv1.Deployment{}
	err = r.Get(ctx, req.NamespacedName, deployment)
	if err != nil {
		if apierrors.IsNotFound(err) {
			kdexv1alpha1.SetConditions(
				&host.Status.Conditions,
				kdexv1alpha1.ConditionStatuses{
					Degraded:    metav1.ConditionFalse,
					Progressing: metav1.ConditionTrue,
					Ready:       metav1.ConditionFalse,
				},
				kdexv1alpha1.ConditionReasonReconcileSuccess,
				"Waiting for deployment to be created by Helm.",
			)
			return ctrl.Result{RequeueAfter: r.RequeueDelay}, nil
		}
		return ctrl.Result{}, err
	}

	for _, cond := range deployment.Status.Conditions {
		if cond.Type == appsv1.DeploymentAvailable && cond.Status != corev1.ConditionTrue {
			kdexv1alpha1.SetConditions(
				&host.Status.Conditions,
				kdexv1alpha1.ConditionStatuses{
					Degraded:    metav1.ConditionFalse,
					Progressing: metav1.ConditionTrue,
					Ready:       metav1.ConditionFalse,
				},
				kdexv1alpha1.ConditionReasonReconcileSuccess,
				fmt.Sprintf("Waiting for deployment %s/%s to be ready.", deployment.Namespace, deployment.Name),
			)
			return ctrl.Result{RequeueAfter: r.RequeueDelay}, nil
		}
	}

	// 3. Internal Host Readiness
	if meta.IsStatusConditionFalse(internalHost.Status.Conditions, string(kdexv1alpha1.ConditionTypeReady)) {
		kdexv1alpha1.SetConditions(
			&host.Status.Conditions,
			kdexv1alpha1.ConditionStatuses{
				Degraded:    metav1.ConditionFalse,
				Progressing: metav1.ConditionTrue,
				Ready:       metav1.ConditionFalse,
			},
			kdexv1alpha1.ConditionReasonReconcileSuccess,
			"Waiting for internal host to be ready.",
		)
		return ctrl.Result{RequeueAfter: r.RequeueDelay}, nil
	}

	if val, ok := internalHost.Status.Attributes["ingress"]; ok {
		if host.Status.Attributes == nil {
			host.Status.Attributes = make(map[string]string)
		}
		host.Status.Attributes["ingress"] = val
	}

	if extensionSelectorErr != nil {
		kdexv1alpha1.SetConditions(
			&host.Status.Conditions,
			kdexv1alpha1.ConditionStatuses{
				Degraded:    metav1.ConditionTrue,
				Progressing: metav1.ConditionFalse,
				Ready:       metav1.ConditionFalse,
			},
			kdexv1alpha1.ConditionReasonReconcileError,
			extensionSelectorErr.Error(),
		)
		return ctrl.Result{}, nil
	}

	// Two distinct translations map to one KDexInternalTranslation name. The
	// higher-precedence one is already being served; the host is Degraded,
	// not Ready, until an author renames one. Any fix arrives as a watched
	// edit, so no requeue.
	if translationCollision != "" {
		setTranslationCollisionCondition(&host, translationCollision)
		return ctrl.Result{}, nil
	}

	kdexv1alpha1.SetConditions(
		&host.Status.Conditions,
		kdexv1alpha1.ConditionStatuses{
			Degraded:    metav1.ConditionFalse,
			Progressing: metav1.ConditionFalse,
			Ready:       metav1.ConditionTrue,
		},
		kdexv1alpha1.ConditionReasonReconcileSuccess,
		"Reconciliation successful",
	)

	log.V(1).Info(
		"reconciled",
		"helmOp", helmOp,
		"internalHostOp", internalHostOp,
	)

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *KDexHostReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if os.Getenv("ENABLE_WEBHOOKS") != FALSE {
		err := ctrl.NewWebhookManagedBy(mgr, &kdexv1alpha1.KDexHost{}).
			WithDefaulter(&nexuswebhook.KDexHostDefaulter[*kdexv1alpha1.KDexHost]{
				Configuration: r.Configuration,
			}).
			WithValidator(&nexuswebhook.KDexHostValidator[*kdexv1alpha1.KDexHost]{}).
			Complete()

		if err != nil {
			return err
		}
	}

	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &kdexv1alpha1.KDexInternalTranslation{}, hostIndexKey, indexInternalTranslationByHost); err != nil {
		return err
	}

	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &kdexv1alpha1.KDexTranslation{}, hostIndexKey, indexTranslationByHostRef); err != nil {
		return err
	}
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &kdexv1alpha1.KDexHostExtension{}, hostIndexKey, indexExtensionByHostRef); err != nil {
		return err
	}

	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &kdexv1alpha1.KDexInternalUtilityPage{}, hostIndexKey, indexInternalUtilityPageByHost); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&kdexv1alpha1.KDexHost{}).
		Owns(&kdexv1alpha1.KDexInternalHost{}).
		Owns(&kdexv1alpha1.KDexInternalTranslation{}).
		Owns(&kdexv1alpha1.KDexInternalUtilityPage{}).
		Watches(
			&kdexv1alpha1.KDexScriptLibrary{},
			MakeHandlerByReferencePath(r.Client, r.Scheme, &kdexv1alpha1.KDexHost{}, &kdexv1alpha1.KDexHostList{}, "{.Spec.ScriptLibraryRef}")).
		Watches(
			&kdexv1alpha1.KDexClusterScriptLibrary{},
			MakeHandlerByReferencePath(r.Client, r.Scheme, &kdexv1alpha1.KDexHost{}, &kdexv1alpha1.KDexHostList{}, "{.Spec.ScriptLibraryRef}")).
		Watches(
			&kdexv1alpha1.KDexFaaSAdaptor{},
			MakeHandlerByReferencePath(r.Client, r.Scheme, &kdexv1alpha1.KDexHost{}, &kdexv1alpha1.KDexHostList{}, "{.Spec.FaaSAdaptorRef}")).
		Watches(
			&kdexv1alpha1.KDexClusterFaaSAdaptor{},
			MakeHandlerByReferencePath(r.Client, r.Scheme, &kdexv1alpha1.KDexHost{}, &kdexv1alpha1.KDexHostList{}, "{.Spec.FaaSAdaptorRef}")).
		Watches(
			&kdexv1alpha1.KDexTheme{},
			MakeHandlerByReferencePath(r.Client, r.Scheme, &kdexv1alpha1.KDexHost{}, &kdexv1alpha1.KDexHostList{}, "{.Spec.ThemeRef}")).
		Watches(
			&kdexv1alpha1.KDexClusterTheme{},
			MakeHandlerByReferencePath(r.Client, r.Scheme, &kdexv1alpha1.KDexHost{}, &kdexv1alpha1.KDexHostList{}, "{.Spec.ThemeRef}")).
		Watches(
			&kdexv1alpha1.KDexTranslation{},
			MakeHandlerByReferencePath(r.Client, r.Scheme, &kdexv1alpha1.KDexHost{}, &kdexv1alpha1.KDexHostList{}, "{.Spec.TranslationRefs[*]}")).
		Watches(
			&kdexv1alpha1.KDexTranslation{},
			handler.EnqueueRequestsFromMapFunc(translationHostRefRequests)).
		Watches(
			&kdexv1alpha1.KDexHostExtension{},
			handler.EnqueueRequestsFromMapFunc(extensionHostRefRequests)).
		Watches(
			&kdexv1alpha1.KDexClusterTranslation{},
			MakeHandlerByReferencePath(r.Client, r.Scheme, &kdexv1alpha1.KDexHost{}, &kdexv1alpha1.KDexHostList{}, "{.Spec.TranslationRefs[*]}")).
		Watches(
			&kdexv1alpha1.KDexUtilityPage{},
			MakeHandlerByReferencePath(r.Client, r.Scheme, &kdexv1alpha1.KDexHost{}, &kdexv1alpha1.KDexHostList{}, "{.Spec.UtilityPages.AnnouncementRef}", "{.Spec.UtilityPages.ErrorRef}", "{.Spec.UtilityPages.LoginRef}", "{.Spec.UtilityPages.ConsentRef}")).
		Watches(
			&kdexv1alpha1.KDexClusterUtilityPage{},
			MakeHandlerByReferencePath(r.Client, r.Scheme, &kdexv1alpha1.KDexHost{}, &kdexv1alpha1.KDexHostList{}, "{.Spec.UtilityPages.AnnouncementRef}", "{.Spec.UtilityPages.ErrorRef}", "{.Spec.UtilityPages.LoginRef}", "{.Spec.UtilityPages.ConsentRef}")).
		Watches(
			&kdexv1alpha1.KDexClusterUtilityPage{},
			handler.EnqueueRequestsFromMapFunc(r.defaultConsentPageRequests)).
		Watches(
			&rbacv1.ClusterRoleBinding{},
			handler.EnqueueRequestsFromMapFunc(r.clusterReaderBindingRequests)).
		Watches(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
				secret, ok := obj.(*corev1.Secret)
				if !ok {
					return nil
				}

				var hostList kdexv1alpha1.KDexHostList
				if err := r.List(ctx, &hostList, client.InNamespace(secret.Namespace)); err != nil {
					return nil
				}

				var requests []reconcile.Request
				secretLabels := labels.Set(secret.Labels)
				for i := range hostList.Items {
					host := &hostList.Items[i]
					if host.Spec.SecretSelector == nil {
						continue
					}
					sel, err := metav1.LabelSelectorAsSelector(host.Spec.SecretSelector)
					if err != nil {
						continue
					}
					if !sel.Matches(secretLabels) {
						continue
					}
					requests = append(requests, reconcile.Request{
						NamespacedName: types.NamespacedName{
							Name:      host.Name,
							Namespace: host.Namespace,
						},
					})
				}
				return requests
			})).
		WithOptions(controller.TypedOptions[reconcile.Request]{
			LogConstructor: LogConstructor("kdexhost", mgr),
		}).
		Named("kdexhost").
		Complete(r)
}

func (r *KDexHostReconciler) createOrUpdateInternalHostResource(
	ctx context.Context,
	host *kdexv1alpha1.KDexHost,
	utilityPageRefs map[kdexv1alpha1.KDexUtilityPageType]*corev1.LocalObjectReference,
	translationRefs []corev1.LocalObjectReference,
	extensions []kdexv1alpha1.InternalHostExtension,
) (controllerutil.OperationResult, *kdexv1alpha1.KDexInternalHost, error) {
	internalHost := &kdexv1alpha1.KDexInternalHost{
		ObjectMeta: metav1.ObjectMeta{
			Name:      host.Name,
			Namespace: host.Namespace,
		},
	}

	op, err := ctrl.CreateOrUpdate(ctx, r.Client, internalHost, func() error {
		// NOTE: only mutate spec/metadata here. ctrl.CreateOrUpdate diffs the
		// whole object and issues an Update when it changes, but Update does not
		// persist the status subresource. Initializing internalHost.Status here
		// made the in-memory object differ from the fetched one on every pass
		// (the empty map is never persisted), so CreateOrUpdate issued a
		// needless Update each reconcile, bumping resourceVersion and fanning
		// out downstream watchers for no semantic change. See issue #19.
		// Only allocate the maps when there is something to copy. Initializing
		// an empty map that the apiserver then drops to nil on persist makes the
		// in-memory object differ from the fetched one on every pass, which
		// would defeat the no-op detection below. See issue #19.
		if len(host.Annotations) > 0 {
			if internalHost.Annotations == nil {
				internalHost.Annotations = make(map[string]string)
			}
			maps.Copy(internalHost.Annotations, host.Annotations)
		}
		if internalHost.Labels == nil {
			internalHost.Labels = make(map[string]string)
		}
		maps.Copy(internalHost.Labels, host.Labels)

		if internalHost.CreationTimestamp.IsZero() {
			internalHost.Labels["app.kubernetes.io/name"] = kdexWeb
			internalHost.Labels["kdex.dev/instance"] = host.Name
		}

		if internalHost.Labels == nil {
			internalHost.Labels = make(map[string]string)
		}
		internalHost.Labels["kdex.dev/generation"] = fmt.Sprintf("%d", host.Generation)
		internalHost.Spec.KDexHostSpec = host.Spec
		internalHost.Spec.AnnouncementRef = utilityPageRefs[kdexv1alpha1.AnnouncementUtilityPageType]
		internalHost.Spec.ConsentRef = utilityPageRefs[kdexv1alpha1.ConsentUtilityPageType]
		internalHost.Spec.ErrorRef = utilityPageRefs[kdexv1alpha1.ErrorUtilityPageType]
		internalHost.Spec.LoginRef = utilityPageRefs[kdexv1alpha1.LoginUtilityPageType]
		internalHost.Spec.InternalTranslationRefs = translationRefs
		internalHost.Spec.Extensions = extensions

		return ctrl.SetControllerReference(host, internalHost, r.Scheme)
	})

	log := logf.FromContext(ctx)

	log.V(2).Info(
		"createOrUpdateInternalHostResource",
		"name", internalHost.Name,
		"op", op,
		"utilityPageRefs", utilityPageRefs,
		"translationRefs", translationRefs,
		"err", err,
	)

	if err != nil {
		kdexv1alpha1.SetConditions(
			&host.Status.Conditions,
			kdexv1alpha1.ConditionStatuses{
				Degraded:    metav1.ConditionTrue,
				Progressing: metav1.ConditionFalse,
				Ready:       metav1.ConditionFalse,
			},
			kdexv1alpha1.ConditionReasonReconcileError,
			err.Error(),
		)

		return controllerutil.OperationResultNone, nil, err
	}

	return op, internalHost, nil
}

// helmClientKey names a host's cached Helm client. A name cannot contain "/",
// so no two hosts share a key; joined by "-", host "a-b" in "c" and host "a"
// in "b-c" did, and one got a client bound to the other's namespace.
func helmClientKey(name, namespace string) string {
	return namespace + "/" + name
}

func (r *KDexHostReconciler) deleteHelmClient(name string, namespace string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.helmClients == nil {
		r.helmClients = map[string]cachedHelmClient{}
	}

	delete(r.helmClients, helmClientKey(name, namespace))

	return nil
}

func (r *KDexHostReconciler) getOrCreateHelmClient(name string, namespace string, secrets kdexv1alpha1.Secrets, logger logr.Logger) (utils.HelmClientInterface, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.helmClients == nil {
		r.helmClients = map[string]cachedHelmClient{}
	}

	key := helmClientKey(name, namespace)
	hash := utils.HashSecrets(secrets)

	cached, ok := r.helmClients[key]

	// Rebuild the client when there is no cached entry, or when the resolved
	// secrets have changed (e.g. registry credential rotation). Reusing a stale
	// client would keep installing/upgrading charts with dead credentials.
	if !ok || cached.hash != hash {
		helmClient, err := r.HelmClientFactory(namespace, secrets, logger)
		if err != nil {
			return nil, err
		}

		cached = cachedHelmClient{client: helmClient, hash: hash}
		r.helmClients[key] = cached
	}

	return cached.client, nil
}

// awaitHelmRunBeforeTeardown stops any helm operation queued for a terminating
// host, and reports whether its teardown must wait -- and when to look again --
// for one already running.
//
// The cancel stops a render still queued for a slot, so it cannot re-install
// what teardown removes. It cannot stop a goroutine already inside a Helm call,
// which takes no context. One installing a companion has no release record for
// it yet, so teardown's label listing would not find it, and the install would
// land after the finalizer is gone: labelled for a host that no longer exists.
// So teardown waits for it to return -- it starts no further Helm call once it
// sees the host terminating -- and says so on the host. But not forever: past
// helmTeardownWaitCap the call has outlived every deadline it has, and a
// deletion held on it would be held by whoever controls the chart's registry.
func (r *KDexHostReconciler) awaitHelmRunBeforeTeardown(host *kdexv1alpha1.KDexHost, log logr.Logger) (ctrl.Result, bool) {
	key := client.ObjectKeyFromObject(host)
	r.cancelHelmOperation(key)
	if !r.helmRunInFlight(key) {
		return ctrl.Result{}, false
	}
	if waited := time.Since(host.DeletionTimestamp.Time); waited > helmTeardownWaitCap {
		log.Info("tearing down despite an in-flight helm operation; a release it is still installing may be left behind",
			"host", host.Name, "namespace", host.Namespace, "waited", waited.String())
		return ctrl.Result{}, false
	}
	log.V(1).Info("waiting for an in-flight helm operation before tearing down", "host", host.Name)
	// Degraded is left as the operation last reported it.
	kdexv1alpha1.SetConditions(
		&host.Status.Conditions,
		kdexv1alpha1.ConditionStatuses{
			Progressing: metav1.ConditionTrue,
			Ready:       metav1.ConditionFalse,
		},
		kdexv1alpha1.ConditionReasonReconciling,
		fmt.Sprintf("Deletion is waiting for an in-flight Helm operation to finish before uninstalling this host's releases "+
			"(for at most %s after deletion began).", helmTeardownWaitCap),
	)
	return ctrl.Result{RequeueAfter: helmTeardownPoll}, true
}

// indexInternalUtilityPageByHost indexes a KDexInternalUtilityPage by the host
// it belongs to, which teardown lists them by.
func indexInternalUtilityPageByHost(rawObj client.Object) []string {
	utilityPage, ok := rawObj.(*kdexv1alpha1.KDexInternalUtilityPage)
	if !ok || utilityPage.Spec.HostRef.Name == "" {
		return nil
	}
	return []string{utilityPage.Spec.HostRef.Name}
}
