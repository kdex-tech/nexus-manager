package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/runtime"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
	"kdex.dev/crds/linter"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// +kubebuilder:webhook:path=/validate-kdex-dev-v1alpha1-kdexfunction,mutating=false,failurePolicy=Ignore,sideEffects=None,groups=kdex.dev,resources=kdexfunctions;kdexfunctions/status,verbs=create;update,versions=v1alpha1,name=validate.kdexfunction.kdex.dev,admissionReviewVersions=v1

type KDexFunctionValidator[T runtime.Object] struct {
}

var _ admission.Validator[*kdexv1alpha1.KDexFunction] = &KDexFunctionValidator[*kdexv1alpha1.KDexFunction]{}

func (v *KDexFunctionValidator[T]) ValidateCreate(ctx context.Context, obj T) (admission.Warnings, error) {
	return v.validate(ctx, obj)
}

func (v *KDexFunctionValidator[T]) ValidateUpdate(ctx context.Context, oldObj, newObj T) (admission.Warnings, error) {
	return v.validate(ctx, newObj)
}

func (v *KDexFunctionValidator[T]) ValidateDelete(ctx context.Context, obj T) (admission.Warnings, error) {
	return nil, nil
}

func (v *KDexFunctionValidator[T]) validate(_ context.Context, obj T) (admission.Warnings, error) {
	var function *kdexv1alpha1.KDexFunction

	switch t := any(obj).(type) {
	case *kdexv1alpha1.KDexFunction:
		function = t
	default:
		return nil, fmt.Errorf("unsupported type: %T", t)
	}

	spec := &function.Spec

	// 1. Structural Validation
	if spec.HostRef.Name == "" {
		return nil, fmt.Errorf("spec.hostRef.name must not be empty")
	}

	re := spec.API.BasePathRegex()
	if !re.MatchString(spec.API.BasePath) {
		return nil, fmt.Errorf("spec.api.basePath %s does not match %s", spec.API.BasePath, re.String())
	}

	re = spec.API.ItemPathRegex()
	for curPath := range spec.API.Paths {
		if !re.MatchString(curPath) {
			return nil, fmt.Errorf("spec.api.paths[%s] does not match %s", curPath, re.String())
		}
	}

	// 2. OpenAPI Validation using vacuum
	if err := v.validateOpenAPI(spec); err != nil {
		return nil, fmt.Errorf("OpenAPI validation failed: %w", err)
	}

	return nil, nil
}

func (v *KDexFunctionValidator[T]) validateOpenAPI(spec *kdexv1alpha1.KDexFunctionSpec) error {
	// Build a minimal OpenAPI 3.0 document from the spec.API
	openAPIDoc := map[string]any{
		"openapi": "3.0.3",
		"info": map[string]any{
			"title":       "Function API",
			"version":     "1.0.0",
			"description": "Auto-generated OpenAPI specification for KDexFunction",
		},
		"paths": map[string]any{},
		"components": map[string]any{
			"responses": map[string]any{
				"BadRequest": map[string]any{
					"description": "Bad Request",
				},
				"Found": map[string]any{
					"description": "Found",
				},
				"InternalServerError": map[string]any{
					"description": "Internal Server Error",
				},
				"NotFound": map[string]any{
					"description": "Not Found",
				},
				"SeeOther": map[string]any{
					"description": "See Other",
				},
				"Unauthorized": map[string]any{
					"description": "Unauthorized",
				},
			},
			"securitySchemes": map[string]any{},
		},
	}

	components := openAPIDoc["components"].(map[string]any)
	securitySchemes := components["securitySchemes"].(map[string]any)

	collector := func(op map[string]any) {
		v, ok := op["security"]
		if !ok {
			return
		}
		// security is user-supplied JSON; tolerate the wrong shape instead of
		// panicking on an unchecked type assertion.
		arr, ok := v.([]any)
		if !ok {
			return
		}
		for _, itm := range arr {
			m, ok := itm.(map[string]any)
			if !ok {
				continue
			}
			for k := range m {
				securitySchemes[k] = map[string]any{
					"type":   "http",
					"scheme": "bearer",
				}
			}
		}
	}

	// Convert spec.API.Paths to standard OpenAPI paths
	paths := make(map[string]any)
	for pathKey, pathItem := range spec.API.Paths {
		pathObj := make(map[string]any)

		if pathItem.Description != "" {
			pathObj["description"] = pathItem.Description
		}
		if pathItem.Summary != "" {
			pathObj["summary"] = pathItem.Summary
		}

		// Add operations
		ops := []struct {
			name string
			raw  *runtime.RawExtension
		}{
			{"connect", pathItem.Connect},
			{"delete", pathItem.Delete},
			{"get", pathItem.Get},
			{"head", pathItem.Head},
			{"options", pathItem.Options},
			{"patch", pathItem.Patch},
			{"post", pathItem.Post},
			{"put", pathItem.Put},
			{"trace", pathItem.Trace},
		}

		for _, opInfo := range ops {
			if opInfo.raw != nil {
				var op map[string]any
				if err := json.Unmarshal(opInfo.raw.Raw, &op); err == nil {
					pathObj[opInfo.name] = op
					collector(op)
				}
			}
		}

		paths[pathKey] = pathObj
	}
	openAPIDoc["paths"] = paths

	// Add schemas if present
	if len(spec.API.Schemas) > 0 {
		schemas := make(map[string]any)
		for schemaKey, schemaRaw := range spec.API.Schemas {
			var schema map[string]any
			if err := json.Unmarshal(schemaRaw.Raw, &schema); err == nil {
				schemas[schemaKey] = schema
			}
		}
		// Add schemas to existing components
		if components, ok := openAPIDoc["components"].(map[string]any); ok {
			components["schemas"] = schemas
		}
	}

	// Marshal to JSON for vacuum
	specBytes, err := json.Marshal(openAPIDoc)
	if err != nil {
		return fmt.Errorf("failed to marshal OpenAPI spec: %w", err)
	}

	// Run vacuum linter
	results, err := linter.LintSpec(specBytes)
	if err != nil {
		return fmt.Errorf("linting error: %w", err)
	}

	// Check for errors in results
	var errorMessages []string
	for _, result := range results {
		// Only fail on errors, not warnings
		if result.Rule.Severity == "error" {
			errorMessages = append(errorMessages, fmt.Sprintf("%s: %s", result.Rule.Name, result.Message))
		}
	}

	if len(errorMessages) > 0 {
		return fmt.Errorf("OpenAPI spec validation errors: %s", strings.Join(errorMessages, "; "))
	}

	return nil
}
