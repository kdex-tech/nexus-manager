# Release B: privilege-redesign cleanup

**Status: done** in nexus-manager v0.9.0 (kdex-tech/nexus-manager#65). Kept as
the record of what was removed and why. Upgrading from a release before v0.8.0
must pass through v0.8.x first: only v0.8.x migrates the legacy RBAC.

Release A (the privilege redesign) migrates every host from the cluster-scoped
host-manager RBAC (`ClusterRole/<host>`, `ClusterRoleBinding/<host>-<namespace>`)
to the namespaced RBAC the host-manager chart now renders. It kept all of
nexus's pre-existing grants and carried migration code. Release B deleted both,
once infra confirmed no legacy object remained on any cluster (2026-11-12):

```sh
kubectl get clusterrolebinding,clusterrole -l app.kubernetes.io/name=host-manager
# must print nothing, on every cluster
```

Before tagging, the fleet was also checked for what the deleted code would
have handled: no host-manager release's latest revision lists a ClusterRole or
ClusterRoleBinding, no RoleBinding carries the `kdex.dev/installer` finalizer,
and no host carries `LegacyRBACPresent`.

The sections below are the removal list as planned (the code markers it
refers to are gone with the code). Beyond it, release B also dropped the
read-only `kdexroles`, `kdexrolebindings` and `kdexinternalpackagereferences`
grants (nothing in nexus reads them) and added `bind` on the cluster-reader
ClusterRole.

## Code

### `internal/controller/legacy_rbac.go` (delete the file)

- Constants: `ConditionLegacyRBACPresent`, `reasonLegacyRBACAwaitingUpgrade`,
  `reasonLegacyRBACCleanupFailed`, `reasonLegacyRBACReadFailed`,
  `reasonLegacyRBACProofReadFailed`, `helmReleaseNameAnnotation`,
  `helmReleaseNamespaceAnnotation`.
- Functions: `helmReleasedBy`, `legacyBindingName`, `isLegacyRole`,
  `isLegacyBinding`, `(*KDexHostReconciler).legacyRBAC`, `legacyRBACNames`,
  `(*KDexHostReconciler).deleteLegacyRBAC`, `setLegacyRBACCondition`,
  `(*KDexHostReconciler).cleanupLegacyRBAC`, `requeueForLegacyRBAC`,
  `(*KDexHostReconciler).hostManagerUpgraded`, `deploymentRolledOut`.
- `kindClusterRole` (`installer.go`) stays: the installer RoleBinding's roleRef
  uses it.

### `internal/controller/cluster_reader.go`

- `(*KDexHostReconciler).clusterReaderGranted` (only the upgrade proof reads it).
- Keep `deploymentServiceAccount` and `hostManagerServiceAccount`: the
  post-install grant uses them.

### `internal/controller/installer.go`

- `legacyInstallerFinalizer` and the `controllerutil.RemoveFinalizer` call in
  `ensureInstallerObjects` that strips it (it was only ever set by pre-release
  builds of release A); edit the `ensureInstaller` doc comment's
  "legacyInstallerFinalizer removed" out of what an existing object is healed
  of.
- `teardownHost`: the `legacyRBAC` / `deleteLegacyRBAC` block before the
  uninstall; edit its doc comment's "any legacy cluster-scoped RBAC, which no
  namespace deletion removes" out of the steps it lists.
- `uninstallHostReleases`: the `DropClusterRBACFromRecord` call and its log line;
  uninstall directly.

### `internal/controller/kdexhost_controller.go`

- `Reconcile`: the `cleanupLegacyRBAC` call, its error log and the deferred
  `requeueForLegacyRBAC` (the block under the marker after
  `healClusterReaderSubject`).

### `internal/utils/helm.go`

- From `HelmClientInterface`: `ReleaseStatus`, `DropClusterRBACFromRecord`.
- `(*HelmClient).latestRevision`, `(*HelmClient).ReleaseStatus`,
  `(*HelmClient).DropClusterRBACFromRecord`, `clusterRBACOrList`; then the
  imports only they use: `k8s.io/apimachinery/pkg/api/meta`,
  `.../apis/meta/v1/unstructured`, `rbacv1`, `releaseutil`, `rspb`, `sort`,
  `maps` and `sigs.k8s.io/yaml` (`metav1`, `driver`, `errors`, `slices` and
  `strings` stay: other code in the file uses them). `sigs.k8s.io/yaml` stays
  in `go.mod`: tests use it.

## RBAC (`internal/controller/rbac.go`, then `make manifests` and regenerate the chart's `chart/templates/rbac/role.yaml`)

Every line commented `// release B: drop ...` above it (spec D8):

- Drop whole: `apps/replicasets`; `batch/jobs`, `batch/cronjobs`;
  core `configmaps`, `persistentvolumeclaims`, `pods`, `services`;
  `discovery.k8s.io/endpointslices`; `gateway.networking.k8s.io/httproutes`,
  `gateways`; `kpack.io/images`, `images/finalizers`, `images/status`;
  `networking.k8s.io/ingresses`; `serving.knative.dev/services`;
  the broad `rbac.authorization.k8s.io/clusterroles` and `clusterrolebindings`
  lines (legacy cleanup only).
- Drop write verbs only: `apps/deployments` (keep get/list/watch); core
  `secrets` (keep get/list/watch); core `serviceaccounts` (drop `patch`; the
  installer bootstrap keeps get/list/watch/create/update/delete); `kdexhosts`
  (drop create/delete; keep update/patch for the finalizer);
  `kdexinternalpackagereferences`; every kdex content kind nexus only reads and
  writes the status of (apps, faasadaptors, functions, page*, scriptlibraries,
  themes, translations, utilitypages, hostextensions, and their cluster
  variants); `kdexroles` and `kdexrolebindings` (verify nothing reads them before
  dropping get/list/watch too).
- Drop dead finalizer grants: every `kdex.dev/*/finalizers` line except
  `kdexhosts/finalizers` (nexus sets no finalizer elsewhere; `blockOwnerDeletion`
  on the internal kinds' ownerRefs needs only the owner's).
- Drop dead status grants: `kdexfaasadaptors/status`,
  `kdexclusterfaasadaptors/status`, `kdexfunctions/status`, `kdexpages/status`,
  `kdexinternalhosts/status`, `kdexinternalpackagereferences/status` (nexus
  writes none of them).
- Change, do not drop: the `resourceNames=kcnas-operator-host-manager-cluster-reader`
  ClusterRoleBinding line gains `list;watch` -- the host reconciler watches that
  one binding through a `metadata.name` field-selected informer, which needs them
  once the broad line is gone.

Keep: get/list/watch on what the reconcilers read; the kdex.dev writes nexus
makes (`kdexhosts` finalizer/status, `kdexinternalhosts` itself but not its
status, `kdexinternaltranslations` and `kdexinternalutilitypages` and their
status, content-kind status); the D2 grants (impersonate `kdex-installer`, rolebindings,
`bind` on `kcnas-operator-installer`, the cluster-reader binding, namespaces get).

## Tests

- Delete `internal/controller/legacy_rbac_test.go`,
  `internal/controller/legacy_record_envtest_test.go`,
  `internal/utils/helm_legacy_record_test.go`.
- `internal/controller/mocks_test.go`: `recordDoc` with its
  `installerDeletable` and `clusterRBACOrList`, `mockInstaller`, the mock's
  `records`, `statuses`, `rewrites`, `record`, `recordOf`, `setStatus`,
  `Rewrites`, `ReleaseStatus`, `DropClusterRBACFromRecord`, and the record
  check in `Uninstall`; `addRelease` if nothing else uses it.
- The helpers permanent tests share (`createObjects`, `assertExists`,
  `reconcileTeardown`, `assertFinalized`) live in
  `internal/controller/testhelpers_test.go`, so deleting the legacy test files
  above compiles; keep them.
- The `ReleaseStatus` / `DropClusterRBACFromRecord` stubs on
  `stuckUninstallHelmClient` (`kdexhost_deletion_blocked_test.go`),
  `stubHelmClient` (`helm_client_cache_test.go`),
  `concurrencyTrackingHelmClient` and `pruneTrackingHelmClient`
  (`kdexhost_helm_concurrency_test.go`).
- `installer_test.go` `TestEnsureInstaller_HealsAnExistingBinding`: drop the
  `kdex.dev/installer` finalizer from its fixture.
- `helm_impersonation_envtest_test.go` probes the installer with
  `ReleaseStatus("absent")`; probe with `Uninstall("absent")` instead (it reads
  the release history as the installer: Forbidden without the grant, nil with
  it).

## Spec and docs

- `kdex-host-manager/docs/superpowers/specs/2026-11-11-privilege-redesign-design.md`,
  Migration step 4: mark done.
- The `LegacyRBACPresent` condition disappears from host status; drop it from
  any user docs that mention it.
