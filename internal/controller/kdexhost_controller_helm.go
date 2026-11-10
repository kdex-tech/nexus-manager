package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/kdex-tech/nexus-manager/internal/utils"
	"helm.sh/helm/v4/pkg/chart/common"
	"helm.sh/helm/v4/pkg/chart/common/util"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/types"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
	"kdex.dev/crds/configuration"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	AttributeHelmReleaseStatus      = "helm.release.status"
	AttributeHelmReleaseGeneration  = "helm.release.generation"
	AttributeHelmReleaseHash        = "helm.release.hash"
	AttributeHelmReleaseError       = "helm.release.error"
	AttributeHelmReleaseOwner       = "helm.release.owner"
	AttributeHelmReleaseLastAttempt = "helm.release.last-attempt"
	// AttributeCompanionReleases records, comma-separated and sorted, every
	// companion release this operator has installed (or is about to: a name
	// is recorded before its install) for the host and not removed since. It lives in status, which only the operator writes, and
	// is the authority for what a prune or a teardown may uninstall. It
	// carries the helm.release. prefix because the async helm operation
	// writes it: the reconcile's own status patch preserves exactly those
	// keys from the server rather than overwriting them with its snapshot.
	AttributeCompanionReleases = "helm.release.companions"

	HelmStatusInProgress = "in_progress"
	HelmStatusCompleted  = "completed"
	HelmStatusFailed     = "failed"

	// LabelCompanionOf marks a Helm release as a companion chart installed by
	// this operator on behalf of the named KDexHost. Helm keeps the label on
	// the release's record Secret, which anyone able to write Secrets in the
	// namespace can set, so it only ever narrows: a release is this host's to
	// prune or uninstall when it carries the label AND the operator recorded
	// installing it (AttributeCompanionReleases). See #46.
	LabelCompanionOf = "kdex.dev/companion-of"
)

// companionLabels are stamped on every companion release and used as the
// discovery selector when pruning. Helm rejects its own reserved label names
// (name, owner, status, version, createdAt, modifiedAt); this key is namespaced
// and collides with none of them.
func companionLabels(hostName string) map[string]string {
	return map[string]string{LabelCompanionOf: hostName}
}

// SetConfiguration replaces the cluster configuration under the same lock
// getConfiguration takes.
//
// r.Configuration is NOT immutable after startup, despite only ever being
// "assigned" once: encoder.Encode calls SetGroupVersionKind, which writes the
// object's TypeMeta. That write is why getConfiguration holds r.mu at all, and
// it means any other writer must hold the same lock or race with an in-flight
// encode. The race detector catches this the moment both run concurrently.
func (r *KDexHostReconciler) SetConfiguration(config configuration.NexusConfiguration) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.Configuration = config
}

// GetConfigurationCopy returns a copy of the cluster configuration taken under
// the lock, so a caller can derive a modified configuration without reading
// fields while an encode is writing TypeMeta.
func (r *KDexHostReconciler) GetConfigurationCopy() configuration.NexusConfiguration {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return *r.Configuration.DeepCopy()
}

// hostDefault returns a copy of the cluster-wide host defaults, taken under the
// lock that guards the configuration.
func (r *KDexHostReconciler) hostDefault() configuration.HostDefault {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return *r.Configuration.HostDefault.DeepCopy()
}

func (r *KDexHostReconciler) getConfiguration() ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	codecs := serializer.NewCodecFactory(r.Scheme)

	info, ok := runtime.SerializerInfoForMediaType(codecs.SupportedMediaTypes(), "application/yaml")
	if !ok {
		return nil, fmt.Errorf("no YAML serializer found")
	}

	encoder := codecs.EncoderForVersion(info.Serializer, configuration.GroupVersion)

	var buf bytes.Buffer
	if err := encoder.Encode(&r.Configuration, &buf); err != nil {
		return nil, fmt.Errorf("failed to encode object to YAML: %w", err)
	}

	return buf.Bytes(), nil
}

