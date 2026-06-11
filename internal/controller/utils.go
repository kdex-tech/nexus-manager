package controller

import (
	"context"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/jsonpath"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	FALSE        = "false"
	MAX_ATTEMPTS = 10
)

// nolint:gocyclo
func MakeHandlerByReferencePath(
	c client.Client,
	scheme *runtime.Scheme,
	watcherType client.Object,
	list client.ObjectList,
	referencePath ...string,
) handler.EventHandler {
	watcherKind, err := getKind(watcherType, scheme)
	if err != nil {
		panic(err)
	}

	log := logf.Log.WithName(
		strings.ToLower(watcherKind),
	).WithName(
		"watch",
	).WithValues(
		"referencePaths", referencePath,
	)

	jpRefs := make([]*jsonpath.JSONPath, len(referencePath))
	for i, refPath := range referencePath {
		jpRefs[i] = jsonpath.New("ref-path-" + strconv.Itoa(i))
		if err := jpRefs[i].Parse(refPath); err != nil {
			panic(err)
		}
	}

	return handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, o client.Object) []reconcile.Request {
		objKind, err := getKind(o, scheme)
		if err != nil {
			log.Error(err, "failed to get object kind", "object", o)
			return []reconcile.Request{}
		}

		if err := c.List(ctx, list, client.InNamespace(o.GetNamespace())); err != nil {
			return []reconcile.Request{}
		}

		items, err := meta.ExtractList(list)
		if err != nil || len(items) == 0 {
			return []reconcile.Request{}
		}

		requests := []reconcile.Request{}
		for _, i := range items {
			item := i.(client.Object)

			log.V(2).Info("processing item", "object", item.GetName(), "namespace", item.GetNamespace())

			for j, jpRef := range jpRefs {
				curPath := referencePath[j]
				jsonPathReference, err := jpRef.FindResults(item)
				if err != nil {
					log.V(2).Info("skipping", "path", curPath, "err", err, "object", item.GetName(), "namespace", item.GetNamespace())
					continue
				}
				if len(jsonPathReference) == 0 || len(jsonPathReference[0]) == 0 {
					log.V(2).Info("skipping", "path", curPath, "object", item.GetName(), "namespace", item.GetNamespace())
					continue
				}

				log.V(2).Info("found", "path", curPath, "object", item.GetName(), "namespace", item.GetNamespace())

				for idx, node := range jsonPathReference {
					for _, curRef := range node {
						ref := reflect.ValueOf(curRef.Interface())

						log.V(2).Info("reference", "reference", ref, "object", item.GetName(), "node", idx)

						isNil := false
						switch ref.Kind() {
						case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
							isNil = ref.IsNil()
						}
						if ref.IsZero() || isNil {
							continue
						}

						theReferenceStruct := ref.Interface()

						log.V(2).Info("struct", "interface", theReferenceStruct, "object", item.GetName(), "node", idx)

						switch v := theReferenceStruct.(type) {
						case string:
							if v == o.GetName() {
								requests = append(requests, reconcile.Request{
									NamespacedName: types.NamespacedName{
										Name:      item.GetName(),
										Namespace: item.GetNamespace(),
									},
								})
							}
						case []string:
							if slices.Contains(v, o.GetName()) {
								requests = append(requests, reconcile.Request{
									NamespacedName: types.NamespacedName{
										Name:      item.GetName(),
										Namespace: item.GetNamespace(),
									},
								})
							}
						case corev1.LocalObjectReference:
							if v.Name == o.GetName() {
								requests = append(requests, reconcile.Request{
									NamespacedName: types.NamespacedName{
										Name:      item.GetName(),
										Namespace: item.GetNamespace(),
									},
								})
							}
						case *corev1.LocalObjectReference:
							if v.Name == o.GetName() {
								requests = append(requests, reconcile.Request{
									NamespacedName: types.NamespacedName{
										Name:      item.GetName(),
										Namespace: item.GetNamespace(),
									},
								})
							}
						case kdexv1alpha1.KDexObjectReference:
							namespace := item.GetNamespace()
							if v.Namespace != "" {
								namespace = v.Namespace
							}
							if v.Kind == objKind && v.Name == o.GetName() && item.GetNamespace() == namespace {
								requests = append(requests, reconcile.Request{
									NamespacedName: types.NamespacedName{
										Name:      item.GetName(),
										Namespace: item.GetNamespace(),
									},
								})
							}
						case *kdexv1alpha1.KDexObjectReference:
							namespace := item.GetNamespace()
							if v.Namespace != "" {
								namespace = v.Namespace
							}
							if v.Kind == objKind && v.Name == o.GetName() && item.GetNamespace() == namespace {
								requests = append(requests, reconcile.Request{
									NamespacedName: types.NamespacedName{
										Name:      item.GetName(),
										Namespace: item.GetNamespace(),
									},
								})
							}
						}
					}
				}
			}
		}
		return requests
	})
}

