/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/scheme"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
	"kdex.dev/crds/configuration"
	kdexlog "kdex.dev/crds/log"
	"kdex.dev/crds/npm"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	"github.com/kdex-tech/nexus-manager/internal/utils"
	// +kubebuilder:scaffold:imports
)

// These tests use Ginkgo (BDD-style Go testing framework). Refer to
// http://onsi.github.io/ginkgo/ to learn more about Ginkgo.

var (
	ctx             context.Context
	cancel          context.CancelFunc
	testEnv         *envtest.Environment
	cfg             *rest.Config
	k8sClient       client.Client
	namespace       string
	secondNamespace string
	hostReconciler  *KDexHostReconciler
	mgrStopped      chan struct{}
)

type MockRegistry struct{}

func (m *MockRegistry) ValidatePackage(packageName string, packageVersion string) error {
	if packageName == "@my-scope/missing" {
		return fmt.Errorf("package not found: %s", packageName)
	}

	return nil
}

func TestControllers(t *testing.T) {
	RegisterFailHandler(Fail)

	// Get the default Ginkgo configuration
	suiteConfig, reporterConfig := GinkgoConfiguration()

	// Enable full stack traces
	reporterConfig.FullTrace = true
	RunSpecs(t, "Controller Suite", suiteConfig, reporterConfig)
}