// computeHelmReleaseHash gates an expensive chart pull + render, so it hashes
// what is actually sent to Helm rather than the whole KDexHost spec. Most of
// that spec — brand, routing, theme/page/translation refs — reaches the host
// through KDexInternalHost and never appears in the chart values, so folding it
// in charged a full re-render for edits that could not change a rendered byte.
//
// The inputs are the effective host-manager ChartSpec plus the companion chart
// declarations, which are reconciled and pruned inside the same gated pass.
// The cluster role is already baked into the values as roleRef.name, so hashing
// the values covers it.
//
// Note this does NOT narrow cluster-wide invalidation: the entire
// NexusConfiguration is handed to every host as .Values.config, and the
// host-manager chart renders it into a ConfigMap plus a checksum annotation, so
// a cluster-default edit genuinely changes every host's manifest. See #27.
func (r *KDexHostReconciler) computeHelmReleaseHash(host *kdexv1alpha1.KDexHost) (string, error) {
	spec, err := r.buildHostManagerChartSpec(host)
	if err != nil {
		return "", err
	}

	var companions []kdexv1alpha1.CompanionChart
	if host.Spec.Helm != nil {
		companions = host.Spec.Helm.CompanionCharts
	}

	inputs := struct {
		ReleaseName string                        `json:"releaseName"`
		ChartName   string                        `json:"chartName"`
		Namespace   string                        `json:"namespace"`
		Version     string                        `json:"version"`
		Values      map[string]any                `json:"values"`
		Companions  []kdexv1alpha1.CompanionChart `json:"companions"`
	}{
		ReleaseName: spec.ReleaseName,
		ChartName:   spec.ChartName,
		Namespace:   spec.Namespace,
		Version:     spec.Version,
		Values:      spec.Values,
		Companions:  companions,
	}

	b, err := json.Marshal(inputs)
	if err != nil {
		return "", err
	}

	hash := sha256.Sum256(b)
	return fmt.Sprintf("%x", hash), nil
}

func (r *KDexHostReconciler) trySetHelmOperationActive(key types.NamespacedName, cancel context.CancelFunc, hash string) bool {
	r.mu.Lock()
	if r.activeHelmOperations == nil {
		r.activeHelmOperations = make(map[types.NamespacedName]helmOperation)
	}
	if old, active := r.activeHelmOperations[key]; active {
		if old.hash == hash {
			r.mu.Unlock()
			return false
		}
		// Cancel old generation if it's still running
		// We do this outside the lock to avoid any potential deadlock if cancel()
		// synchronously triggers something that needs the lock.
		r.mu.Unlock()
		old.cancel()

		r.mu.Lock()
	}
	r.activeHelmOperations[key] = helmOperation{cancel, hash}
	r.mu.Unlock()
	return true
}

func (r *KDexHostReconciler) clearHelmOperationActive(key types.NamespacedName, hash string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.activeHelmOperations != nil {
		if op, exists := r.activeHelmOperations[key]; exists && op.hash == hash {
			delete(r.activeHelmOperations, key)
		}
	}
}

// cancelHelmOperation cancels and forgets any in-flight Helm operation for the
// host. Called on teardown so a render still queued behind the fleet cannot
// wake up after the CR is gone and re-install what deletion just removed. The
// cancel only unblocks a goroutine parked in acquireHelmRenderSlot -- Helm's
// own calls take no context -- so runAsyncHelmReconcile re-checks the host
// after it acquires a slot rather than relying on this alone.
func (r *KDexHostReconciler) cancelHelmOperation(key types.NamespacedName) {
	r.mu.Lock()
	op, active := r.activeHelmOperations[key]
	if active {
		delete(r.activeHelmOperations, key)
	}
	r.mu.Unlock()

	// Outside the lock: cancel() may synchronously run work that needs it.
	if active {
		op.cancel()
	}
}

func (r *KDexHostReconciler) isHelmOperationActive(key types.NamespacedName, hash string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.activeHelmOperations == nil {
		return false
	}
	op, active := r.activeHelmOperations[key]
	return active && op.hash == hash
}

