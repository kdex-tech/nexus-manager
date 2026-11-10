package utils

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"

	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
)

func IfElse[T any](predicate bool, trueVal T, elseVal T) T {
	if predicate {
		return trueVal
	}
	return elseVal
}

func MapSlice[T any, U any](slice []T, mapper func(T) U) []U {
	result := make([]U, len(slice))
	for i, v := range slice {
		result[i] = mapper(v)
	}
	return result
}

// HashSecrets identifies a set of Secrets by version: each one's
// namespace/name@resourceVersion, sorted, so two reads of the same Secrets
// agree and an update to any of them -- which bumps its resourceVersion --
// does not. Nothing a fresh copy changes, such as the addresses behind its
// pointer fields, takes part. Nor are a Secret's contents hashed: the result
// is recorded in status.
func HashSecrets(secrets kdexv1alpha1.Secrets) string {
	ids := make([]string, 0, len(secrets))
	for _, s := range secrets {
		ids = append(ids, s.Namespace+"/"+s.Name+"@"+s.ResourceVersion)
	}
	slices.Sort(ids)
	h := sha256.New()
	for _, id := range ids {
		h.Write([]byte(id))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}
