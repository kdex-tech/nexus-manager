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
	"k8s.io/client-go/util/retry"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
	"kdex.dev/crds/configuration"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	// helmAttributePrefix starts every status attribute the async helm
	// operation writes. Reconcile's own status patch overwrites the server's
	// status with its snapshot except for keys with this prefix, which it
	// carries forward from the server instead.
	helmAttributePrefix = "helm.release."

	AttributeHelmReleaseStatus      = helmAttributePrefix + "status"
	AttributeHelmReleaseGeneration  = helmAttributePrefix + "generation"
	AttributeHelmReleaseHash        = helmAttributePrefix + "hash"
	AttributeHelmReleaseError       = helmAttributePrefix + "error"
	AttributeHelmReleaseOwner       = helmAttributePrefix + "owner"
	AttributeHelmReleaseLastAttempt = helmAttributePrefix + "last-attempt"
	AttributeHelmReleaseAttempts    = helmAttributePrefix + "attempts"
	AttributeHelmReleaseSecretsHash = helmAttributePrefix + "secrets-hash"
	// AttributeCompanionReleases records, comma-separated and sorted, every
	// companion release this operator has installed (or is about to: a name
	// is recorded before its install) for the host and not removed since. It
	// lives in status, which only the operator writes, and is the authority
	// for what a prune or a teardown may uninstall. It carries
	// helmAttributePrefix because the async helm operation writes it, so the
	// reconcile's own status patch must preserve it rather than overwrite it.
	// It cannot be rebuilt from observation: status that is lost (a restore
	// without status, an overwrite) is re-seeded from the spec as it is then,
	// so a companion dropped from the spec before that is orphaned, and only
	// logged as unrecorded.
	AttributeCompanionReleases = helmAttributePrefix + "companions"

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

// helmRunInFlight reports whether a helm operation goroutine for the host is
// still running in this process, cancelled or not.
func (r *KDexHostReconciler) helmRunInFlight(key types.NamespacedName) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.helmRuns[key] > 0
}

// errHostTerminating stops a helm operation whose host is gone or being
// deleted.
var errHostTerminating = errors.New("host is terminating")

// helmRunWanted returns why a helm operation should go no further, or nil:
// its context is done (superseded, torn down, or the operator stopping), its
// host is gone or terminating (errHostTerminating), or the host could not be
// read. It reads the host live, since a cache can predate the deletion it is
// there to notice.
//
// The host it read is returned with a nil error.
func (r *KDexHostReconciler) helmRunWanted(ctx context.Context, key types.NamespacedName) (*kdexv1alpha1.KDexHost, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	host := &kdexv1alpha1.KDexHost{}
	if err := r.reader().Get(ctx, key, host); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, errHostTerminating
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("failed to re-read the host: %w", err)
	}
	if !host.DeletionTimestamp.IsZero() {
		return nil, errHostTerminating
	}
	return host, nil
}

// stillWanted is helmRunWanted's verdict alone, for a check between steps.
func (r *KDexHostReconciler) stillWanted(ctx context.Context, key types.NamespacedName) error {
	_, err := r.helmRunWanted(ctx, key)
	return err
}

// haltHelmRun ends a helm operation that a step stopped with err, and reports
// whether it did. A failed read or Helm call is the operation's to report: a
// cancelled operation's status belongs to its successor or the teardown, and a
// terminating host's to the teardown.
func (r *KDexHostReconciler) haltHelmRun(ctx context.Context, namespace, name, hash string, err error, log logr.Logger) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, errHostTerminating):
		log.V(2).Info("host is terminating; the helm operation stops here", "namespace", namespace, "name", name)
	case ctx.Err() != nil:
		log.V(2).Info("helm operation cancelled; it stops here", "namespace", namespace, "name", name)
	default:
		r.updateHelmStatus(ctx, namespace, name, hash, err, nil, log)
	}
	return true
}

// helmRetryBackoffCap bounds how long a host whose helm runs keep failing
// for one spec waits between them.
const helmRetryBackoffCap = 15 * time.Minute

