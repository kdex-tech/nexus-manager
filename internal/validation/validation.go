package validation

import (
	"bytes"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
	"kdex.dev/crds/npm"
	"kdex.dev/crds/render"
	kdexresource "kdex.dev/crds/resource"
)

func ValidatePackageReference(
	packageReference *kdexv1alpha1.PackageReference,
	secret *corev1.Secret,
	packageValidatorFactory npm.PackageValidatorFactory,
	defaultRegistry string,
) error {
	registry := packageReference.Registry
	if registry == "" {
		registry = defaultRegistry
	}

	validator, err := packageValidatorFactory(registry, secret)
	if err != nil {
		return err
	}

	return validator.ValidatePackage(
		packageReference.Name,
		packageReference.Version,
	)
}

func ValidateAssets(assets kdexv1alpha1.Assets) error {
	renderer := render.Renderer{}

	_, err := renderer.RenderOne(
		"theme-assets",
		assets.String(),
		render.DefaultTemplateData(),
	)

	return err
}

func ValidateResourceProvider(resourceProvider kdexresource.ResourceProvider) error {
	if resourceProvider.GetResourceImage() == "" {
		for _, url := range resourceProvider.GetResourceURLs() {
			if url != "" && !strings.Contains(url, "://") {
				return fmt.Errorf("%s contains relative url but no image was provided", url)
			}
		}
	}

	if resourceProvider.GetResourceImage() != "" && resourceProvider.GetResourcePath() == "" {
		return fmt.Errorf("ingressPath must be specified when an image is specified")
	}

	if resourceProvider.GetResourceImage() != "" && resourceProvider.GetResourcePath() != "" {
		for _, url := range resourceProvider.GetResourceURLs() {
			if url != "" &&
				!strings.Contains(url, "://") &&
				!strings.HasPrefix(url, resourceProvider.GetResourcePath()) {

				return fmt.Errorf("%s is not prefixed by ingressPath: %s", url, resourceProvider.GetResourcePath())
			}
		}
	}

	return nil
}

func ValidateScriptLibrary(spec *kdexv1alpha1.KDexScriptLibrarySpec) error {
	renderer := render.Renderer{}

	td := render.DefaultTemplateData()

	// validate head scripts
	var buffer bytes.Buffer
	separator := ""

	if spec.PackageReference != nil {
		if spec.PackageReference.Name == "" {
			return fmt.Errorf("package reference name is required")
		}

		if !strings.HasPrefix(spec.PackageReference.Name, "@") || !strings.Contains(spec.PackageReference.Name, "/") {
			return fmt.Errorf("invalid package name, must be scoped with @scope/name: %s", spec.PackageReference.Name)
		}

		buffer.WriteString(spec.PackageReference.ToScriptTag())
		separator = "\n"
	}

	for _, script := range spec.Scripts {
		output := script.ToHeadTag()
		if output != "" {
			buffer.WriteString(separator)
			separator = "\n"
			buffer.WriteString(output)
		}
	}

	if _, err := renderer.RenderOne("head-scripts", buffer.String(), td); err != nil {
		return fmt.Errorf("failed to validate head scripts: %w", err)
	}

	// validate foot scripts
	buffer = bytes.Buffer{}
	separator = ""

	for _, script := range spec.Scripts {
		output := script.ToFootTag()
		if output != "" {
			buffer.WriteString(separator)
			separator = "\n"
			buffer.WriteString(output)
		}
	}

	if _, err := renderer.RenderOne("foot-scripts", buffer.String(), td); err != nil {
		return fmt.Errorf("failed to validate foot scripts: %w", err)
	}

	return nil
}
