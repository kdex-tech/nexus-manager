package controller

import (
	"context"
	stderrors "errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/kdex-tech/nexus-manager/internal/page"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func ResolveContents(
	ctx context.Context,
	c client.Client,
	referrer client.Object,
	referrerConditions *[]metav1.Condition,
	contentEntries []kdexv1alpha1.ContentEntry,
	requeueDelay time.Duration,
) (map[string]page.ResolvedContentEntry, bool, ctrl.Result, error) {
	contents := make(map[string]page.ResolvedContentEntry)

	for _, contentEntry := range contentEntries {
		rawHTML := contentEntry.RawHTML
		if rawHTML != "" {
			contents[contentEntry.Slot] = page.ResolvedContentEntry{
				Content: rawHTML,
				Slot:    contentEntry.Slot,
			}

			continue
		}

		appRef := contentEntry.AppRef

		app, shouldReturn, r1, err := ResolveKDexObjectReference(ctx, c, referrer, referrerConditions, appRef, requeueDelay)
		if shouldReturn {
			return nil, shouldReturn, r1, err
		}

		var appSpec *kdexv1alpha1.KDexAppSpec

		switch v := app.(type) {
		case *kdexv1alpha1.KDexApp:
			appSpec = &v.Spec
		case *kdexv1alpha1.KDexClusterApp:
			appSpec = &v.Spec
		}

		if appSpec == nil {
			// The entry had no rawHTML and its appRef was missing or resolved
			// to something that is not a KDexApp/KDexClusterApp. Fail with a
			// descriptive error instead of dereferencing a nil *KDexAppSpec.
			err := fmt.Errorf(
				"content entry for slot %q has no rawHTML and no resolvable KDexApp/KDexClusterApp reference",
				contentEntry.Slot)
			kdexv1alpha1.SetConditions(
				referrerConditions,
				kdexv1alpha1.ConditionStatuses{
					Degraded:    metav1.ConditionTrue,
					Progressing: metav1.ConditionFalse,
					Ready:       metav1.ConditionFalse,
				},
				kdexv1alpha1.ConditionReasonReconcileError,
				err.Error(),
			)
			return nil, true, ctrl.Result{}, err
		}

		contents[contentEntry.Slot] = page.ResolvedContentEntry{
			AppObj:            app,
			Attributes:        contentEntry.Attributes,
			CustomElementName: contentEntry.CustomElementName,
			PackageReference:  &appSpec.PackageReference,
			Scripts:           appSpec.Scripts,
			Slot:              contentEntry.Slot,
		}
	}

	return contents, false, ctrl.Result{}, nil
}

func ResolveHost(
	ctx context.Context,
	c client.Client,
	object client.Object,
	objectConditions *[]metav1.Condition,
	hostRef *corev1.LocalObjectReference,
	requeueDelay time.Duration,
) (*kdexv1alpha1.KDexHost, bool, ctrl.Result, error) {
	if hostRef == nil {
		return nil, false, ctrl.Result{}, nil
	}

	var host kdexv1alpha1.KDexHost
	hostName := types.NamespacedName{
		Name:      hostRef.Name,
		Namespace: object.GetNamespace(),
	}
	if err := c.Get(ctx, hostName, &host); err != nil {
		if errors.IsNotFound(err) {
			kdexv1alpha1.SetConditions(
				objectConditions,
				kdexv1alpha1.ConditionStatuses{
					Degraded:    metav1.ConditionTrue,
					Progressing: metav1.ConditionFalse,
					Ready:       metav1.ConditionFalse,
				},
				kdexv1alpha1.ConditionReasonReconcileError,
				err.Error(),
			)

			return nil, true, ctrl.Result{RequeueAfter: requeueDelay}, nil
		}

		return nil, true, ctrl.Result{}, err
	}

	if isReady, r1, err := isReady(&host, &host.Status.Conditions, requeueDelay); !isReady {
		return nil, true, r1, err
	}

	return &host, false, ctrl.Result{}, nil
}

func ResolvePageNavigations(
	ctx context.Context,
	c client.Client,
	object client.Object,
	objectConditions *[]metav1.Condition,
	navigationRefs map[string]*kdexv1alpha1.KDexObjectReference,
	requeueDelay time.Duration,
) (map[string]page.ResolvedNavigation, bool, ctrl.Result, error) {
	navigations := make(map[string]page.ResolvedNavigation)

	for navigationName, navigationRef := range navigationRefs {
		navigation, shouldReturn, response, err := ResolveKDexObjectReference(
			ctx, c, object, objectConditions, navigationRef, requeueDelay)

		if shouldReturn {
			return nil, true, response, err
		}

		if navigation != nil {
			var navigationSpec *kdexv1alpha1.KDexPageNavigationSpec

			switch v := navigation.(type) {
			case *kdexv1alpha1.KDexPageNavigation:
				navigationSpec = &v.Spec
			case *kdexv1alpha1.KDexClusterPageNavigation:
				navigationSpec = &v.Spec
			}

			navigations[navigationName] = page.ResolvedNavigation{
				Generation: navigation.GetGeneration(),
				Name:       navigation.GetName(),
				Spec:       navigationSpec,
			}
		}
	}

	return navigations, false, ctrl.Result{}, nil
}

func ResolvePage(
	ctx context.Context,
	c client.Client,
	object client.Object,
	objectConditions *[]metav1.Condition,
	pageRef *corev1.LocalObjectReference,
	requeueDelay time.Duration,
) (*kdexv1alpha1.KDexPage, bool, ctrl.Result, error) {
	if pageRef == nil {
		return nil, false, ctrl.Result{}, nil
	}

	var kdexPage kdexv1alpha1.KDexPage
	pageName := types.NamespacedName{
		Name:      pageRef.Name,
		Namespace: object.GetNamespace(),
	}
	if err := c.Get(ctx, pageName, &kdexPage); err != nil {
		if errors.IsNotFound(err) {
			kdexv1alpha1.SetConditions(
				objectConditions,
				kdexv1alpha1.ConditionStatuses{
					Degraded:    metav1.ConditionTrue,
					Progressing: metav1.ConditionFalse,
					Ready:       metav1.ConditionFalse,
				},
				kdexv1alpha1.ConditionReasonReconcileError,
				err.Error(),
			)

			return nil, true, ctrl.Result{RequeueAfter: requeueDelay}, nil
		}

		return nil, true, ctrl.Result{}, err
	}

	if isReady, r1, err := isReady(&kdexPage, &kdexPage.Status.Conditions, requeueDelay); !isReady {
		return nil, true, r1, err
	}

	return &kdexPage, false, ctrl.Result{}, nil
}

// errCrossNamespaceReference is the uniform refusal for a reference into a
// namespace other than the referrer's. Cross-namespace references are disabled
// in this release; the concept is kept for a later opt-in.
var errCrossNamespaceReference = stderrors.New("references to other namespaces are disabled")

func isOtherNamespace(referrer client.Object, namespace string) bool {
	return namespace != "" && namespace != referrer.GetNamespace()
}

func refuseCrossNamespace(conditions *[]metav1.Condition) error {
	kdexv1alpha1.SetConditions(
		conditions,
		kdexv1alpha1.ConditionStatuses{
			Degraded:    metav1.ConditionTrue,
			Progressing: metav1.ConditionFalse,
			Ready:       metav1.ConditionFalse,
		},
		kdexv1alpha1.ConditionReasonReconcileError,
		errCrossNamespaceReference.Error(),
	)

	return errCrossNamespaceReference
}

func ResolveKDexObjectReference(
	ctx context.Context,
	c client.Client,
	referrer client.Object,
	referrerConditions *[]metav1.Condition,
	objectRef *kdexv1alpha1.KDexObjectReference,
	requeueDelay time.Duration,
) (client.Object, bool, ctrl.Result, error) {
	if objectRef == nil {
		return nil, false, ctrl.Result{}, nil
	}

	t := reflect.TypeOf(referrer)

	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}

	referrerKind := t.Name()
	isReferrerClustered := strings.Contains(referrerKind, "Cluster")

	if isReferrerClustered && !strings.Contains(objectRef.Kind, "Cluster") {
		return nil, true, ctrl.Result{}, fmt.Errorf(
			"referrer %s is cluster scoped so %s must also be cluster scoped", referrerKind, objectRef.Kind)
	}

	gvk := schema.GroupVersionKind{Group: "kdex.dev", Version: "v1alpha1", Kind: objectRef.Kind}
	obj, err := c.Scheme().New(gvk)
	if err != nil {
		return nil, true, ctrl.Result{}, fmt.Errorf("unknown kind %s", objectRef.Kind)
	}

	key := client.ObjectKey{Name: objectRef.Name}

	if !isReferrerClustered && !strings.Contains(objectRef.Kind, "Cluster") {
		// Refused before any Get, so the outcome (and the message) cannot
		// reveal whether the target exists in the other namespace.
		if isOtherNamespace(referrer, objectRef.Namespace) {
			return nil, true, ctrl.Result{}, refuseCrossNamespace(referrerConditions)
		}
		key.Namespace = referrer.GetNamespace()
	}

	if err := c.Get(ctx, key, obj.(client.Object)); err != nil {
		kdexv1alpha1.SetConditions(
			referrerConditions,
			kdexv1alpha1.ConditionStatuses{
				Degraded:    metav1.ConditionFalse,
				Progressing: metav1.ConditionTrue,
				Ready:       metav1.ConditionFalse,
			},
			kdexv1alpha1.ConditionReasonReconciling,
			err.Error(),
		)

		return nil, true, ctrl.Result{}, err
	}

	it := reflect.ValueOf(obj).Elem()
	statusField := it.FieldByName("Status")
	conditionsField := statusField.FieldByName("Conditions")
	conditions, ok := conditionsField.Interface().([]metav1.Condition)

	if !ok {
		return obj.(client.Object), true, ctrl.Result{}, fmt.Errorf("no condition field on status %v", obj)
	}

	if isReady, r1, err := isReady(obj.(client.Object), &conditions, requeueDelay); !isReady {
		return obj.(client.Object), true, r1, err
	}

	return obj.(client.Object), false, ctrl.Result{}, nil
}

