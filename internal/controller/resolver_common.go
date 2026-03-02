package controller

import (
	"context"
	"fmt"
	"reflect"
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

func ResolvePageBinding(
	ctx context.Context,
	c client.Client,
	object client.Object,
	objectConditions *[]metav1.Condition,
	pageBindingRef *corev1.LocalObjectReference,
	requeueDelay time.Duration,
) (*kdexv1alpha1.KDexPageBinding, bool, ctrl.Result, error) {
	if pageBindingRef == nil {
		return nil, false, ctrl.Result{}, nil
	}

	var pageBinding kdexv1alpha1.KDexPageBinding
	pageBindingName := types.NamespacedName{
		Name:      pageBindingRef.Name,
		Namespace: object.GetNamespace(),
	}
	if err := c.Get(ctx, pageBindingName, &pageBinding); err != nil {
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

	if isReady, r1, err := isReady(&pageBinding, &pageBinding.Status.Conditions, requeueDelay); !isReady {
		return nil, true, r1, err
	}

	return &pageBinding, false, ctrl.Result{}, nil
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

	if t.Kind() == reflect.Ptr {
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
		key.Namespace = referrer.GetNamespace()
		if objectRef.Namespace != "" {
			key.Namespace = objectRef.Namespace
		}
	}

	if err := c.Get(ctx, key, obj.(client.Object)); err != nil {
		if errors.IsNotFound(err) {
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

			return nil, true, ctrl.Result{RequeueAfter: requeueDelay}, nil
		}
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
		namespace := object.GetNamespace()
		if v.Namespace != "" {
			namespace = v.Namespace
		}
		secretName = types.NamespacedName{
			Name:      v.Name,
			Namespace: namespace,
		}
	default:
		return nil, true, ctrl.Result{}, fmt.Errorf("unknown type %T", secretRef)
	}

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
	}

	return &secret, false, ctrl.Result{}, nil
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
	if t.Kind() == reflect.Ptr {
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