func (r *KDexHostReconciler) reconcileHelmReleases(ctx context.Context, host *kdexv1alpha1.KDexHost, secrets kdexv1alpha1.Secrets, log logr.Logger) (controllerutil.OperationResult, error) {
	log.V(2).Info("reconcileHelmReleases#1", "host", host.Name, "namespace", host.Namespace)

	// Fetch a fresh copy of the host to ensure we have the latest status and generation
	latestHost := &kdexv1alpha1.KDexHost{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(host), latestHost); err != nil {
		log.Error(err, "failed to get latest host", "namespace", host.Namespace, "name", host.Name)
		return controllerutil.OperationResultNone, fmt.Errorf("failed to get latest host: %w", err)
	}

	// Update the local object's generation and resource version to match the latest from the server.
	// We DO NOT overwrite host.Status directly because it may contain changes from earlier in the reconcile loop
	// (like resolved utility page generations) that haven't been persisted yet.
	host.Generation = latestHost.Generation
	host.ResourceVersion = latestHost.ResourceVersion

	currentHash, err := r.computeHelmReleaseHash(host)
	if err != nil {
		return controllerutil.OperationResultNone, fmt.Errorf("failed to compute helm release hash: %w", err)
	}

	// Check if a Helm operation is already in progress using the latest status from the server
	status := latestHost.Status.Attributes[AttributeHelmReleaseStatus]
	recordedHash := latestHost.Status.Attributes[AttributeHelmReleaseHash]

	key := client.ObjectKeyFromObject(host)
	if status == HelmStatusInProgress {
		// If it's marked in progress in status, check if we actually have a goroutine running for it
		if r.isHelmOperationActive(key, currentHash) {
			// Still working, requeue
			log.V(2).Info("reconcileHelmReleases#2", "host", host.Name, "namespace", host.Namespace)
			return controllerutil.OperationResultNone, nil
		}

		// If not active in memory, it means either:
		// 1. This controller instance just started and we are recovering a zombie.
		// 2. Another controller instance was running it and we are taking over (if it's no longer leader).
		owner := latestHost.Status.Attributes[AttributeHelmReleaseOwner]
		if owner == r.ControllerID {
			log.V(2).Info("Found zombie Helm operation in status from this instance, restarting", "host", host.Name, "namespace", host.Namespace)
		} else {
			log.V(2).Info("Found zombie Helm operation in status from another instance, taking over", "host", host.Name, "namespace", host.Namespace, "previousOwner", owner)
		}
	}

	if status == HelmStatusCompleted && recordedHash == currentHash {
		// Release is already in the desired state, nothing to do.
		log.Info("reconcileHelmReleases#3", "host", host.Name, "namespace", host.Namespace, "msg", "hash matches and release is healthy, skipping")
		return controllerutil.OperationResultNone, nil
	}

	if status == HelmStatusFailed && recordedHash == currentHash {
		// Last attempt failed for this same spec. The failure may have been
		// transient (a network policy that hadn't landed yet, a registry
		// blip, control-plane pressure during --wait), so we retry instead
		// of locking the release in a permanent Degraded state. A bounded
		// backoff governed by r.RequeueDelay keeps us from hammering when
		// the cause is chronic.
		if lastAttemptStr := latestHost.Status.Attributes[AttributeHelmReleaseLastAttempt]; lastAttemptStr != "" {
			if lastAttempt, parseErr := time.Parse(time.RFC3339, lastAttemptStr); parseErr == nil {
				elapsed := time.Since(lastAttempt)
				if elapsed < r.RequeueDelay {
					log.V(2).Info("reconcileHelmReleases#3b",
						"host", host.Name, "namespace", host.Namespace,
						"msg", "in backoff after failure",
						"elapsed", elapsed.String(),
						"interval", r.RequeueDelay.String())
					return controllerutil.OperationResultNone, nil
				}
			}
		}
		log.Info("reconcileHelmReleases#3c", "host", host.Name, "namespace", host.Namespace, "msg", "retrying after prior failure")
	}

	// Start a new Helm operation asynchronously
	log.V(2).Info("reconcileHelmReleases#4", "host", host.Name, "namespace", host.Namespace)
	if host.Status.Attributes == nil {
		host.Status.Attributes = make(map[string]string)
	}
	host.Status.Attributes[AttributeHelmReleaseStatus] = HelmStatusInProgress
	host.Status.Attributes[AttributeHelmReleaseHash] = currentHash
	host.Status.Attributes[AttributeHelmReleaseGeneration] = strconv.FormatInt(host.Generation, 10)
	host.Status.Attributes[AttributeHelmReleaseOwner] = r.ControllerID
	delete(host.Status.Attributes, AttributeHelmReleaseError)
	// Clear LastAttempt at the moment we transition into a new attempt. This
	// patch operates on freshly-Get'd state, so the diff definitively removes
	// the key on the server. The complementary delete in updateHelmStatus on
	// success can race with stale-cache reads; clearing here is the reliable
	// place to do it.
	delete(host.Status.Attributes, AttributeHelmReleaseLastAttempt)

	kdexv1alpha1.SetConditions(
		&host.Status.Conditions,
		kdexv1alpha1.ConditionStatuses{
			Progressing: metav1.ConditionTrue,
			Ready:       metav1.ConditionFalse,
		},
		kdexv1alpha1.ConditionReasonReconciling,
		"Helm installation in progress",
	)

	// The companion record is the helm operation's to write, and this host's
	// snapshot may predate the last one: carry the server's forward (#46).
	if rec, ok := latestHost.Status.Attributes[AttributeCompanionReleases]; ok {
		host.Status.Attributes[AttributeCompanionReleases] = rec
	}

	// We need to update the status now before starting the goroutine.
	// We use Patch to avoid conflicts with the defer block.
	patch := client.MergeFrom(latestHost.DeepCopy())
	latestHost.Status = host.Status
	if err := r.Status().Patch(ctx, latestHost, patch); err != nil {
		log.Error(err, "failed to patch status for helm progress", "namespace", host.Namespace, "name", host.Name)
		return controllerutil.OperationResultNone, fmt.Errorf("failed to patch status for helm progress: %w", err)
	}
	// Update the local object with the new resource version
	host.ResourceVersion = latestHost.ResourceVersion

	// Start the goroutine with a tracked context
	asyncCtx, cancel := context.WithCancel(r.Ctx)
	if !r.trySetHelmOperationActive(key, cancel, currentHash) {
		cancel()
		log.V(2).Info("reconcileHelmReleases#5", "host", host.Name, "namespace", host.Namespace, "msg", "already active")
		return controllerutil.OperationResultNone, nil
	}

	go r.runAsyncHelmReconcile(asyncCtx, host.Namespace, host.Name, currentHash, secrets, log)

	return controllerutil.OperationResultUpdated, nil
}