func ResolveOrDefaultPageArchetype(ctx context.Context, c client.Client, referrer client.Object, referrerConditions *[]metav1.Condition, pageArchetypeRef *kdexv1alpha1.KDexObjectReference, requeueDelay time.Duration) (client.Object, bool, ctrl.Result, error) {
	pageArchetypeObj, shouldReturn, r1, err := ResolveKDexObjectReference(ctx, c, referrer, referrerConditions, pageArchetypeRef, requeueDelay)
	if shouldReturn {
		return nil, shouldReturn, r1, err
	}

	if pageArchetypeObj == nil {
		var clusterPageArchetypeList kdexv1alpha1.KDexClusterPageArchetypeList
		if err = c.List(ctx, &clusterPageArchetypeList, client.MatchingLabels{"kdex.dev/default": "true"}); err != nil {
			kdexv1alpha1.SetConditions(
				referrerConditions,
				kdexv1alpha1.ConditionStatuses{
					Degraded:    metav1.ConditionTrue,
					Progressing: metav1.ConditionFalse,
					Ready:       metav1.ConditionFalse,
				},
				kdexv1alpha1.ConditionReasonReconcileSuccess,
				err.Error(),
			)
			return nil, true, ctrl.Result{}, err
		}

		if len(clusterPageArchetypeList.Items) != 0 {
			slices.SortFunc(clusterPageArchetypeList.Items, func(a, b kdexv1alpha1.KDexClusterPageArchetype) int {
				return a.CreationTimestamp.Compare(b.CreationTimestamp.Time)
			})

			pageArchetypeObj = &clusterPageArchetypeList.Items[0]
		}
	}

	return pageArchetypeObj, false, ctrl.Result{}, nil
}

