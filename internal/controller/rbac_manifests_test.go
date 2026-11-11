package controller

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

type rbacRule struct {
	APIGroups     []string `json:"apiGroups,omitempty"`
	Resources     []string `json:"resources,omitempty"`
	ResourceNames []string `json:"resourceNames,omitempty"`
	Verbs         []string `json:"verbs,omitempty"`
}

type rbacObject struct {
	Kind     string `json:"kind"`
	Metadata struct {
		Name   string            `json:"name"`
		Labels map[string]string `json:"labels"`
	} `json:"metadata"`
	Rules           []rbacRule `json:"rules"`
	AggregationRule any        `json:"aggregationRule"`
	RoleRef         any        `json:"roleRef"`
	Subjects        any        `json:"subjects"`
}

const repoRoot = "../.."

var rbacKinds = []string{"Role", "ClusterRole", "RoleBinding", "ClusterRoleBinding"}

// stripTemplating drops Helm template lines so the remainder parses as YAML.
func stripTemplating(src string) string {
	var out []string
	for _, l := range strings.Split(src, "\n") {
		if !strings.Contains(l, "{{") {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

func loadRBAC(t *testing.T, path string, strip bool) rbacObject {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	src := string(b)
	if strip {
		src = stripTemplating(src)
	}
	var o rbacObject
	if err := yaml.Unmarshal([]byte(src), &o); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return o
}

func rbacFiles(t *testing.T) map[string]rbacObject {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(repoRoot, "config/rbac/*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]rbacObject{}
	for _, f := range files {
		if filepath.Base(f) == "kustomization.yaml" {
			continue
		}
		o := loadRBAC(t, f, false)
		if slices.Contains(rbacKinds, o.Kind) {
			out[filepath.Base(f)] = o
		}
	}
	return out
}

func TestChartRBACMatchesConfig(t *testing.T) {
	cfg := rbacFiles(t)
	if len(cfg) == 0 {
		t.Fatal("no RBAC manifests found in config/rbac")
	}
	for name, want := range cfg {
		got := loadRBAC(t, filepath.Join(repoRoot, "chart/templates/rbac", name), true)
		if got.Kind != want.Kind {
			t.Errorf("%s: chart kind %q != config kind %q", name, got.Kind, want.Kind)
		}
		for what, pair := range map[string][2]any{
			"rules":           {got.Rules, want.Rules},
			"aggregationRule": {got.AggregationRule, want.AggregationRule},
			"roleRef":         {got.RoleRef, want.RoleRef},
			"subjects":        {got.Subjects, want.Subjects},
		} {
			g, _ := yaml.Marshal(pair[0])
			w, _ := yaml.Marshal(pair[1])
			// Role names differ by prefix (kustomize namePrefix vs Helm fullname); roleRef
			// names are therefore compared only for the fixed cluster-reader binding below.
			// Subjects are templated in the chart (SA name/namespace), so only the
			// fixed cluster-reader binding's (operator-maintained) list is compared.
			if what == "roleRef" || (what == "subjects" && name != "cluster_reader_role_binding.yaml") {
				continue
			}
			if string(g) != string(w) {
				t.Errorf("%s: chart %s differ from config:\nchart:\n%s\nconfig:\n%s", name, what, g, w)
			}
		}
		for _, k := range []string{"app.kubernetes.io/managed-by", "kdex.dev/aggregate-to-installer"} {
			if strings.HasPrefix(name, "installer") || strings.HasPrefix(name, "cluster_reader") {
				if got.Metadata.Labels[k] != want.Metadata.Labels[k] {
					t.Errorf("%s: label %s chart=%q config=%q", name, k, got.Metadata.Labels[k], want.Metadata.Labels[k])
				}
			}
		}
	}
}

func ruleVerbs(rules []rbacRule, group, resource string) []string {
	var verbs []string
	for _, r := range rules {
		if slices.Contains(r.APIGroups, group) && slices.Contains(r.Resources, resource) {
			verbs = append(verbs, r.Verbs...)
		}
	}
	slices.Sort(verbs)
	return slices.Compact(verbs)
}

func TestClusterReaderCoversClusterScopedKDexKinds(t *testing.T) {
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "kdex.dev/crds").Output()
	if err != nil {
		t.Fatalf("go list kdex.dev/crds: %v", err)
	}
	crdDir := filepath.Join(strings.TrimSpace(string(out)), "config/crd/bases")
	files, err := filepath.Glob(filepath.Join(crdDir, "*.yaml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no CRDs under %s: %v", crdDir, err)
	}
	reader := rbacFiles(t)["cluster_reader_role.yaml"]
	if reader.Kind == "" {
		t.Fatal("config/rbac/cluster_reader_role.yaml missing")
	}
	checked := 0
	for _, f := range files {
		b, _ := os.ReadFile(f)
		var crd struct {
			Spec struct {
				Scope string `json:"scope"`
				Group string `json:"group"`
				Names struct {
					Kind   string `json:"kind"`
					Plural string `json:"plural"`
				} `json:"names"`
			} `json:"spec"`
		}
		if err := yaml.Unmarshal(b, &crd); err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		if crd.Spec.Scope != "Cluster" || !strings.HasPrefix(crd.Spec.Names.Kind, "KDexCluster") {
			continue
		}
		checked++
		verbs := ruleVerbs(reader.Rules, crd.Spec.Group, crd.Spec.Names.Plural)
		if !slices.Equal(verbs, []string{"get", "list", "watch"}) {
			t.Errorf("cluster-reader verbs for %s = %v, want [get list watch]", crd.Spec.Names.Plural, verbs)
		}
	}
	if checked == 0 {
		t.Fatal("found no cluster-scoped KDexCluster* CRDs")
	}
}

func TestInstallerBaseCoversLiveCompanions(t *testing.T) {
	base := rbacFiles(t)["installer_base_role.yaml"]
	if base.Kind == "" {
		t.Fatal("config/rbac/installer_base_role.yaml missing")
	}
	if base.Metadata.Labels["kdex.dev/aggregate-to-installer"] != "true" {
		t.Error("installer-base lacks the aggregate-to-installer label")
	}
	full := []string{"create", "delete", "get", "list", "patch", "update", "watch"}
	want := map[string][]string{
		"":                          {"configmaps", "services", "serviceaccounts", "persistentvolumeclaims", "secrets"},
		"apps":                      {"deployments"},
		"kdex.dev":                  {"kdexapps", "kdexfunctions", "kdexhostextensions", "kdexpages", "kdexroles", "kdexrolebindings", "kdextranslations"},
		"rbac.authorization.k8s.io": {"roles", "rolebindings"},
	}
	for group, resources := range want {
		for _, res := range resources {
			verbs := ruleVerbs(base.Rules, group, res)
			for _, v := range full {
				if !slices.Contains(verbs, v) {
					t.Errorf("installer-base lacks %s on %s/%s", v, group, res)
				}
			}
		}
	}
	roles := ruleVerbs(base.Rules, "rbac.authorization.k8s.io", "roles")
	for _, v := range []string{"escalate", "bind"} {
		if !slices.Contains(roles, v) {
			t.Errorf("installer-base lacks %s on roles", v)
		}
	}
}

func TestNoHostManagerClusterRoleEnv(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(repoRoot, "chart/templates/manager/manager.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "HOST_MANAGER_CLUSTER_ROLE") {
		t.Error("chart manager.yaml still sets HOST_MANAGER_CLUSTER_ROLE")
	}
}
