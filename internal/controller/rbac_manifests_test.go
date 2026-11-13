package controller

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
	RoleRef         struct {
		APIGroup string `json:"apiGroup"`
		Kind     string `json:"kind"`
		Name     string `json:"name"`
	} `json:"roleRef"`
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

// loadManifest decodes the manifest at path into obj, any chart templating
// lines stripped (config/ manifests have none).
func loadManifest(path string, obj any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return yaml.Unmarshal([]byte(stripTemplating(string(b))), obj)
}

func loadRBAC(t *testing.T, path string) rbacObject {
	t.Helper()
	var o rbacObject
	if err := loadManifest(path, &o); err != nil {
		t.Fatalf("load %s: %v", path, err)
	}
	return o
}

func rbacFiles(t *testing.T) map[string]rbacObject {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(repoRoot, "config/rbac/*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	fixed, err := filepath.Glob(filepath.Join(repoRoot, "config/rbac/fixed/*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, fixed...)
	out := map[string]rbacObject{}
	for _, f := range files {
		if filepath.Base(f) == "kustomization.yaml" {
			continue
		}
		o := loadRBAC(t, f)
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
		got := loadRBAC(t, filepath.Join(repoRoot, "chart/templates/rbac", name))
		if got.Kind != want.Kind {
			t.Errorf("%s: chart kind %q != config kind %q", name, got.Kind, want.Kind)
		}
		pairs := map[string][2]any{
			"aggregationRule": {got.AggregationRule, want.AggregationRule},
		}
		// An aggregated role's rules are the aggregation controller's, and
		// neither file declares them (TestAggregatedRolesDeclareNoRules).
		if want.AggregationRule == nil {
			pairs["rules"] = [2]any{got.Rules, want.Rules}
		}
		for what, pair := range pairs {
			g, gerr := yaml.Marshal(pair[0])
			w, werr := yaml.Marshal(pair[1])
			if gerr != nil || werr != nil {
				t.Fatalf("%s: marshal %s: %v %v", name, what, gerr, werr)
			}
			if string(g) != string(w) {
				t.Errorf("%s: chart %s differ from config:\nchart:\n%s\nconfig:\n%s", name, what, g, w)
			}
		}
		// roleRef apiGroup/kind always; the name only for the fixed-name binding
		// (other bindings name a templated/prefixed role, stripped from the chart copy).
		if got.RoleRef.APIGroup != want.RoleRef.APIGroup || got.RoleRef.Kind != want.RoleRef.Kind {
			t.Errorf("%s: roleRef chart=%+v config=%+v", name, got.RoleRef, want.RoleRef)
		}
		if name == "cluster_reader_role_binding.yaml" && got.RoleRef.Name != want.RoleRef.Name {
			t.Errorf("%s: roleRef name chart=%q config=%q", name, got.RoleRef.Name, want.RoleRef.Name)
		}
		if strings.HasPrefix(name, "installer") || strings.HasPrefix(name, "cluster_reader") {
			if got.Metadata.Name != want.Metadata.Name {
				t.Errorf("%s: fixed name chart=%q config=%q", name, got.Metadata.Name, want.Metadata.Name)
			}
			k := "kdex.dev/aggregate-to-installer"
			if got.Metadata.Labels[k] != want.Metadata.Labels[k] {
				t.Errorf("%s: label %s chart=%q config=%q", name, k, got.Metadata.Labels[k], want.Metadata.Labels[k])
			}
		}
	}
}

// The cluster-reader binding's subjects are maintained at runtime by nexus; a
// declared (even empty) list would be reset on every chart upgrade (atomic list).
func TestClusterReaderBindingDeclaresNoSubjects(t *testing.T) {
	for _, f := range []string{"config/rbac/fixed/cluster_reader_role_binding.yaml", "chart/templates/rbac/cluster_reader_role_binding.yaml"} {
		b, err := os.ReadFile(filepath.Join(repoRoot, f))
		if err != nil {
			t.Fatal(err)
		}
		if regexp.MustCompile(`(?m)^subjects:`).Match(b) {
			t.Errorf("%s declares subjects", f)
		}
	}
}

// The clusterrole-aggregation-controller owns an aggregated role's rules. Helm
// v4 applies server-side, so a declared rules key -- even an empty one -- is a
// field the chart claims too, and every upgrade after the first conflicts with
// the controller over it.
func TestAggregatedRolesDeclareNoRules(t *testing.T) {
	var aggregated []string
	for _, dir := range []string{"config/rbac", "config/rbac/fixed", "chart/templates/rbac"} {
		files, err := filepath.Glob(filepath.Join(repoRoot, dir, "*.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			var o map[string]any
			if err := yaml.Unmarshal([]byte(stripTemplating(string(b))), &o); err != nil {
				t.Fatalf("parse %s: %v", f, err)
			}
			if _, ok := o["aggregationRule"]; !ok {
				continue
			}
			aggregated = append(aggregated, f)
			if _, ok := o["rules"]; ok {
				t.Errorf("%s: an aggregated role declares rules", f)
			}
		}
	}
	for _, want := range []string{"config/rbac/fixed/installer_role.yaml", "chart/templates/rbac/installer_role.yaml"} {
		if !slices.Contains(aggregated, filepath.Join(repoRoot, want)) {
			t.Errorf("%s is not an aggregated role (found %v)", want, aggregated)
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
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
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

// nexus updates the cluster-reader binding's subjects without bind on its
// ClusterRole, which the RBAC escalation check allows only while nexus itself
// holds every rule of that role. A rule added there alone would refuse every
// grant and heal; bind would instead let any rule added there be granted to
// any subject. So the manager role must cover the reader role.
func TestManagerRoleCoversClusterReaderRole(t *testing.T) {
	files := rbacFiles(t)
	manager, reader := files["role.yaml"], files["cluster_reader_role.yaml"]
	if manager.Kind == "" || reader.Kind == "" {
		t.Fatal("role.yaml or cluster_reader_role.yaml missing")
	}
	// Only rules over whole resources cover the reader's.
	var whole []rbacRule
	for _, rule := range manager.Rules {
		if len(rule.ResourceNames) == 0 {
			whole = append(whole, rule)
		}
	}
	for _, rule := range reader.Rules {
		if len(rule.ResourceNames) != 0 {
			t.Fatalf("cluster reader rule %+v is name-scoped: extend this test", rule)
		}
		for _, group := range rule.APIGroups {
			for _, resource := range rule.Resources {
				held := ruleVerbs(whole, group, resource)
				for _, verb := range rule.Verbs {
					if !slices.Contains(held, verb) {
						t.Errorf("manager role lacks %s %s/%s, which the cluster reader grants", verb, group, resource)
					}
				}
			}
		}
	}
}