// helmRetryBackoff is how long to wait, from the last attempt at a run, before
// the attempt after the attempts-th: base, doubled for each attempt after the
// first, at most helmRetryBackoffCap. Each run holds the fleet's one
// render slot for as long as its chart fetch takes, so a tenant chart that
// fails slowly, retried at a flat interval, would hold it most of the time.
//
// Fetching outside the slot would free it outright, but Helm pulls a chart
// into memory, and the slot exists to bound memory: unbounded concurrent
// fetches of tenant-sized charts are the stampede it prevents.
func helmRetryBackoff(base time.Duration, attempts int) time.Duration {
	if base <= 0 {
		return 0
	}
	d := base
	for i := 1; i < attempts; i++ {
		if d >= helmRetryBackoffCap/2 {
			return helmRetryBackoffCap
		}
		d *= 2
	}
	return min(d, helmRetryBackoffCap)
}

// helmAttempts reads AttributeHelmReleaseAttempts.
func helmAttempts(host *kdexv1alpha1.KDexHost) int {
	n, _ := strconv.Atoi(host.Status.Attributes[AttributeHelmReleaseAttempts])
	return n
}

// takeHelmAttributes makes the helm.release. keys of snapshot, a reconcile's
// copy of a host's attributes, those of last, the host as last read -- the
// read a status patch is built against -- and leaves every other key alone.
// The patch then never writes a helm.release. key. The async helm operation
// owns those keys and writes them while the reconcile runs: the reconcile's
// copy may hold values the operation has since replaced, or keys it has since
// deleted -- a success clears last-attempt and attempts, which the run's start
// set -- and a patch built from that copy would put them back.
func takeHelmAttributes(snapshot, last map[string]string) {
	for k := range snapshot {
		if _, ok := last[k]; !ok && strings.HasPrefix(k, helmAttributePrefix) {
			delete(snapshot, k)
		}
	}
	for k, v := range last {
		if strings.HasPrefix(k, helmAttributePrefix) {
			snapshot[k] = v
		}
	}
}

// reader is APIReader, or Client when none is set.
func (r *KDexHostReconciler) reader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

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

// registerHelmRun makes the helm operation for hash the host's active one,
// cancelling any other, and counts it as running. done undoes both and
// releases the operation's context, and must be called when the goroutine
// running the operation returns. ok is false, and nothing is registered, when
// an operation for the same hash is already active.
//
// The two are registered as a pair, before the goroutine starts, because they
// end differently: a superseding operation or a teardown takes the active
// entry away while the goroutine may still be inside a Helm call, which takes
// no context, but the run count lasts until the goroutine returns. Teardown
// waits on the count.
func (r *KDexHostReconciler) registerHelmRun(key types.NamespacedName, cancel context.CancelFunc, hash string) (done func(), ok bool) {
	r.mu.Lock()
	if r.activeHelmOperations == nil {
		r.activeHelmOperations = make(map[types.NamespacedName]helmOperation)
	}
	if old, active := r.activeHelmOperations[key]; active {
		if old.hash == hash {
			r.mu.Unlock()
			return nil, false
		}
		// Cancel old generation if it's still running
		// We do this outside the lock to avoid any potential deadlock if cancel()
		// synchronously triggers something that needs the lock.
		r.mu.Unlock()
		old.cancel()

		r.mu.Lock()
	}
	r.helmRunSeq++
	id := r.helmRunSeq
	r.activeHelmOperations[key] = helmOperation{cancel: cancel, hash: hash, id: id}
	if r.helmRuns == nil {
		r.helmRuns = make(map[types.NamespacedName]int)
	}
	r.helmRuns[key]++
	r.mu.Unlock()

	return func() {
		r.mu.Lock()
		if op, exists := r.activeHelmOperations[key]; exists && op.id == id {
			delete(r.activeHelmOperations, key)
		}
		if r.helmRuns[key]--; r.helmRuns[key] <= 0 {
			delete(r.helmRuns, key)
		}
		r.mu.Unlock()
		cancel()
	}, true
}