// acquireHelmRenderSlot blocks until a global render slot is available or ctx is
// cancelled, bounding the number of concurrent in-process Helm renders to
// HelmRenderConcurrency regardless of fleet size (issue #24). It returns a
// release func and whether a slot was acquired; when concurrency is not bounded
// (HelmRenderConcurrency <= 0) it returns immediately.
func (r *KDexHostReconciler) acquireHelmRenderSlot(ctx context.Context) (func(), bool) {
	r.helmRenderSemOnce.Do(func() {
		if r.HelmRenderConcurrency > 0 {
			r.helmRenderSem = make(chan struct{}, r.HelmRenderConcurrency)
		}
	})

	if r.helmRenderSem == nil {
		return func() {}, true
	}

	select {
	case r.helmRenderSem <- struct{}{}:
		// Idempotent so the caller can release early -- once the renders are
		// done and only deletions remain -- while still deferring it for the
		// error paths. Draining the channel twice would hand out a slot that
		// was never taken.
		var once sync.Once
		return func() { once.Do(func() { <-r.helmRenderSem }) }, true
	case <-ctx.Done():
		return func() {}, false
	}
}

func (r *KDexHostReconciler) runAsyncHelmReconcile(
	ctx context.Context,
	namespace, name string,
	hash string,
	secrets kdexv1alpha1.Secrets,
	log logr.Logger,
) {
	log.V(2).Info("runAsyncHelmReconcile#1", "namespace", namespace, "name", name)

	// Fetch a fresh copy of the host
	host := &kdexv1alpha1.KDexHost{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, host); err != nil {
		log.Error(err, "failed to get host", "namespace", namespace, "name", name)
		r.clearHelmOperationActive(client.ObjectKey{Namespace: namespace, Name: name}, hash)
		return // Host might have been deleted
	}

	if r.HelmClientFactory == nil {
		err := fmt.Errorf("HelmClientFactory is nil")
		log.Error(err, "controller misconfigured")
		r.updateHelmStatus(ctx, namespace, name, hash, err, nil, log)
		return
	}

	// Gate the heavy chart load + render behind the global concurrency bound.
	// Goroutines that are waiting here hold only their stack, not a render's
	// heap footprint, so peak memory stays a function of HelmRenderConcurrency
	// rather than the number of hosts. See issue #24.
	release, ok := r.acquireHelmRenderSlot(ctx)
	if !ok {
		log.V(2).Info("helm render cancelled while waiting for a render slot", "namespace", namespace, "name", name)
		r.clearHelmOperationActive(types.NamespacedName{Namespace: namespace, Name: name}, hash)
		return
	}
	defer release()

	// Re-read the host now that the slot is held. The wait above can last
	// minutes on a serialized fleet, and the snapshot taken before it says
	// nothing about what is still true: the host may have been deleted and
	// fully finalized in the meantime. Proceeding on the stale copy would
	// re-create the Helm client that deletion just removed and re-install the
	// release plus every companion the stale spec declares -- releases that
	// then run with no CR left in the cluster referencing them, invisible to
	// the prune because the stale spec still declares them.
	if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, host); err != nil {
		if !apierrors.IsNotFound(err) {
			log.Error(err, "failed to re-read host after acquiring a render slot", "namespace", namespace, "name", name)
		}
		log.V(2).Info("host is gone; abandoning queued helm render", "namespace", namespace, "name", name)
		r.clearHelmOperationActive(types.NamespacedName{Namespace: namespace, Name: name}, hash)
		return
	}
	if !host.DeletionTimestamp.IsZero() {
		log.V(2).Info("host is terminating; abandoning queued helm render", "namespace", namespace, "name", name)
		r.clearHelmOperationActive(types.NamespacedName{Namespace: namespace, Name: name}, hash)
		return
	}

	c, err := r.getOrCreateHelmClient(
		host.Name,
		host.Namespace,
		secrets,
		log.WithName("helm"),
	)
	if err != nil {
		r.updateHelmStatus(ctx, namespace, name, hash, err, nil, log)
		return
	}

	// 1. Reconcile kdex-host-manager chart
	if err := r.reconcileHostManagerChart(c, host); err != nil {
		r.updateHelmStatus(ctx, namespace, name, hash, err, nil, log)
		return
	}

	// 2. Reconcile companion charts. None may take a KDexHost's name: a host's
	// own release carries it, so the companion would replace that release.
	hostReleases, err := r.kdexHostNames(ctx, namespace)
	if err == nil {
		err = checkCompanionNames(host, hostReleases)
	}
	if err != nil {
		r.updateHelmStatus(ctx, namespace, name, hash, err, nil, log)
		return
	}
	// Record every declared companion as this operator's BEFORE installing
	// it, and install nothing if the record cannot be written: a release that
	// exists unrecorded can never be pruned or torn down (#46). A recorded
	// name with no release of its own is harmless, since only a release that
	// also carries the host's label is ever acted on.
	recorded, err := r.recordCompanions(ctx, host)
	if err != nil {
		r.updateHelmStatus(ctx, namespace, name, hash, err, nil, log)
		return
	}
	if host.Spec.Helm != nil {
		for _, companion := range host.Spec.Helm.CompanionCharts {
			if err := r.reconcileCompanionChart(c, host, companion); err != nil {
				r.updateHelmStatus(ctx, namespace, name, hash, err, nil, log)
				return
			}
		}
	}

	// Every chart load is done, so give the render slot back before the prune.
	// The prune loads no chart -- it lists releases and uninstalls -- but each
	// uninstall waits on foreground deletion for up to 5 minutes. Holding a
	// MEMORY bound across that would let one companion blocked by a finalizer
	// stall every other host's render, which is the whole failure this slot is
	// supposed to prevent rather than cause.
	release()

	// 3. Prune companions we own that are no longer declared. Deliberately
	// outside the guard above: dropping the entire spec.helm block orphans
	// every companion, which is exactly when pruning matters most. Runs after
	// the installs so a rename (x -> y) never leaves the host with neither.
	// What it removed leaves the record, whether or not it then failed.
	pruned, err := r.pruneCompanionCharts(c, host, hostReleases, log)
	record := nextCompanionRecord(recorded, nil, pruned)
	r.updateHelmStatus(ctx, namespace, name, hash, err, &record, log)
}