func ResolveSecret(
	ctx context.Context,
	c client.Client,
	object client.Object,
	objectConditions *[]metav1.Condition,
	secretRef any,
	requeueDelay time.Duration,
) (*corev1.Secret, bool, ctrl.Result, error) {
	if reflect.ValueOf(secretRef).IsNil() {
		return nil, false, ctrl.Result{}, nil
	}

	var secretName types.NamespacedName
	switch v := secretRef.(type) {
	case *corev1.LocalObjectReference:
		secretName = types.NamespacedName{
			Name:      v.Name,
			Namespace: object.GetNamespace(),
		}
	case *kdexv1alpha1.KDexObjectReference:
		// A cluster-scoped referrer (namespace "") is written only by the
		// trusted operator, so its secretRef is operator-authored and takes the
		// operator path with no namespace rule (the webhooks require
		// secretRef.namespace on cluster-scoped resources).
		if object.GetNamespace() == "" {
			return ResolveOperatorSecret(ctx, c, object, objectConditions, v, requeueDelay)
		}
		if isOtherNamespace(object, v.Namespace) {
			return nil, true, ctrl.Result{}, refuseCrossNamespace(objectConditions)
		}
		secretName = types.NamespacedName{
			Name:      v.Name,
			Namespace: object.GetNamespace(),
		}
	default:
		return nil, true, ctrl.Result{}, fmt.Errorf("unknown type %T", secretRef)
	}

	return getSecret(ctx, c, objectConditions, secretName, requeueDelay)
}

