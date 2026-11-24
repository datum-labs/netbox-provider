// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"path/filepath"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	netboxv1alpha1 "go.miloapis.com/netbox-provider/api/v1alpha1"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var (
	cfg        *rest.Config
	k8sClient  client.Client
	testEnv    *envtest.Environment
	testScheme *runtime.Scheme
	ctx        context.Context
	cancel     context.CancelFunc
)

func TestControllers(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Controller Suite")
}

var _ = BeforeSuite(func() {
	ctx, cancel = context.WithCancel(context.TODO())

	By("bootstrapping a test API server with this controller's CRDs and the vendored Fleet inventory CRDs")
	fleetCRDs := envtest.CRDInstallOptions{
		Paths:              []string{filepath.Join("..", "..", "test", "fleet", "crds", "fleet.datumapis.com.yaml")},
		ErrorIfPathMissing: true,
	}
	Expect(envtest.ReadCRDFiles(&fleetCRDs)).To(Succeed())
	preserveDeviceStatus(fleetCRDs.CRDs)
	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "base", "crd", "netbox.miloapis.com.yaml")},
		CRDs:                  fleetCRDs.CRDs,
		ErrorIfCRDPathMissing: true,
	}

	var err error
	cfg, err = testEnv.Start()
	Expect(err).NotTo(HaveOccurred())
	Expect(cfg).NotTo(BeNil())

	testScheme = runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(testScheme))
	utilruntime.Must(netboxv1alpha1.AddToScheme(testScheme))

	k8sClient, err = client.New(cfg, client.Options{Scheme: testScheme})
	Expect(err).NotTo(HaveOccurred())
	Expect(k8sClient).NotTo(BeNil())
})

var _ = AfterSuite(func() {
	cancel()
	By("tearing down the test environment")
	err := testEnv.Stop()
	Expect(err).NotTo(HaveOccurred())
})

// preserveDeviceStatus keeps unknown fields under Device.status. The vendored
// CRD enables Device's status subresource but declares status as an empty
// object, so the API server would prune anything a status writer sends. Fleet
// discovery (datum-cloud/fleet#130) will declare its status schema; until
// then, preserving unknown fields stands in for it, so the envtests can show
// that a status another writer filled survives this controller's applies.
// The vendored file itself stays verbatim.
func preserveDeviceStatus(crds []*apiextensionsv1.CustomResourceDefinition) {
	found := false
	for _, crd := range crds {
		if crd.Spec.Names.Kind != "Device" {
			continue
		}
		for i := range crd.Spec.Versions {
			version := &crd.Spec.Versions[i]
			Expect(version.Subresources).NotTo(BeNil(), "the vendored Device CRD enables the status subresource")
			Expect(version.Subresources.Status).NotTo(BeNil(), "the vendored Device CRD enables the status subresource")
			status := version.Schema.OpenAPIV3Schema.Properties["status"]
			status.XPreserveUnknownFields = ptr.To(true)
			version.Schema.OpenAPIV3Schema.Properties["status"] = status
			found = true
		}
	}
	Expect(found).To(BeTrue(), "the vendored CRDs include Device")
}