// startHelmRun registers the helm operation for hash and runs it in a new
// goroutine, returning false when one for the same hash is already active.
func (r *KDexHostReconciler) startHelmRun(
	ctx context.Context,
	cancel context.CancelFunc,
	namespace, name string,
	hash string,
	secrets kdexv1alpha1.Secrets,
	log logr.Logger,
) bool {
	done, ok := r.registerHelmRun(types.NamespacedName{Namespace: namespace, Name: name}, cancel, hash)
	if !ok {
		return false
	}
	go func() {
		defer done()
		r.runAsyncHelmReconcile(ctx, namespace, name, hash, secrets, log)
	}()
	return true
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

	// Seed a host's record the first time it is seen without one: a new host,
	// one whose companions were installed before the record existed (v0.7.7
	// and earlier), or one whose status was lost. For the last two the hash
	// need not change, so no helm run would ever write a record, and a
	// companion dropped from the spec later would be found unrecorded and
	// never pruned. Seeding happens before the hash check can skip the run.
	// For a new host it records the declared names before their install, as
	// the run itself would. Only an ABSENT record is seeded: an empty one is
	// the operator's word that it installed nothing (#46).
	if _, ok := companionRecord(latestHost); !ok {
		seeded, err := r.seedCompanionRecord(ctx, latestHost)
		if err != nil {
			return controllerutil.OperationResultNone, err
		}
		latestHost = seeded
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
	// A retry is another attempt at the same run: the same spec, with the same
	// resolved Secrets. A changed Secret -- the fix for a registry that
	// refused the last attempt's credentials -- starts a new run, with no
	// backoff to wait out.
	secretsHash := utils.HashSecrets(secrets)
	sameRun := recordedHash == currentHash && latestHost.Status.Attributes[AttributeHelmReleaseSecretsHash] == secretsHash

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

	if (status == HelmStatusFailed || status == HelmStatusInProgress) && sameRun {
		// The last attempt at this same run failed, or never finished: a
		// zombie is what a run that killed the operator -- a chart that ran
		// it out of memory -- leaves behind, recording no failure. Either may
		// be transient (a network policy that hadn't landed yet, a registry
		// blip, control-plane pressure during --wait), so we retry instead of
		// locking the release in a permanent Degraded state. A backoff from
		// r.RequeueDelay, growing with each attempt, keeps us from hammering
		// -- or crash-looping -- when the cause is chronic.
		if lastAttemptStr := latestHost.Status.Attributes[AttributeHelmReleaseLastAttempt]; lastAttemptStr != "" {
			if lastAttempt, parseErr := time.Parse(time.RFC3339, lastAttemptStr); parseErr == nil {
				elapsed := time.Since(lastAttempt)
				interval := helmRetryBackoff(r.RequeueDelay, helmAttempts(latestHost))
				if elapsed < interval {
					log.V(2).Info("reconcileHelmReleases#3b",
						"host", host.Name, "namespace", host.Namespace,
						"msg", "in backoff after failure",
						"elapsed", elapsed.String(),
						"interval", interval.String())
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
	// Count the attempt now, as it starts, and stamp when: an attempt that
	// kills the operator records nothing when it ends, and must still back
	// off. A success clears both; a failure counts nothing more, and
	// re-stamps the time from its end.
	attempts := 1
	if sameRun {
		attempts = helmAttempts(latestHost) + 1
	}
	host.Status.Attributes[AttributeHelmReleaseAttempts] = strconv.Itoa(attempts)
	host.Status.Attributes[AttributeHelmReleaseLastAttempt] = time.Now().UTC().Format(time.RFC3339)
	host.Status.Attributes[AttributeHelmReleaseSecretsHash] = secretsHash

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
	if _, ok := companionRecord(latestHost); ok {
		host.Status.Attributes[AttributeCompanionReleases] = latestHost.Status.Attributes[AttributeCompanionReleases]
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
	if !r.startHelmRun(asyncCtx, cancel, host.Namespace, host.Name, currentHash, secrets, log) {
		cancel()
		log.V(2).Info("reconcileHelmReleases#5", "host", host.Name, "namespace", host.Namespace, "msg", "already active")
		return controllerutil.OperationResultNone, nil
	}

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
	key := types.NamespacedName{Namespace: namespace, Name: name}

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
		return
	}
	defer release()

	// Read the host only now that the slot is held. The wait above can last
	// minutes on a serialized fleet, and nothing read before it says what is
	// still true: the host may have been deleted and fully finalized in the
	// meantime. Proceeding on a stale copy would re-create the Helm client
	// that deletion just removed and re-install the release plus every
	// companion the stale spec declares -- releases that then run with no CR
	// left in the cluster referencing them, invisible to the prune because the
	// stale spec still declares them. Read live: a cached copy can predate the
	// deletion it exists to notice.
	host, err := r.helmRunWanted(ctx, key)
	if r.haltHelmRun(ctx, namespace, name, hash, err, log) {
		return
	}
	// Asked once each chart is in hand, before Helm acts on it: a fetch can
	// outlast teardown's wait for this run.
	proceed := func() error { return r.stillWanted(ctx, key) }

	c, err := r.getOrCreateHelmClient(
		host.Name,
		host.Namespace,
		secrets,
		log.WithName("helm"),
		utils.HelmAsOperator,
	)
	if err != nil {
		r.updateHelmStatus(ctx, namespace, name, hash, err, nil, log)
		return
	}

	// 0. Refuse companion sources outright, before anything is installed: a
	// host whose companions cannot all be fetched safely installs none of its
	// charts, the host-manager chart included.
	if err := checkCompanionSources(host); err != nil {
		r.updateHelmStatus(ctx, namespace, name, hash, err, nil, log)
		return
	}

	// 1. Reconcile kdex-host-manager chart
	if r.haltHelmRun(ctx, namespace, name, hash, r.reconcileHostManagerChart(c, host, proceed), log) {
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
	// name whose install then fails has no release of its own, and the next
	// run that finds it neither declared nor labelled forgets it.
	declared := declaredCompanionNames(host)
	if err := r.recordCompanions(ctx, host, declared); err != nil {
		r.updateHelmStatus(ctx, namespace, name, hash, err, nil, log)
		return
	}
	if host.Spec.Helm != nil {
		for _, companion := range host.Spec.Helm.CompanionCharts {
			// Teardown waits for this goroutine, so once the host is
			// terminating install nothing more: anything installed now only
			// delays the teardown that has to remove it.
			if r.haltHelmRun(ctx, namespace, name, hash, r.stillWanted(ctx, key), log) {
				return
			}
			if r.haltHelmRun(ctx, namespace, name, hash, r.reconcileCompanionChart(c, host, companion, proceed), log) {
				return
			}
		}
	}

	// Every chart load is done, so give the render slot back before the prune.
	// The prune loads no chart -- it lists releases and uninstalls -- but each
	// uninstall waits on foreground deletion for up to utils.HelmCallTimeout.
	// Holding a MEMORY bound across that would let one companion blocked by a
	// finalizer stall every other host's render, which is the whole failure
	// this slot is supposed to prevent rather than cause.
	release()

	// 3. Prune companions we own that are no longer declared. Deliberately
	// outside the guard above: dropping the entire spec.helm block orphans
	// every companion, which is exactly when pruning matters most. Runs after
	// the installs so a rename (x -> y) never leaves the host with neither.
	// What it removed leaves the record, whether or not it then failed, and so
	// does a recorded name that is neither declared nor labelled as the host's:
	// no release of the host's carries it, so it has nothing left to authorize
	// but the uninstall of a release forged under its name. That needs the
	// labelled set, so a prune that could not list releases forgets nothing.
	//
	// A terminating host prunes nothing, and stops pruning the moment it is
	// seen terminating: teardown uninstalls every recorded release anyway,
	// and waits for this goroutine to do it. What a stopped prune removed
	// stays recorded, harmlessly: with no release, nothing carries its label.
	if r.haltHelmRun(ctx, namespace, name, hash, r.stillWanted(ctx, key), log) {
		return
	}
	pruned, labelled, err := r.pruneCompanionCharts(c, host, hostReleases, proceed, log)
	if errors.Is(err, errHostTerminating) || ctx.Err() != nil {
		r.haltHelmRun(ctx, namespace, name, hash, err, log)
		return
	}
	change := companionRecordChange{added: declared, pruned: pruned}
	if labelled != nil {
		change.stale = staleCompanions(recordedCompanions(host), declaredCompanions(host), labelled)
	}
	r.updateHelmStatus(ctx, namespace, name, hash, err, &change, log)
}

// recordCompanions adds declared to AttributeCompanionReleases as the server
// holds it, writing status only when that changes it, and leaves the record as
// it then stands on host.
func (r *KDexHostReconciler) recordCompanions(ctx context.Context, host *kdexv1alpha1.KDexHost, declared []string) error {
	change := companionRecordChange{added: declared}
	latest, err := r.patchHostStatus(ctx, client.ObjectKeyFromObject(host), func(latest *kdexv1alpha1.KDexHost) bool {
		next := nextCompanionRecord(recordedCompanions(latest), change, nil)
		if next == latest.Status.Attributes[AttributeCompanionReleases] {
			return false
		}
		latest.Status.Attributes[AttributeCompanionReleases] = next
		return true
	})
	if err != nil {
		return fmt.Errorf("failed to record the companion releases before installing them: %w", err)
	}
	if rec, ok := companionRecord(latest); ok {
		if host.Status.Attributes == nil {
			host.Status.Attributes = make(map[string]string)
		}
		host.Status.Attributes[AttributeCompanionReleases] = formatCompanionRecord(rec)
	}
	return nil
}

// seedCompanionRecord writes the record of a host that has none from what it
// declares (see specCompanionRecord), and returns the host as it then stands.
func (r *KDexHostReconciler) seedCompanionRecord(ctx context.Context, host *kdexv1alpha1.KDexHost) (*kdexv1alpha1.KDexHost, error) {
	hostReleases, err := r.kdexHostNames(ctx, host.Namespace)
	if err != nil {
		return nil, fmt.Errorf("failed to seed the companion record: %w", err)
	}
	latest, err := r.patchHostStatus(ctx, client.ObjectKeyFromObject(host), func(latest *kdexv1alpha1.KDexHost) bool {
		if _, ok := companionRecord(latest); ok {
			return false
		}
		latest.Status.Attributes[AttributeCompanionReleases] = formatCompanionRecord(specCompanionRecord(latest, hostReleases))
		return true
	})
	if err != nil {
		return nil, fmt.Errorf("failed to seed the companion record: %w", err)
	}
	return latest, nil
}

// patchHostStatus applies mutate to the host as the server holds it now, and
// writes the result only if nothing else wrote the host since that read: on a
// conflict it reads the host again and re-applies mutate. A plain merge patch
// carries no resourceVersion, so two writers of the companion record would
// each win in turn, the last dropping whatever the other had just recorded.
// mutate reports whether there is anything to write; it may assume Attributes
// is not nil. The host returned is as last read or written.
func (r *KDexHostReconciler) patchHostStatus(ctx context.Context, key client.ObjectKey, mutate func(*kdexv1alpha1.KDexHost) bool) (*kdexv1alpha1.KDexHost, error) {
	var host *kdexv1alpha1.KDexHost
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		host = &kdexv1alpha1.KDexHost{}
		if err := r.reader().Get(ctx, key, host); err != nil {
			return err
		}
		patch := client.MergeFromWithOptions(host.DeepCopy(), client.MergeFromWithOptimisticLock{})
		if host.Status.Attributes == nil {
			host.Status.Attributes = make(map[string]string)
		}
		if !mutate(host) {
			return nil
		}
		return r.Status().Patch(ctx, host, patch)
	})
	return host, err
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
// recordChange is applied to AttributeCompanionReleases as the server holds it
// when the outcome is written; nil leaves the record as it is.
func (r *KDexHostReconciler) updateHelmStatus(ctx context.Context, namespace, name string, hash string, err error, recordChange *companionRecordChange, log logr.Logger) {
	log.V(2).Info("updateHelmStatus", "hash", hash, "err", err)

	_, patchErr := r.patchHostStatus(ctx, client.ObjectKey{Namespace: namespace, Name: name}, func(host *kdexv1alpha1.KDexHost) bool {
		setHelmOutcome(host, hash, r.ControllerID, err)
		if recordChange != nil {
			// Written even when empty: the reconcile's status patch preserves a
			// helm.release. key the server holds, but restores a deleted one
			// from its own stale snapshot.
			host.Status.Attributes[AttributeCompanionReleases] = nextCompanionRecord(
				recordedCompanions(host), *recordChange, declaredCompanions(host))
		}
		return true
	})
	if patchErr != nil {
		if !apierrors.IsNotFound(patchErr) {
			log.Error(patchErr, "failed to patch status")
		}
		log.V(2).Info("updateHelmStatus", "patchErr", patchErr)
	}
}

// setHelmOutcome writes a helm reconcile's outcome to the host's status.
func setHelmOutcome(host *kdexv1alpha1.KDexHost, hash, controllerID string, err error) {
	if err != nil {
		host.Status.Attributes[AttributeHelmReleaseStatus] = HelmStatusFailed
		host.Status.Attributes[AttributeHelmReleaseError] = err.Error()
		// Timestamp the failure so the next reconcile can compute backoff
		// and decide whether to retry (see reconcileHelmReleases#3b/#3c). The
		// attempt was counted when it started.
		host.Status.Attributes[AttributeHelmReleaseLastAttempt] = time.Now().UTC().Format(time.RFC3339)
		reason := kdexv1alpha1.ConditionReasonReconcileError
		var refused *companionRefusedError
		if errors.As(err, &refused) {
			reason = ConditionReasonCompanionChartRefused
		}
		kdexv1alpha1.SetConditions(
			&host.Status.Conditions,
			kdexv1alpha1.ConditionStatuses{
				Progressing: metav1.ConditionFalse,
				Degraded:    metav1.ConditionTrue,
			},
			reason,
			err.Error(),
		)
	} else {
		host.Status.Attributes[AttributeHelmReleaseStatus] = HelmStatusCompleted
		delete(host.Status.Attributes, AttributeHelmReleaseError)
		delete(host.Status.Attributes, AttributeHelmReleaseLastAttempt)
		delete(host.Status.Attributes, AttributeHelmReleaseAttempts)
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
	host.Status.Attributes[AttributeHelmReleaseOwner] = controllerID
}

// reconcileHostManagerChart installs or upgrades the host's own release,
// asking proceed once its chart is in hand.
func (r *KDexHostReconciler) reconcileHostManagerChart(helmClient utils.HelmClientInterface, host *kdexv1alpha1.KDexHost, proceed func() error) error {
	spec, err := r.buildHostManagerChartSpec(host)
	if err != nil {
		return err
	}
	spec.Proceed = proceed

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

// reconcileCompanionChart installs or upgrades one companion release, asking
// proceed once its chart is in hand.
func (r *KDexHostReconciler) reconcileCompanionChart(helmClient utils.HelmClientInterface, host *kdexv1alpha1.KDexHost, companion kdexv1alpha1.CompanionChart, proceed func() error) error {
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
		Proceed:     proceed,
	}

	return helmClient.InstallOrUpgrade(spec)
}

// pruneCompanionCharts uninstalls companion releases this operator installed
// for the host that the host no longer declares, and returns the ones it
// removed and every release labelled as the host's (nil when they could not be
// listed). Converging from desired state alone cannot do this: an entry removed
// from the list leaves no trace in the spec, so the release must be found some
// other way. Neither way alone is trusted (#46): the owner label is
// rediscovered from Helm's records, which anyone who can write Secrets in the
// namespace can label, and the operator's own record in status can name a
// release since taken over. A release is pruned only when both name it, and
// never when it is a KDexHost's own release.
//
// proceed, when not nil, is asked before each uninstall whether to go on; the
// prune stops with its error.
func (r *KDexHostReconciler) pruneCompanionCharts(helmClient utils.HelmClientInterface, host *kdexv1alpha1.KDexHost, hostReleases map[string]struct{}, proceed func() error, log logr.Logger) ([]string, map[string]struct{}, error) {
	owned, labelled, err := ownedCompanions(helmClient, host, recordedCompanions(host), hostReleases, log)
	if err != nil {
		return nil, nil, err
	}

	declared := declaredCompanions(host)
	var pruned []string
	for _, releaseName := range owned {
		if _, stillDeclared := declared[releaseName]; stillDeclared {
			continue
		}
		if proceed != nil {
			if err := proceed(); err != nil {
				return pruned, labelled, err
			}
		}
		log.V(1).Info("pruning companion chart no longer declared", "release", releaseName, "host", host.Name)
		if err := helmClient.Uninstall(releaseName); err != nil {
			return pruned, labelled, fmt.Errorf("failed to prune companion chart %s: %w", releaseName, err)
		}
		pruned = append(pruned, releaseName)
	}

	return pruned, labelled, nil
}

// uninstallCompanionCharts removes every companion release belonging to the
// host on teardown: what this operator recorded and still finds labelled as the
// host's. A KDexHost's own release is never one of them. The record is written
// before every install, and the caller waits out any helm operation this
// process still runs, so every companion the operator installed is in it.
//
// What that excludes (#46): a name only the spec declares, recorded nowhere --
// a release forged under it with the host's label is left alone, as is another
// host's release of the same name relabelled as this host's. What it does not:
// a declared name whose install FAILED stays recorded (a name still declared is
// never stale), so a release forged under it with the host's label IS
// uninstalled. That is the exposure already accepted for a forged revision of a
// release the operator did install: the spec's author could have let the
// install succeed and then forged one.
func (r *KDexHostReconciler) uninstallCompanionCharts(helmClient utils.HelmClientInterface, host *kdexv1alpha1.KDexHost, hostReleases map[string]struct{}, log logr.Logger) error {
	recorded, ok := companionRecord(host)
	if !ok {
		// Deleted before any reconcile could seed the record (installed before
		// it existed, or status lost): the spec is the only account left.
		recorded = specCompanionRecord(host, hostReleases)
	}
	owned, _, err := ownedCompanions(helmClient, host, recorded, hostReleases, log)
	if err != nil {
		return err
	}

	// Every target is attempted before returning: one companion blocked by a
	// finalizer must not strand the rest. The failures are joined and returned
	// rather than logged, because the caller removes the host finalizer on a nil
	// error -- reporting success while releases survive is what turns a
	// transient failure into a permanent orphan.
	var errs []error
	for _, releaseName := range owned {
		if err := helmClient.Uninstall(releaseName); err != nil {
			log.Error(err, "failed to uninstall companion release", "name", releaseName)
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

// ownedCompanions returns the companion releases that are the host's to remove
// -- labelled as the host's AND in recorded, and not a KDexHost's own release
// -- and every release labelled as the host's. A labelled release missing from
// the record is reported, not removed: it is either a forgery or one installed
// before the record existed, and only an operator can tell which.
func ownedCompanions(helmClient utils.HelmClientInterface, host *kdexv1alpha1.KDexHost, recorded, hostReleases map[string]struct{}, log logr.Logger) ([]string, map[string]struct{}, error) {
	listed, err := helmClient.ListReleases(companionLabels(host.Name))
	if err != nil {
		return nil, nil, err
	}
	labelled := make(map[string]struct{}, len(listed))
	owned := make([]string, 0, len(listed))
	for _, releaseName := range listed {
		labelled[releaseName] = struct{}{}
		if _, isHost := hostReleases[releaseName]; isHost {
			log.Info("ignoring a release labelled as a companion that is a KDexHost's own release",
				"release", releaseName, "host", host.Name)
			continue
		}
		if _, ok := recorded[releaseName]; !ok {
			log.Info("ignoring a release labelled as this host's companion that this operator has no record of installing; "+
				"uninstall it by hand (helm uninstall <release> -n <namespace>) or, if it is the host's, add it to "+
				"status.attributes[\""+AttributeCompanionReleases+"\"]",
				"release", releaseName, "namespace", host.Namespace, "host", host.Name)
			continue
		}
		owned = append(owned, releaseName)
	}
	return owned, labelled, nil
}

// companionRecord reads AttributeCompanionReleases, and reports whether the
// host has one at all. Absent and empty differ: absent is a host never seeded,
// empty one the operator recorded installing nothing for.
func companionRecord(host *kdexv1alpha1.KDexHost) (map[string]struct{}, bool) {
	raw, ok := host.Status.Attributes[AttributeCompanionReleases]
	recorded := map[string]struct{}{}
	for name := range strings.SplitSeq(raw, ",") {
		if name != "" {
			recorded[name] = struct{}{}
		}
	}
	return recorded, ok
}

// recordedCompanions is companionRecord's set, empty when there is none.
func recordedCompanions(host *kdexv1alpha1.KDexHost) map[string]struct{} {
	recorded, _ := companionRecord(host)
	return recorded
}

// declaredCompanionNames lists the companions the host declares, in spec order.
func declaredCompanionNames(host *kdexv1alpha1.KDexHost) []string {
	if host.Spec.Helm == nil {
		return nil
	}
	names := make([]string, 0, len(host.Spec.Helm.CompanionCharts))
	for _, companion := range host.Spec.Helm.CompanionCharts {
		names = append(names, companion.Name)
	}
	return names
}

// declaredCompanions is declaredCompanionNames as a set.
func declaredCompanions(host *kdexv1alpha1.KDexHost) map[string]struct{} {
	declared := map[string]struct{}{}
	for _, name := range declaredCompanionNames(host) {
		declared[name] = struct{}{}
	}
	return declared
}

// specCompanionRecord stands in for the record of a host that has none: every
// companion it declares, except a KDexHost's name, which is never a
// companion's. It is exact for a host whose spec has not dropped a companion
// since the record was last written, or would have been; a companion dropped
// before then is not in it.
func specCompanionRecord(host *kdexv1alpha1.KDexHost, hostReleases map[string]struct{}) map[string]struct{} {
	record := declaredCompanions(host)
	for name := range record {
		if _, isHost := hostReleases[name]; isHost {
			delete(record, name)
		}
	}
	return record
}

// formatCompanionRecord writes a record as AttributeCompanionReleases holds it.
func formatCompanionRecord(record map[string]struct{}) string {
	return strings.Join(slices.Sorted(maps.Keys(record)), ",")
}

// companionRecordChange is what one helm operation did to the companion
// record. It is applied to the record the server holds when it is written, not
// to the one the operation read at its start: two operations for one host can
// overlap, and writing back a whole record computed from a stale read drops
// whatever the other recorded in the meantime.
type companionRecordChange struct {
	// added were declared, and so recorded before their install.
	added []string
	// pruned were uninstalled.
	pruned []string
	// stale were recorded but neither declared nor labelled as the host's
	// when the operation looked: no release of the host's carries the name.
	stale []string
}

// nextCompanionRecord is current with change applied. A stale name declared
// stays, since the spec that now declares it is about to install it.
func nextCompanionRecord(current map[string]struct{}, change companionRecordChange, declared map[string]struct{}) string {
	next := maps.Clone(current)
	if next == nil {
		next = map[string]struct{}{}
	}
	for _, name := range change.added {
		next[name] = struct{}{}
	}
	for _, name := range change.pruned {
		delete(next, name)
	}
	for _, name := range change.stale {
		if _, ok := declared[name]; !ok {
			delete(next, name)
		}
	}
	return formatCompanionRecord(next)
}

// staleCompanions are the recorded names neither declared nor labelled.
func staleCompanions(recorded, declared, labelled map[string]struct{}) []string {
	var stale []string
	for name := range recorded {
		_, isDeclared := declared[name]
		_, isLabelled := labelled[name]
		if !isDeclared && !isLabelled {
			stale = append(stale, name)
		}
	}
	slices.Sort(stale)
	return stale
}

// ConditionReasonCompanionChartRefused is the reason on a host whose companion
// chart was refused before anything was installed. It lives here, not in
// kdex-crds, which declares no reason for it.
const ConditionReasonCompanionChartRefused kdexv1alpha1.ConditionReason = "CompanionChartRefused"

// companionRefusedError is a companion chart the operator will not fetch.
type companionRefusedError struct{ msg string }

func (e *companionRefusedError) Error() string { return e.msg }

// checkCompanionSources refuses every companion that is not an OCI chart
// reference. A companion is tenant input, and only a registry fetch is bounded
// (time, requests, bytes): a local path would read the operator's filesystem,
// and an http(s) or "repo/chart" reference would need a classic repository
// index, which is unbounded and not supported. The Repository field names such
// an index, so it is refused too.
func checkCompanionSources(host *kdexv1alpha1.KDexHost) error {
	if host.Spec.Helm == nil {
		return nil
	}
	for _, companion := range host.Spec.Helm.CompanionCharts {
		if !strings.HasPrefix(companion.Chart, "oci://") {
			return &companionRefusedError{fmt.Sprintf("companion chart %q: chart %q must be an oci:// reference", companion.Name, companion.Chart)}
		}
		if companion.Repository != "" {
			return &companionRefusedError{fmt.Sprintf("companion chart %q: repository %q is not supported; companions are fetched from oci:// references only", companion.Name, companion.Repository)}
		}
	}
	return nil
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