// recordCompanions adds every companion the host declares to
// AttributeCompanionReleases, writing status only when that changes it, and
// returns the record as it then stands.
func (r *KDexHostReconciler) recordCompanions(ctx context.Context, host *kdexv1alpha1.KDexHost) (map[string]struct{}, error) {
	recorded := recordedCompanions(host)
	var declared []string
	if host.Spec.Helm != nil {
		for _, companion := range host.Spec.Helm.CompanionCharts {
			declared = append(declared, companion.Name)
		}
	}
	next := nextCompanionRecord(recorded, declared, nil)
	if next == host.Status.Attributes[AttributeCompanionReleases] {
		return recorded, nil
	}
	patch := client.MergeFrom(host.DeepCopy())
	if host.Status.Attributes == nil {
		host.Status.Attributes = make(map[string]string)
	}
	host.Status.Attributes[AttributeCompanionReleases] = next
	if err := r.Status().Patch(ctx, host, patch); err != nil {
		return nil, fmt.Errorf("failed to record the companion releases before installing them: %w", err)
	}
	return recordedCompanions(host), nil
}

// kdexHostNames names every KDexHost in the namespace. Each one's own Helm
// release is named after it, so none of these names is a companion's.
func (r *KDexHostReconciler) kdexHostNames(ctx context.Context, namespace string) (map[string]struct{}, error) {
	var hosts kdexv1alpha1.KDexHostList
	if err := r.List(ctx, &hosts, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("failed to list the namespace's KDexHosts: %w", err)
	}
	names := make(map[string]struct{}, len(hosts.Items))
	for _, h := range hosts.Items {
		names[h.Name] = struct{}{}
	}
	return names, nil
}