// resolvePackageSecret resolves a package reference's credential: its own
// secretRef (tenant rules apply) or, absent that, the operator's default.
func resolvePackageSecret(
	ctx context.Context,
	c client.Client,
	object client.Object,
	conds *[]metav1.Condition,
	own, operatorDefault *kdexv1alpha1.KDexObjectReference,
	requeueDelay time.Duration,
) (*corev1.Secret, bool, ctrl.Result, error) {
	if own != nil {
		return ResolveSecret(ctx, c, object, conds, own, requeueDelay)
	}

	return ResolveOperatorSecret(ctx, c, object, conds, operatorDefault, requeueDelay)
}

// ResolveOperatorSecret resolves a Secret named by the operator's own
// NexusConfiguration (DefaultNpmSecretRef). It applies no namespace rule: the
// operator, not the tenant, chose that namespace. Never use it for a reference
// authored on a tenant resource.
func ResolveOperatorSecret(
	ctx context.Context,
	c client.Client,
	object client.Object,
	conds *[]metav1.Condition,
	ref *kdexv1alpha1.KDexObjectReference,
	requeueDelay time.Duration,
) (*corev1.Secret, bool, ctrl.Result, error) {
	if ref == nil {
		return nil, false, ctrl.Result{}, nil
	}

	namespace := object.GetNamespace()
	if ref.Namespace != "" {
		namespace = ref.Namespace
	}

	return getSecret(ctx, c, conds, types.NamespacedName{Name: ref.Name, Namespace: namespace}, requeueDelay)
}

func getSecret(
	ctx context.Context,
	c client.Client,
	objectConditions *[]metav1.Condition,
	secretName types.NamespacedName,
	requeueDelay time.Duration,
) (*corev1.Secret, bool, ctrl.Result, error) {
	var secret corev1.Secret
	if err := c.Get(ctx, secretName, &secret); err != nil {
		if errors.IsNotFound(err) {
			kdexv1alpha1.SetConditions(
				objectConditions,
				kdexv1alpha1.ConditionStatuses{
					Degraded:    metav1.ConditionTrue,
					Progressing: metav1.ConditionFalse,
					Ready:       metav1.ConditionFalse,
				},
				kdexv1alpha1.ConditionReasonReconcileError,
				err.Error(),
			)

			return nil, true, ctrl.Result{RequeueAfter: requeueDelay}, nil
		}

		// Any other error (timeout, forbidden, server error, ...) must be
		// propagated so the reconcile retries instead of silently proceeding
		// with an empty Secret.
		return nil, true, ctrl.Result{}, err
	}

	return &secret, false, ctrl.Result{}, nil
}

func ResolveSecrets(ctx context.Context, c client.Client, objectStatus *kdexv1alpha1.KDexObjectStatus, namespace string, selector *metav1.LabelSelector) (kdexv1alpha1.Secrets, error) {
	if selector == nil {
		return kdexv1alpha1.Secrets{}, nil
	}

	sel, err := metav1.LabelSelectorAsSelector(selector)
	if err != nil {
		return nil, fmt.Errorf("invalid secretSelector: %w", err)
	}

	var list corev1.SecretList
	if err := c.List(ctx, &list, client.InNamespace(namespace), client.MatchingLabelsSelector{Selector: sel}); err != nil {
		return nil, err
	}

	slices.SortFunc(list.Items, func(a, b corev1.Secret) int {
		return strings.Compare(a.Name, b.Name)
	})

	secrets := make(kdexv1alpha1.Secrets, 0, len(list.Items))
	for _, secret := range list.Items {
		secrets = append(secrets, secret)
		objectStatus.Attributes[secret.Name+".secret.generation"] = fmt.Sprintf("%d", secret.GetGeneration())
	}
	return secrets, nil
}

func isReady(
	referred client.Object,
	referredConditions *[]metav1.Condition,
	requeueDelay time.Duration,
) (bool, ctrl.Result, error) {
	t := reflect.TypeOf(referred)
	if t == nil {
		return false, ctrl.Result{}, fmt.Errorf("referred is nil")
	}
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}

	if !meta.IsStatusConditionTrue(*referredConditions, string(kdexv1alpha1.ConditionTypeReady)) {
		kdexv1alpha1.SetConditions(
			referredConditions,
			kdexv1alpha1.ConditionStatuses{
				Degraded:    metav1.ConditionTrue,
				Progressing: metav1.ConditionFalse,
				Ready:       metav1.ConditionFalse,
			},
			kdexv1alpha1.ConditionReasonReconcileError,
			fmt.Sprintf("referenced %s %s is not ready", t.Name(), referred.GetName()),
		)

		return false, ctrl.Result{RequeueAfter: requeueDelay}, nil
	}

	return true, ctrl.Result{}, nil
}