var _ = BeforeSuite(func() {
	flags := flag.NewFlagSet("dummy-flags", flag.ContinueOnError)
	opts := zap.Options{
		Development: true,
		DestWriter:  GinkgoWriter,
	}
	opts.BindFlags(flags)
	simulatedArgs := []string{
		"--zap-log-level=debug",
		"--zap-encoder=console",
		"--zap-stacktrace-level=error",
	}
	err := flags.Parse(simulatedArgs)
	if err != nil {
		panic(err)
	}

	logger, err := kdexlog.New(&opts, map[string]string{
		"kdexpage": "2",
	})
	if err != nil {
		panic(err)
	}
	logf.SetLogger(logger)

	ctx, cancel = context.WithCancel(context.TODO())

	// +kubebuilder:scaffold:scheme

	By("bootstrapping test environment")
	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{}, // No local CRDs initially
		ErrorIfCRDPathMissing: true,
		WebhookInstallOptions: envtest.WebhookInstallOptions{
			Paths: []string{
				filepath.Join("..", "..", "config", "webhook", "manifests.yaml"),
			},
		},
	}

	crdPath := getCRDPath()
	testEnv.CRDDirectoryPaths = append(testEnv.CRDDirectoryPaths, filepath.Join(crdPath, "config", "crd", "bases"))

	// Retrieve the first found binary directory to allow running tests from IDEs
	if getFirstFoundEnvTestBinaryDir() != "" {
		testEnv.BinaryAssetsDirectory = getFirstFoundEnvTestBinaryDir()
	}

	err = admissionregistrationv1.AddToScheme(scheme.Scheme)
	Expect(err).NotTo(HaveOccurred())

	err = appsv1.AddToScheme(scheme.Scheme)
	Expect(err).NotTo(HaveOccurred())

	err = batchv1.AddToScheme(scheme.Scheme)
	Expect(err).NotTo(HaveOccurred())

	err = configuration.AddToScheme(scheme.Scheme)
	Expect(err).NotTo(HaveOccurred())

	err = corev1.AddToScheme(scheme.Scheme)
	Expect(err).NotTo(HaveOccurred())

	err = kdexv1alpha1.AddToScheme(scheme.Scheme)
	Expect(err).NotTo(HaveOccurred())

	err = rbacv1.AddToScheme(scheme.Scheme)
	Expect(err).NotTo(HaveOccurred())

	// cfg is defined in this file globally.
	cfg, err = testEnv.Start()
	Expect(err).NotTo(HaveOccurred())
	Expect(cfg).NotTo(BeNil())

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme.Scheme})
	Expect(err).NotTo(HaveOccurred())
	Expect(k8sClient).NotTo(BeNil())

	namespace = "default"
	secondNamespace = "second-namespace"

	ns2 := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: secondNamespace}}
	Expect(k8sClient.Create(ctx, ns2)).To(Succeed())

	k8sManager, err := manager.New(cfg, manager.Options{
		Controller: config.Controller{
			Logger: logger,
		},
		Logger: logger,
		Scheme: scheme.Scheme,
		WebhookServer: webhook.NewServer(webhook.Options{
			Port:    testEnv.WebhookInstallOptions.LocalServingPort,
			CertDir: testEnv.WebhookInstallOptions.LocalServingCertDir,
		}),
	})
	Expect(err).NotTo(HaveOccurred())

	packageValidatorFactory := func(registry string, secret *corev1.Secret) (npm.PackageValidator, error) {
		return &MockRegistry{}, nil
	}

	configuration := configuration.LoadConfiguration("/config.yaml", scheme.Scheme)
	Expect(err).NotTo(HaveOccurred())

	// App
	appReconciler := &KDexAppReconciler{
		Client:                  k8sManager.GetClient(),
		PackageValidatorFactory: packageValidatorFactory,
		RequeueDelay:            0,
		Scheme:                  k8sManager.GetScheme(),
	}
	err = appReconciler.SetupWithManager(k8sManager)
	Expect(err).NotTo(HaveOccurred())

	// Host
	hostReconciler = &KDexHostReconciler{
		Client:        k8sManager.GetClient(),
		Ctx:           ctx,
		Configuration: configuration,
		RequeueDelay:  0,
		Scheme:        k8sManager.GetScheme(),
		HelmClientFactory: func(namespace string) (utils.HelmClientInterface, error) {
			return &MockHelmClient{}, nil
		},
	}
	err = hostReconciler.SetupWithManager(k8sManager)
	Expect(err).NotTo(HaveOccurred())

	// Page Archetype
	pageArchetypeReconciler := &KDexPageArchetypeReconciler{
		Client:       k8sClient,
		RequeueDelay: 0,
		Scheme:       k8sClient.Scheme(),
	}
	err = pageArchetypeReconciler.SetupWithManager(k8sManager)
	Expect(err).NotTo(HaveOccurred())

	// Page Binding
	pageReconciler := &KDexPageReconciler{
		Client:       k8sClient,
		RequeueDelay: 0,
		Scheme:       k8sClient.Scheme(),
	}
	err = pageReconciler.SetupWithManager(k8sManager)
	Expect(err).NotTo(HaveOccurred())

	// Page Footer
	pageFooterReconciler := &KDexPageFooterReconciler{
		Client:       k8sClient,
		RequeueDelay: 0,
		Scheme:       k8sClient.Scheme(),
	}
	err = pageFooterReconciler.SetupWithManager(k8sManager)
	Expect(err).ToNot(HaveOccurred())

	// Page Header
	pageHeaderReconciler := &KDexPageHeaderReconciler{
		Client:       k8sClient,
		RequeueDelay: 0,
		Scheme:       k8sClient.Scheme(),
	}
	err = pageHeaderReconciler.SetupWithManager(k8sManager)
	Expect(err).ToNot(HaveOccurred())

	// Page Navigation
	pageNavigationReconciler := &KDexPageNavigationReconciler{
		Client:       k8sClient,
		RequeueDelay: 0,
		Scheme:       k8sClient.Scheme(),
	}
	err = pageNavigationReconciler.SetupWithManager(k8sManager)
	Expect(err).ToNot(HaveOccurred())

	// Script Library
	scriptLibraryReconciler := &KDexScriptLibraryReconciler{
		Client:                  k8sClient,
		PackageValidatorFactory: packageValidatorFactory,
		RequeueDelay:            0,
		Scheme:                  k8sClient.Scheme(),
	}
	err = scriptLibraryReconciler.SetupWithManager(k8sManager)
	Expect(err).ToNot(HaveOccurred())

	// Theme
	themeReconciler := &KDexThemeReconciler{
		Client:       k8sClient,
		RequeueDelay: 0,
		Scheme:       k8sClient.Scheme(),
	}
	err = themeReconciler.SetupWithManager(k8sManager)
	Expect(err).ToNot(HaveOccurred())

	// Translation
	translationReconciler := &KDexTranslationReconciler{
		Client:       k8sClient,
		RequeueDelay: 0,
		Scheme:       k8sClient.Scheme(),
	}
	err = translationReconciler.SetupWithManager(k8sManager)
	Expect(err).ToNot(HaveOccurred())

	// Utility Page
	utilityPageReconciler := &KDexUtilityPageReconciler{
		Client:       k8sClient,
		RequeueDelay: 0,
		Scheme:       k8sClient.Scheme(),
	}
	err = utilityPageReconciler.SetupWithManager(k8sManager)
	Expect(err).ToNot(HaveOccurred())

	// Function
	functionReconciler := &KDexFunctionReconciler{
		Client: k8sClient,
		Scheme: k8sClient.Scheme(),
	}
	err = functionReconciler.SetupWithManager(k8sManager)
	Expect(err).ToNot(HaveOccurred())

	// Create default resources once for all tests
	defaultTranslation := &kdexv1alpha1.KDexClusterTranslation{
		ObjectMeta: metav1.ObjectMeta{
			Name: "kdex-default-translation",
		},
		Spec: kdexv1alpha1.KDexTranslationSpec{
			Translations: []kdexv1alpha1.Translation{
				{
					Lang: "en",
					KeysAndValues: map[string]string{
						"brandName":    "KDex Tech",
						"organization": "KDex Tech Inc.",
					},
				},
			},
		},
	}
	_ = k8sClient.Create(ctx, defaultTranslation)

	defaultArchetype := &kdexv1alpha1.KDexClusterPageArchetype{
		ObjectMeta: metav1.ObjectMeta{
			Name: "kdex-default-page-archetype",
		},
		Spec: kdexv1alpha1.KDexPageArchetypeSpec{
			Content: "[[ .Content.main ]]",
		},
	}
	_ = k8sClient.Create(ctx, defaultArchetype)

	for _, name := range []string{
		"kdex-default-utility-page-announcement",
		"kdex-default-utility-page-error",
		"kdex-default-utility-page-login",
	} {
		page := &kdexv1alpha1.KDexClusterUtilityPage{
			ObjectMeta: metav1.ObjectMeta{
				Name: name,
			},
			Spec: kdexv1alpha1.KDexUtilityPageSpec{
				Type: kdexv1alpha1.AnnouncementUtilityPageType,
				ContentEntries: []kdexv1alpha1.ContentEntry{
					{
						Slot: "main",
						ContentEntryStatic: kdexv1alpha1.ContentEntryStatic{
							RawHTML: "<h1>Announcement</h1>",
						},
					},
				},
				PageArchetypeRef: kdexv1alpha1.KDexObjectReference{
					Kind: "KDexClusterPageArchetype",
					Name: "kdex-default-page-archetype",
				},
			},
		}
		if strings.Contains(name, "error") {
			page.Spec.Type = kdexv1alpha1.ErrorUtilityPageType
			page.Spec.ContentEntries[0].ContentEntryStatic.RawHTML = "<h1>Error</h1>"
		} else if strings.Contains(name, "login") {
			page.Spec.Type = kdexv1alpha1.LoginUtilityPageType
			page.Spec.ContentEntries[0].ContentEntryStatic.RawHTML = "<h1>Login</h1>"
		}
		_ = k8sClient.Create(ctx, page)
	}

	mgrStopped = make(chan struct{})
	go func() {
		defer GinkgoRecover()
		defer close(mgrStopped)
		err := k8sManager.Start(ctx)
		Expect(err).ToNot(HaveOccurred(), "failed to run manager")
	}()

	// Wait for cache to sync and resources to be available in the manager's cache
	By("waiting for cache to sync")
	Eventually(func() error {
		return k8sManager.GetClient().Get(ctx, types.NamespacedName{Name: "kdex-default-translation"}, &kdexv1alpha1.KDexClusterTranslation{})
	}, "10s", "1s").Should(Succeed())
})