// updateHelmStatus reports a helm reconcile's outcome on the host. A non-nil
// companionRecord replaces AttributeCompanionReleases; nil leaves it as it is.
func (r *KDexHostReconciler) updateHelmStatus(ctx context.Context, namespace, name string, hash string, err error, companionRecord *string, log logr.Logger) {
	log.V(2).Info("updateHelmStatus", "hash", hash, "err", err)

	defer r.clearHelmOperationActive(types.NamespacedName{Namespace: namespace, Name: name}, hash)

	host := &kdexv1alpha1.KDexHost{}
	if getErr := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, host); getErr != nil {
		log.Error(getErr, "failed to get host")
		return
	}

	patch := client.MergeFrom(host.DeepCopy())

	if host.Status.Attributes == nil {
		host.Status.Attributes = make(map[string]string)
	}

	if err != nil {
		host.Status.Attributes[AttributeHelmReleaseStatus] = HelmStatusFailed
		host.Status.Attributes[AttributeHelmReleaseError] = err.Error()
		// Timestamp the failure so the next reconcile can compute backoff
		// and decide whether to retry (see reconcileHelmReleases#3b/#3c).
		host.Status.Attributes[AttributeHelmReleaseLastAttempt] = time.Now().UTC().Format(time.RFC3339)
		kdexv1alpha1.SetConditions(
			&host.Status.Conditions,
			kdexv1alpha1.ConditionStatuses{
				Progressing: metav1.ConditionFalse,
				Degraded:    metav1.ConditionTrue,
			},
			kdexv1alpha1.ConditionReasonReconcileError,
			err.Error(),
		)
	} else {
		host.Status.Attributes[AttributeHelmReleaseStatus] = HelmStatusCompleted
		delete(host.Status.Attributes, AttributeHelmReleaseError)
		delete(host.Status.Attributes, AttributeHelmReleaseLastAttempt)
		kdexv1alpha1.SetConditions(
			&host.Status.Conditions,
			kdexv1alpha1.ConditionStatuses{
				Progressing: metav1.ConditionFalse,
			},
			kdexv1alpha1.ConditionReasonReconcileSuccess,
			"Helm installation completed",
		)
	}
	host.Status.Attributes[AttributeHelmReleaseHash] = hash
	host.Status.Attributes[AttributeHelmReleaseOwner] = r.ControllerID
	if companionRecord != nil {
		// Written even when empty: the reconcile's status patch preserves a
		// helm.release. key the server holds, but restores a deleted one from
		// its own stale snapshot.
		host.Status.Attributes[AttributeCompanionReleases] = *companionRecord
	}

	if patchErr := r.Status().Patch(ctx, host, patch); patchErr != nil {
		if !apierrors.IsNotFound(patchErr) && !apierrors.IsConflict(patchErr) {
			log.Error(patchErr, "failed to patch status")
		}
		log.V(2).Info("updateHelmStatus", "patchErr", patchErr)
	}
}

func (r *KDexHostReconciler) reconcileHostManagerChart(helmClient utils.HelmClientInterface, host *kdexv1alpha1.KDexHost) error {
	spec, err := r.buildHostManagerChartSpec(host)
	if err != nil {
		return err
	}

	return helmClient.InstallOrUpgrade(spec)
}

