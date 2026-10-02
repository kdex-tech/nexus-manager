package controller

import (
	"fmt"
	"slices"
	"strings"

	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
)

// translationSource is one translation resolved for a host, before it is copied
// into a KDexInternalTranslation named "<host>-<Name>".
type translationSource struct {
	Kind       string
	Namespace  string // empty for KDexClusterTranslation
	Name       string
	Generation int64
	Spec       kdexv1alpha1.KDexTranslationSpec
}

func (s translationSource) key() string {
	return s.Kind + "/" + s.Namespace + "/" + s.Name
}

// orderTranslationSources returns a host's translations from lowest to highest
// precedence: the default, then self-attached translations (by name), then the
// host's translationRefs (in list order). host-manager writes its catalog in
// this order and the catalog is last-write-wins, so the host-declared value
// beats anything a self-attached translation ships.
//
// A translation that is both self-attached and host-declared keeps only its
// host-declared position. Two distinct sources with the same Name would share
// one KDexInternalTranslation; the higher-precedence one is kept and collision
// describes each conflict (empty when there is none).
func orderTranslationSources(
	defaultSrc *translationSource,
	selfAttached, declared []translationSource,
) ([]translationSource, string) {
	declaredKeys := make(map[string]bool, len(declared))
	for _, s := range declared {
		declaredKeys[s.key()] = true
	}

	self := slices.Clone(selfAttached)
	slices.SortFunc(self, func(a, b translationSource) int { return strings.Compare(a.Name, b.Name) })

	candidates := []translationSource{}
	if defaultSrc != nil {
		candidates = append(candidates, *defaultSrc)
	}
	for _, s := range self {
		if !declaredKeys[s.key()] {
			candidates = append(candidates, s)
		}
	}
	candidates = append(candidates, declared...)

	// Walk from highest precedence down so the first claimant of a name wins.
	winner := map[string]translationSource{}
	keep := make([]bool, len(candidates))
	conflicts := []string{}
	for i := len(candidates) - 1; i >= 0; i-- {
		c := candidates[i]
		prev, taken := winner[c.Name]
		if !taken {
			winner[c.Name] = c
			keep[i] = true
			continue
		}
		if prev.key() != c.key() {
			conflicts = append(conflicts, fmt.Sprintf("%s and %s both map to internal translation suffix %q; using %s",
				c.key(), prev.key(), c.Name, prev.key()))
		}
	}

	ordered := make([]translationSource, 0, len(candidates))
	for i, c := range candidates {
		if keep[i] {
			ordered = append(ordered, c)
		}
	}
	slices.Sort(conflicts)
	return ordered, strings.Join(conflicts, "; ")
}