var _ = AfterSuite(func() {
	By("tearing down the test environment")
	cancel()
	if mgrStopped != nil {
		Eventually(mgrStopped, "30s").Should(BeClosed())
	}
	err := testEnv.Stop()
	Expect(err).NotTo(HaveOccurred())
})

func getCRDPath() string {
	cmd := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "kdex.dev/crds")
	out, err := cmd.Output()
	if err != nil {
		panic(fmt.Errorf("failed to get crd module path: %w", err))
	}
	return strings.TrimSpace(string(out))
}

// getFirstFoundEnvTestBinaryDir locates the first binary in the specified path.
// ENVTEST-based tests depend on specific binaries, usually located in paths set by
// controller-runtime. When running tests directly (e.g., via an IDE) without using
// Makefile targets, the 'BinaryAssetsDirectory' must be explicitly configured.
//
// This function streamlines the process by finding the required binaries, similar to
// setting the 'KUBEBUILDER_ASSETS' environment variable. To ensure the binaries are
// properly set up, run 'make setup-envtest' beforehand.
func getFirstFoundEnvTestBinaryDir() string {
	basePath := filepath.Join("..", "..", "bin", "k8s")
	entries, err := os.ReadDir(basePath)
	if err != nil {
		logf.Log.Error(err, "Failed to read directory", "path", basePath)
		return ""
	}
	for _, entry := range entries {
		if entry.IsDir() {
			return filepath.Join(basePath, entry.Name())
		}
	}
	return ""
}