// buildHostManagerChartSpec computes exactly what will be handed to Helm for
// this host. It is the single source of truth for both the render and the
// release hash: deriving the hash from anything else invites the drift where
// the hash reports "unchanged" while the render would differ, which silently
// skips a needed upgrade.
func (r *KDexHostReconciler) buildHostManagerChartSpec(host *kdexv1alpha1.KDexHost) (*utils.ChartSpec, error) {
	configBytes, err := r.getConfiguration()
	if err != nil {
		return nil, err
	}

	configVals, err := common.ReadValues(configBytes)
	if err != nil {
		return nil, err
	}

	// Shift configVals down to the "config" key which matches the chart
	vals := map[string]any{}
	vals["config"] = configVals

	if len(host.Spec.Env) != 0 {
		vals["env"] = host.Spec.Env
	}

	// Seed cluster-wide default top-level chart values from NexusConfiguration
	// (e.g. resources, extraArgs, valkey topology). These land at the top level
	// alongside "config" and are merged UNDER the per-host overrides below, so a
	// per-host spec.helm.hostManager.values always wins. CoalesceTables merges at
	// top-level-key granularity, which matches how the chart reads `resources`
	// (whole-map `toYaml .Values.resources`).
	// Read the cluster configuration once, under the lock. getConfiguration's
	// encode writes the object's TypeMeta, so these reads are not free of a
	// concurrent writer just because the configuration is only assigned at
	// startup.
	hostDefault := r.hostDefault()

	if defaults := hostDefault.Chart.Values; defaults != nil && len(defaults.Raw) > 0 {
		defaultVals, err := common.ReadValues(defaults.Raw)
		if err != nil {
			return nil, err
		}
		vals = util.CoalesceTables(vals, defaultVals)
	}

	if host.Spec.Helm != nil && host.Spec.Helm.HostManager != nil {
		overrideVals, err := common.ReadValues([]byte(host.Spec.Helm.HostManager.Values))
		if err != nil {
			return nil, err
		}
		vals = util.CoalesceTables(overrideVals, vals)
	}

	chartName := hostDefault.Chart.Name
	version := hostDefault.Chart.Version

	if host.Spec.Helm != nil && host.Spec.Helm.HostManager != nil && host.Spec.Helm.HostManager.Version != "" {
		version = host.Spec.Helm.HostManager.Version
	}

	// Set last so they override all other configurations

	vals["focalHost"] = host.Name
	vals["fullnameOverride"] = host.Name

	// The namespaces host-manager caches, watches and resolves references in,
	// besides the host's own (host-manager#290). Always set, so a per-host
	// values override can't widen them; changing the list rolls host-manager,
	// whose cache namespaces are fixed at startup.
	referenceNamespaces := make([]any, 0, len(host.Spec.ReferenceNamespaces))
	for _, ns := range host.Spec.ReferenceNamespaces {
		referenceNamespaces = append(referenceNamespaces, ns)
	}
	vals["referenceNamespaces"] = referenceNamespaces

	hostManagerClusterRole := os.Getenv("HOST_MANAGER_CLUSTER_ROLE")

	roleRef := map[string]string{}
	roleRef["apiGroup"] = "rbac.authorization.k8s.io"
	roleRef["kind"] = "ClusterRole"

	// If the result of roleRef.name is blank the host-manager chart creates it's own ClusterRole
	roleRef["name"] = hostManagerClusterRole

	vals["roleRef"] = roleRef

	return &utils.ChartSpec{
		ReleaseName: host.Name,
		ChartName:   chartName,
		Namespace:   host.Namespace,
		Values:      vals,
		Version:     version,
		UpgradeCRDs: false,
	}, nil
}

func (r *KDexHostReconciler) reconcileCompanionChart(helmClient utils.HelmClientInterface, host *kdexv1alpha1.KDexHost, companion kdexv1alpha1.CompanionChart) error {
	if companion.Repository != "" {
		if err := helmClient.AddRepository(companion.Name, companion.Repository); err != nil {
			return err
		}
	}

	vals, err := common.ReadValues([]byte(companion.Values))
	if err != nil {
		return err
	}

	spec := &utils.ChartSpec{
		ReleaseName: companion.Name,
		ChartName:   companion.Chart,
		Namespace:   host.Namespace,
		Values:      vals,
		Version:     companion.Version,
		UpgradeCRDs: false,
		Labels:      companionLabels(host.Name),
	}

	return helmClient.InstallOrUpgrade(spec)
}

// pruneCompanionCharts uninstalls companion releases this operator installed
// for the host that the host no longer declares, and returns the ones it
// removed. Converging from desired state alone cannot do this: an entry removed
// from the list leaves no trace in the spec, so the release must be found some
// other way. Neither way alone is trusted (#46): the owner label is
// rediscovered from Helm's records, which anyone who can write Secrets in the
// namespace can label, and the operator's own record in status can name a
// release since taken over. A release is pruned only when both name it, and
// never when it is a KDexHost's own release.
func (r *KDexHostReconciler) pruneCompanionCharts(helmClient utils.HelmClientInterface, host *kdexv1alpha1.KDexHost, hostReleases map[string]struct{}, log logr.Logger) ([]string, error) {
	owned, err := ownedCompanions(helmClient, host, hostReleases, log)
	if err != nil {
		return nil, err
	}

	declared := make(map[string]struct{})
	if host.Spec.Helm != nil {
		for _, companion := range host.Spec.Helm.CompanionCharts {
			declared[companion.Name] = struct{}{}
		}
	}

	var pruned []string
	for _, releaseName := range owned {
		if _, stillDeclared := declared[releaseName]; stillDeclared {
			continue
		}
		log.V(1).Info("pruning companion chart no longer declared", "release", releaseName, "host", host.Name)
		if err := helmClient.Uninstall(releaseName); err != nil {
			return pruned, fmt.Errorf("failed to prune companion chart %s: %w", releaseName, err)
		}
		pruned = append(pruned, releaseName)
	}

	return pruned, nil
}

