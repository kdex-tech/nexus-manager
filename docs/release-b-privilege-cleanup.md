# Release B: privilege-redesign cleanup

Release A (the privilege redesign) migrates every host from the cluster-scoped
host-manager RBAC (`ClusterRole/<host>`, `ClusterRoleBinding/<host>-<namespace>`)
to the namespaced RBAC the host-manager chart now renders. It keeps all of
nexus's pre-existing grants and carries migration code. Release B deletes both,
once infra confirms no legacy object remains on any cluster:

```sh
kubectl get clusterrolebinding,clusterrole -l app.kubernetes.io/name=host-manager
# must print nothing, on every cluster
```

Every symbol below carries the comment
`// Migration code: delete in release B (privilege-redesign follow-up issue).`
(`rg 'Migration code: delete in release B'` lists them), and every RBAC marker
to drop carries a `// release B: drop ...` comment above it in
`internal/controller/rbac.go`. When the follow-up issue is filed, replace
"privilege-redesign follow-up issue" in those comments with its number.

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
  builds of release A).
- `teardownHost`: the `legacyRBAC` / `deleteLegacyRBAC` block before the
  uninstall.
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
  imports only they use (`k8s.io/apimachinery/pkg/api/meta`,
  `.../apis/meta/v1/unstructured`, `rbacv1`, `releaseutil`, `sort`, `maps` if
  unused, and `rspb` if unused).

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
makes (`kdexhosts` finalizer/status, `kdexinternalhosts`,
`kdexinternaltranslations`, `kdexinternalutilitypages` and their status,
content-kind status); the D2 grants (impersonate `kdex-installer`, rolebindings,
`bind` on `kcnas-operator-installer`, the cluster-reader binding, namespaces get).

## Tests

- Delete `internal/controller/legacy_rbac_test.go`,
  `internal/controller/legacy_record_envtest_test.go`,
  `internal/utils/helm_legacy_record_test.go`.
- `internal/controller/mocks_test.go`: `recordDoc` and the mock's `records`,
  `statuses`, `rewrites`, `record`, `recordOf`, `setStatus`, `Rewrites`,
  `ReleaseStatus`, `DropClusterRBACFromRecord`, and the record check in
  `Uninstall`; `addRelease` if nothing else uses it.
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