// MakeHandlerForDefaultSecret enqueues every item in list whenever the
// cluster-default credential Secret named by defaultRef changes. The
// per-resource secretRef watches (MakeHandlerByReferencePath) only fire for
// resources that name a Secret explicitly; resources that fall back to the
// NexusConfiguration default carry no such reference, so without this the
// default Secret would behave as a fixed snapshot — a rotation (e.g. ESO) of
// the default credential would never requeue the dependent reconciles. We
// enqueue all items rather than only the ones currently relying on the default:
// reconciling a resource that has its own secretRef is idempotent, and
// default-secret change events are rare (rotation), so the extra work is
// negligible and avoids re-deriving per-item fallback state in the handler.
func MakeHandlerForDefaultSecret(
	c client.Client,
	list client.ObjectList,
	defaultRef *kdexv1alpha1.KDexObjectReference,
) handler.EventHandler {
	return handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, o client.Object) []reconcile.Request {
		return defaultSecretRequests(ctx, c, list, defaultRef, o)
	})
}

// defaultSecretRequests returns a reconcile request for every item in list when
// the changed Secret o is the cluster-default credential named by defaultRef,
// and nil otherwise. An empty defaultRef.Namespace matches the Secret in any
// namespace (name-only); an explicit namespace must match exactly. Split out
// from MakeHandlerForDefaultSecret so the mapping logic is unit-testable
// without standing up a controller manager.
func defaultSecretRequests(
	ctx context.Context,
	c client.Client,
	list client.ObjectList,
	defaultRef *kdexv1alpha1.KDexObjectReference,
	o client.Object,
) []reconcile.Request {
	if defaultRef == nil || defaultRef.Name == "" {
		return nil
	}
	if o.GetName() != defaultRef.Name {
		return nil
	}
	if defaultRef.Namespace != "" && o.GetNamespace() != defaultRef.Namespace {
		return nil
	}

	if err := c.List(ctx, list); err != nil {
		return nil
	}

	items, err := meta.ExtractList(list)
	if err != nil {
		return nil
	}

	requests := make([]reconcile.Request, 0, len(items))
	for _, i := range items {
		item := i.(client.Object)
		requests = append(requests, reconcile.Request{
			NamespacedName: types.NamespacedName{
				Name:      item.GetName(),
				Namespace: item.GetNamespace(),
			},
		})
	}
	return requests
}

func MergeEnvVars(existing []corev1.EnvVar, overrides []corev1.EnvVar) []corev1.EnvVar {
	// Create a map to track the index of existing environment variables
	indexMap := make(map[string]int)
	for i, env := range existing {
		indexMap[env.Name] = i
	}

	for _, override := range overrides {
		if idx, found := indexMap[override.Name]; found {
			// Found a match: Update the existing entry
			existing[idx] = override
		} else {
			// No match: Append the new entry and update the map
			existing = append(existing, override)
			indexMap[override.Name] = len(existing) - 1
		}
	}

	return existing
}

func LogConstructor(name string, mgr ctrl.Manager) func(request *reconcile.Request) logr.Logger {
	return func(request *reconcile.Request) logr.Logger {
		l := mgr.GetControllerOptions().Logger.WithName(name)
		if request != nil {
			l = l.WithValues("namespace", request.Namespace, "name", request.Name)
		}
		return l
	}
}

func getKind(obj client.Object, scheme *runtime.Scheme) (string, error) {
	gvk, err := apiutil.GVKForObject(obj, scheme)
	if err != nil {
		return "", err
	}
	return gvk.Kind, nil
}