// uninstallCompanionCharts removes every companion release belonging to the
// host on teardown: the union of what the spec still declares and what this
// operator recorded installing and still finds labelled as the host's. The
// union matters because the two sets can differ in both directions — a
// companion dropped in the same edit that deleted the host is owned but
// undeclared, while one added moments ago may be declared but not yet
// installed. A KDexHost's own release is never one of them.
func (r *KDexHostReconciler) uninstallCompanionCharts(helmClient utils.HelmClientInterface, host *kdexv1alpha1.KDexHost, hostReleases map[string]struct{}, log logr.Logger) error {
	owned, err := ownedCompanions(helmClient, host, hostReleases, log)
	if err != nil {
		return err
	}

	seen := make(map[string]struct{}, len(owned))
	targets := make([]string, 0, len(owned))
	for _, releaseName := range owned {
		seen[releaseName] = struct{}{}
		targets = append(targets, releaseName)
	}
	if host.Spec.Helm != nil {
		for _, companion := range host.Spec.Helm.CompanionCharts {
			if _, dup := seen[companion.Name]; dup {
				continue
			}
			if _, isHost := hostReleases[companion.Name]; isHost {
				continue
			}
			seen[companion.Name] = struct{}{}
			targets = append(targets, companion.Name)
		}
	}

	// Every target is attempted before returning: one companion blocked by a
	// finalizer must not strand the rest. The failures are joined and returned
	// rather than logged, because the caller removes the host finalizer on a nil
	// error -- reporting success while releases survive is what turns a
	// transient failure into a permanent orphan.
	var errs []error
	for _, releaseName := range targets {
		if err := helmClient.Uninstall(releaseName); err != nil {
			log.Error(err, "failed to uninstall companion release", "name", releaseName)
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

// ownedCompanions returns the companion releases that are the host's to remove:
// labelled as the host's AND recorded as installed by this operator, and not a
// KDexHost's own release. A labelled release missing from the record is
// reported, not removed: it is either a forgery or one installed before the
// record existed, and only an operator can tell which.
func ownedCompanions(helmClient utils.HelmClientInterface, host *kdexv1alpha1.KDexHost, hostReleases map[string]struct{}, log logr.Logger) ([]string, error) {
	labelled, err := helmClient.ListReleases(companionLabels(host.Name))
	if err != nil {
		return nil, err
	}
	recorded := recordedCompanions(host)
	owned := make([]string, 0, len(labelled))
	for _, releaseName := range labelled {
		if _, isHost := hostReleases[releaseName]; isHost {
			log.Info("ignoring a release labelled as a companion that is a KDexHost's own release",
				"release", releaseName, "host", host.Name)
			continue
		}
		if _, ok := recorded[releaseName]; !ok {
			log.Info("ignoring a release labelled as this host's companion that this operator has no record of installing",
				"release", releaseName, "host", host.Name)
			continue
		}
		owned = append(owned, releaseName)
	}
	return owned, nil
}

// recordedCompanions reads AttributeCompanionReleases.
func recordedCompanions(host *kdexv1alpha1.KDexHost) map[string]struct{} {
	recorded := map[string]struct{}{}
	for name := range strings.SplitSeq(host.Status.Attributes[AttributeCompanionReleases], ",") {
		if name != "" {
			recorded[name] = struct{}{}
		}
	}
	return recorded
}

// nextCompanionRecord is AttributeCompanionReleases with added recorded and
// pruned removed.
func nextCompanionRecord(recorded map[string]struct{}, added, pruned []string) string {
	next := maps.Clone(recorded)
	if next == nil {
		next = map[string]struct{}{}
	}
	for _, name := range added {
		next[name] = struct{}{}
	}
	for _, name := range pruned {
		delete(next, name)
	}
	return strings.Join(slices.Sorted(maps.Keys(next)), ",")
}

// checkCompanionNames refuses a companion named after a KDexHost in the
// namespace. A host's own release carries the host's name, so installing the
// companion would upgrade that release with the companion's chart.
func checkCompanionNames(host *kdexv1alpha1.KDexHost, hostReleases map[string]struct{}) error {
	if host.Spec.Helm == nil {
		return nil
	}
	for _, companion := range host.Spec.Helm.CompanionCharts {
		if _, isHost := hostReleases[companion.Name]; isHost {
			return fmt.Errorf("companion chart %q has the name of KDexHost %q, whose own Helm release it would replace", companion.Name, companion.Name)
		}
	}
	return nil
}
