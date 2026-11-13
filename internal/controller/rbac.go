package controller

// +kubebuilder:rbac:groups=apps,resources=deployments,                                 verbs=get
// +kubebuilder:rbac:groups=core,resources=secrets,                                     verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=serviceaccounts,                             verbs=get;list;watch;create;update;delete
// +kubebuilder:rbac:groups=kdex.dev,resources=kdexapps,                                verbs=get;list;watch
// +kubebuilder:rbac:groups=kdex.dev,resources=kdexapps/status,                         verbs=get;update;patch
// +kubebuilder:rbac:groups=kdex.dev,resources=kdexclusterapps,                         verbs=get;list;watch
// +kubebuilder:rbac:groups=kdex.dev,resources=kdexclusterapps/status,                  verbs=get;update;patch
// +kubebuilder:rbac:groups=kdex.dev,resources=kdexclusterfaasadaptors,                 verbs=get;list;watch
// +kubebuilder:rbac:groups=kdex.dev,resources=kdexclusterpagearchetypes,               verbs=get;list;watch
// +kubebuilder:rbac:groups=kdex.dev,resources=kdexclusterpagearchetypes/status,        verbs=get;update;patch
// +kubebuilder:rbac:groups=kdex.dev,resources=kdexclusterpagefooters,                  verbs=get;list;watch
// +kubebuilder:rbac:groups=kdex.dev,resources=kdexclusterpagefooters/status,           verbs=get;update;patch
// +kubebuilder:rbac:groups=kdex.dev,resources=kdexclusterpageheaders,                  verbs=get;list;watch
// +kubebuilder:rbac:groups=kdex.dev,resources=kdexclusterpageheaders/status,           verbs=get;update;patch
// +kubebuilder:rbac:groups=kdex.dev,resources=kdexclusterpagenavigations,              verbs=get;list;watch
// +kubebuilder:rbac:groups=kdex.dev,resources=kdexclusterpagenavigations/status,       verbs=get;update;patch
// +kubebuilder:rbac:groups=kdex.dev,resources=kdexclusterscriptlibraries,              verbs=get;list;watch
// +kubebuilder:rbac:groups=kdex.dev,resources=kdexclusterscriptlibraries/status,       verbs=get;update;patch
// +kubebuilder:rbac:groups=kdex.dev,resources=kdexclusterthemes,                       verbs=get;list;watch
// +kubebuilder:rbac:groups=kdex.dev,resources=kdexclusterthemes/status,                verbs=get;update;patch
// +kubebuilder:rbac:groups=kdex.dev,resources=kdexclustertranslations,                 verbs=get;list;watch
// +kubebuilder:rbac:groups=kdex.dev,resources=kdexclustertranslations/status,          verbs=get;update;patch
// +kubebuilder:rbac:groups=kdex.dev,resources=kdexclusterutilitypages,                 verbs=get;list;watch
// +kubebuilder:rbac:groups=kdex.dev,resources=kdexclusterutilitypages/status,          verbs=get;update;patch
// +kubebuilder:rbac:groups=kdex.dev,resources=kdexfaasadaptors,                        verbs=get;list;watch
// +kubebuilder:rbac:groups=kdex.dev,resources=kdexfunctions,                           verbs=get;list;watch
// +kubebuilder:rbac:groups=kdex.dev,resources=kdexhosts,                               verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=kdex.dev,resources=kdexhosts/finalizers,                    verbs=update
// +kubebuilder:rbac:groups=kdex.dev,resources=kdexhosts/status,                        verbs=get;update;patch
// +kubebuilder:rbac:groups=kdex.dev,resources=kdexinternalhosts,                       verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kdex.dev,resources=kdexinternaltranslations,                verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kdex.dev,resources=kdexinternaltranslations/status,         verbs=get;update;patch
// +kubebuilder:rbac:groups=kdex.dev,resources=kdexinternalutilitypages,                verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kdex.dev,resources=kdexinternalutilitypages/status,         verbs=get;update;patch
// +kubebuilder:rbac:groups=kdex.dev,resources=kdexpagearchetypes,                      verbs=get;list;watch
// +kubebuilder:rbac:groups=kdex.dev,resources=kdexpagearchetypes/status,               verbs=get;update;patch
// +kubebuilder:rbac:groups=kdex.dev,resources=kdexpages,                               verbs=get;list;watch
// +kubebuilder:rbac:groups=kdex.dev,resources=kdexpagefooters,                         verbs=get;list;watch
// +kubebuilder:rbac:groups=kdex.dev,resources=kdexpagefooters/status,                  verbs=get;update;patch
// +kubebuilder:rbac:groups=kdex.dev,resources=kdexpageheaders,                         verbs=get;list;watch
// +kubebuilder:rbac:groups=kdex.dev,resources=kdexpageheaders/status,                  verbs=get;update;patch
// +kubebuilder:rbac:groups=kdex.dev,resources=kdexpagenavigations,                     verbs=get;list;watch
// +kubebuilder:rbac:groups=kdex.dev,resources=kdexpagenavigations/status,              verbs=get;update;patch
// +kubebuilder:rbac:groups=kdex.dev,resources=kdexscriptlibraries,                     verbs=get;list;watch
// +kubebuilder:rbac:groups=kdex.dev,resources=kdexscriptlibraries/status,              verbs=get;update;patch
// +kubebuilder:rbac:groups=kdex.dev,resources=kdexthemes,                              verbs=get;list;watch
// +kubebuilder:rbac:groups=kdex.dev,resources=kdexthemes/status,                       verbs=get;update;patch
// +kubebuilder:rbac:groups=kdex.dev,resources=kdextranslations,                        verbs=get;list;watch
// +kubebuilder:rbac:groups=kdex.dev,resources=kdextranslations/status,                 verbs=get;update;patch
// +kubebuilder:rbac:groups=kdex.dev,resources=kdexhostextensions,                       verbs=get;list;watch
// +kubebuilder:rbac:groups=kdex.dev,resources=kdexhostextensions/status,                verbs=get;update;patch
// +kubebuilder:rbac:groups=kdex.dev,resources=kdexutilitypages,                        verbs=get;list;watch
// +kubebuilder:rbac:groups=kdex.dev,resources=kdexutilitypages/status,                 verbs=get;update;patch
// The per-namespace installer SA is impersonated for Helm and bound to the
// fixed installer ClusterRole. The fixed cluster-reader ClusterRoleBinding's
// subject list is maintained, and the binding is watched through an informer
// field-selected on metadata.name (cache_options.go), which needs list;watch.
// bind on the installer ClusterRole lets nexus write installer bindings
// without holding its (aggregated, changing) rules; the admission policy
// installer_namespace_policy.yaml confines where. The cluster-reader binding
// gets no bind: nexus may update its subjects only because it holds every rule
// of that role itself (TestManagerRoleCoversClusterReaderRole), so no rule
// beyond nexus's own can ever be granted through it.
// +kubebuilder:rbac:groups=core,resources=serviceaccounts,resourceNames=kdex-installer,verbs=impersonate
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=rolebindings,verbs=get;list;watch;create;update;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles,resourceNames=kcnas-operator-installer,verbs=bind
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterrolebindings,resourceNames=kcnas-operator-host-manager-cluster-reader,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=core,resources=namespaces,verbs=get
