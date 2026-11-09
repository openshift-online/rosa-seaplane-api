package render

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	k8svalidation "k8s.io/apimachinery/pkg/util/validation"

	seaplanev1alpha1 "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1"
	configv1 "github.com/openshift/api/config/v1"
	hypershiftv1beta1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

func testCluster() *seaplanev1alpha1.Cluster {
	return &seaplanev1alpha1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-cluster",
			Namespace: "account-123456789012",
			UID:       "abc12345",
		},
		Spec: seaplanev1alpha1.ClusterSpec{
			CreatorARN: "arn:aws:iam::123456789012:user/admin",
			HostedCluster: seaplanev1alpha1.HostedClusterSpecPassthrough{
				Release:   hypershiftv1beta1.Release{Image: "quay.io/ocp:4.17"},
				IssuerURL: "https://oidc.example.com/abc12345",
				Configuration: &hypershiftv1beta1.ClusterConfiguration{
					Proxy: &configv1.ProxySpec{
						HTTPProxy:  "http://proxy.example.com:8080",
						HTTPSProxy: "https://proxy.example.com:8443",
						NoProxy:    "localhost,127.0.0.1",
					},
				},
				Networking: hypershiftv1beta1.ClusterNetworking{
					ClusterNetwork: []hypershiftv1beta1.ClusterNetworkEntry{{CIDR: mustParseCIDR("10.128.0.0/14")}},
					ServiceNetwork: []hypershiftv1beta1.ServiceNetworkEntry{{CIDR: mustParseCIDR("172.30.0.0/16")}},
					MachineNetwork: []hypershiftv1beta1.MachineNetworkEntry{{CIDR: mustParseCIDR("10.0.0.0/16")}},
				},
				Platform: hypershiftv1beta1.PlatformSpec{
					Type: hypershiftv1beta1.AWSPlatform,
					AWS: &hypershiftv1beta1.AWSPlatformSpec{
						Region: "us-east-1",
						CloudProviderConfig: &hypershiftv1beta1.AWSCloudProviderConfig{
							VPC:  "vpc-abc",
							Zone: "us-east-1a",
							Subnet: &hypershiftv1beta1.AWSResourceReference{
								ID: ptr.To("subnet-1"),
							},
						},
						RolesRef: hypershiftv1beta1.AWSRolesRef{
							ControlPlaneOperatorARN: "arn:cpo",
							IngressARN:              "arn:ingress",
							ImageRegistryARN:        "arn:registry",
							KubeCloudControllerARN:  "arn:kccm",
							NodePoolManagementARN:   "arn:npm",
							NetworkARN:              "arn:network",
							StorageARN:              "arn:storage",
						},
					},
				},
			},
		},
	}
}

func TestClusterResourcesPreservesProxyConfiguration(t *testing.T) {
	resources, err := ClusterResources(testCluster(), false, "f7a3.0.example.com", "")
	if err != nil {
		t.Fatalf("ClusterResources: %v", err)
	}

	var hostedCluster *hypershiftv1beta1.HostedCluster
	for _, resource := range resources {
		if resource.Resource == "hostedclusters" {
			hostedCluster = resource.Object.(*hypershiftv1beta1.HostedCluster)
			break
		}
	}
	if hostedCluster == nil {
		t.Fatal("no hostedcluster resource found")
	}
	if hostedCluster.Spec.Configuration == nil || hostedCluster.Spec.Configuration.Proxy == nil {
		t.Fatal("expected proxy configuration to be preserved")
	}

	proxy := hostedCluster.Spec.Configuration.Proxy
	if proxy.HTTPProxy != "http://proxy.example.com:8080" ||
		proxy.HTTPSProxy != "https://proxy.example.com:8443" ||
		proxy.NoProxy != "localhost,127.0.0.1" {
		t.Errorf("proxy configuration was not preserved: %+v", proxy)
	}
}

// testClusterWithOidcConfig returns a cluster fixture using the
// OidcConfig-backed issuer path (OidcConfigID set).
func testClusterWithOidcConfig() *seaplanev1alpha1.Cluster {
	c := testCluster()
	c.Spec.OidcConfigID = "test-oidc-config"
	c.Spec.AccountID = "123456789012"
	return c
}

func TestClusterResourcesCount(t *testing.T) {
	resources, err := ClusterResources(testCluster(), false, "f7a3.0.example.com", "")
	if err != nil {
		t.Fatalf("ClusterResources: %v", err)
	}
	if got := len(resources); got != 6 {
		t.Errorf("expected 6 resources, got %d", got)
	}
}

func TestClusterResourcesTypes(t *testing.T) {
	resources, err := ClusterResources(testCluster(), false, "f7a3.0.example.com", "")
	if err != nil {
		t.Fatalf("ClusterResources: %v", err)
	}

	expected := []struct {
		resource string
		name     string
	}{
		{"namespaces", "cluster-abc12345"},
		{"configmaps", "cluster-config"},
		{"externalsecrets", "pull-secret"},
		{"certificates", "api-serving-cert"},
		{"hostedclusters", HostedClusterName(testCluster())},
		{"secrets", "ssh-key"},
	}

	for i, e := range expected {
		if resources[i].Resource != e.resource {
			t.Errorf("resource[%d]: expected resource %q, got %q", i, e.resource, resources[i].Resource)
		}
		if resources[i].Name != e.name {
			t.Errorf("resource[%d]: expected name %q, got %q", i, e.name, resources[i].Name)
		}
	}
}

func TestClusterResourcesDecoupleHostedClusterNameFromCustomerName(t *testing.T) {
	cluster := testCluster()
	cluster.UID = "550e8400-e29b-41d4-a716-446655440000"
	cluster.Name = "customer-name-longer-than-18-characters"
	cluster.Spec.HostedClusterName = seaplanev1alpha1.HostedClusterNameFromDNSReservation(&seaplanev1alpha1.DNSReservation{
		ObjectMeta: metav1.ObjectMeta{UID: "550e8400-e29b-41d4-a716-446655440000"},
	})
	resources, err := ClusterResources(cluster, false, "f7a3.0.example.com", "")
	if err != nil {
		t.Fatalf("ClusterResources: %v", err)
	}

	var hostedCluster *hypershiftv1beta1.HostedCluster
	var clusterConfig *corev1.ConfigMap
	var servingCert *Certificate
	for _, resource := range resources {
		switch resource.Resource {
		case "hostedclusters":
			if resource.Name != HostedClusterName(cluster) {
				t.Errorf("HostedCluster resource name = %q, want %q", resource.Name, HostedClusterName(cluster))
			}
			hostedCluster = resource.Object.(*hypershiftv1beta1.HostedCluster)
		case "configmaps":
			if resource.Name == "cluster-config" {
				clusterConfig = resource.Object.(*corev1.ConfigMap)
			}
		case "certificates":
			if resource.Name == "api-serving-cert" {
				servingCert = resource.Object.(*Certificate)
			}
		}
	}
	if hostedCluster == nil {
		t.Fatal("rendered resources do not include a HostedCluster")
	}
	if hostedCluster.Name != HostedClusterName(cluster) {
		t.Errorf("HostedCluster metadata.name = %q, want %q", hostedCluster.Name, HostedClusterName(cluster))
	}
	hcpNamespace := ManagementNamespace(string(cluster.UID)) + "-" + hostedCluster.Name
	if len(hcpNamespace) != k8svalidation.DNS1123LabelMaxLength {
		t.Errorf("HostedControlPlane namespace length = %d, want %d", len(hcpNamespace), k8svalidation.DNS1123LabelMaxLength)
	}
	if len(hcpNamespace) > k8svalidation.DNS1123LabelMaxLength {
		t.Errorf("HostedControlPlane namespace %q is %d chars; max is %d", hcpNamespace, len(hcpNamespace), k8svalidation.DNS1123LabelMaxLength)
	}
	if problems := k8svalidation.IsDNS1123Label(hcpNamespace); len(problems) > 0 {
		t.Errorf("HostedControlPlane namespace %q is not a DNS label: %v", hcpNamespace, problems)
	}
	wantAPIHost := "api." + HostedClusterName(cluster) + ".f7a3.0.example.com"
	if got := hostedCluster.Spec.KubeAPIServerDNSName; got != wantAPIHost {
		t.Errorf("KubeAPIServerDNSName = %q, want HostedCluster name %q", got, wantAPIHost)
	}
	var apiRouteHostname, oauthRouteHostname string
	for _, service := range hostedCluster.Spec.Services {
		if service.Route == nil {
			continue
		}
		switch service.Service {
		case hypershiftv1beta1.APIServer:
			apiRouteHostname = service.Route.Hostname
		case hypershiftv1beta1.OAuthServer:
			oauthRouteHostname = service.Route.Hostname
		}
	}
	if apiRouteHostname != wantAPIHost {
		t.Errorf("API Route hostname = %q, want %q", apiRouteHostname, wantAPIHost)
	}
	wantOAuthHost := "oauth." + HostedClusterName(cluster) + ".f7a3.0.example.com"
	if oauthRouteHostname != wantOAuthHost {
		t.Errorf("OAuth Route hostname = %q, want HostedCluster name %q", oauthRouteHostname, wantOAuthHost)
	}
	if servingCert == nil {
		t.Fatal("rendered resources do not include the API serving Certificate")
	}
	wantCertDNSName := "*." + HostedClusterName(cluster) + ".f7a3.0.example.com"
	if len(servingCert.Spec.DNSNames) != 1 || servingCert.Spec.DNSNames[0] != wantCertDNSName {
		t.Errorf("API serving cert DNSNames = %v, want [%q]", servingCert.Spec.DNSNames, wantCertDNSName)
	}
	if clusterConfig == nil || clusterConfig.Data["cluster_name"] != cluster.Name {
		t.Errorf("cluster-config cluster_name = %v, want customer-facing name %q", clusterConfig, cluster.Name)
	}
}

func TestLongCustomerNameUsesHostedClusterNameForAPIOAuthAndCertificateDNS(t *testing.T) {
	reservation := &seaplanev1alpha1.DNSReservation{
		ObjectMeta: metav1.ObjectMeta{UID: "550e8400-e29b-41d4-a716-446655440000"},
	}
	cluster := testCluster()
	cluster.UID = "12345678-1234-1234-1234-123456789abc"
	cluster.Name = "customer-name-longer-than-18-characters"
	cluster.Spec.DNSReservationID = string(reservation.UID)
	cluster.Spec.HostedClusterName = seaplanev1alpha1.HostedClusterNameFromDNSReservation(reservation)

	resources, err := ClusterResources(cluster, false, "f7a3.0.example.com", "")
	if err != nil {
		t.Fatalf("ClusterResources: %v", err)
	}

	var hostedCluster *hypershiftv1beta1.HostedCluster
	var servingCert *Certificate
	for _, resource := range resources {
		switch resource.Resource {
		case "hostedclusters":
			hostedCluster = resource.Object.(*hypershiftv1beta1.HostedCluster)
		case "certificates":
			if resource.Name == "api-serving-cert" {
				servingCert = resource.Object.(*Certificate)
			}
		}
	}
	if hostedCluster == nil || servingCert == nil {
		t.Fatalf("rendered HostedCluster or API serving Certificate is missing: HostedCluster=%v Certificate=%v", hostedCluster != nil, servingCert != nil)
	}

	hostedName := seaplanev1alpha1.HostedClusterNameFromDNSReservation(reservation)
	wantAPIHost := "api." + hostedName + ".f7a3.0.example.com"
	wantOAuthHost := "oauth." + hostedName + ".f7a3.0.example.com"
	if got := hostedCluster.Spec.KubeAPIServerDNSName; got != wantAPIHost {
		t.Errorf("KubeAPIServerDNSName = %q, want %q", got, wantAPIHost)
	}
	var apiRouteHostname, oauthRouteHostname string
	for _, service := range hostedCluster.Spec.Services {
		if service.Route == nil {
			continue
		}
		switch service.Service {
		case hypershiftv1beta1.APIServer:
			apiRouteHostname = service.Route.Hostname
		case hypershiftv1beta1.OAuthServer:
			oauthRouteHostname = service.Route.Hostname
		}
	}
	if apiRouteHostname != wantAPIHost {
		t.Errorf("API Route hostname = %q, want %q", apiRouteHostname, wantAPIHost)
	}
	if oauthRouteHostname != wantOAuthHost {
		t.Errorf("OAuth Route hostname = %q, want %q", oauthRouteHostname, wantOAuthHost)
	}
	wantCertDNSName := "*." + hostedName + ".f7a3.0.example.com"
	if len(servingCert.Spec.DNSNames) != 1 || servingCert.Spec.DNSNames[0] != wantCertDNSName {
		t.Errorf("API serving cert DNSNames = %v, want [%q]", servingCert.Spec.DNSNames, wantCertDNSName)
	}
}

func TestClusterResourcesRenderIntoUIDManagementNamespace(t *testing.T) {
	cluster := testCluster()
	resources, err := ClusterResources(cluster, false, "f7a3.0.example.com", "")
	if err != nil {
		t.Fatalf("ClusterResources: %v", err)
	}
	wantNamespace := ManagementNamespace(string(cluster.UID))
	for _, resource := range resources {
		if resource.Resource == "namespaces" {
			namespace := resource.Object.(*corev1.Namespace)
			if namespace.Name != wantNamespace {
				t.Errorf("rendered namespace name = %q, want %q", namespace.Name, wantNamespace)
			}
			continue
		}
		if resource.Namespace != wantNamespace {
			t.Errorf("resource %s/%s namespace = %q, want %q", resource.Resource, resource.Name, resource.Namespace, wantNamespace)
		}
	}
}

// TestClusterResourcesWithOidcConfig verifies the OIDC signing key
// ExternalSecret and ServiceAccountSigningKey reference are rendered when the
// referenced OidcConfig is type=unmanaged (oidcSigningKeyExternal=true).
func TestClusterResourcesWithOidcConfig(t *testing.T) {
	resources, err := ClusterResources(testClusterWithOidcConfig(), true, "f7a3.0.example.com", "")
	if err != nil {
		t.Fatalf("ClusterResources: %v", err)
	}
	if got := len(resources); got != 7 {
		t.Fatalf("expected 7 resources, got %d", got)
	}

	last := resources[len(resources)-1]
	if last.Resource != "externalsecrets" || last.Name != "oidc-signing-key" {
		t.Errorf("expected last resource to be externalsecrets/oidc-signing-key, got %s/%s", last.Resource, last.Name)
	}
	es, ok := last.Object.(*ExternalSecret)
	if !ok {
		t.Fatalf("expected *ExternalSecret, got %T", last.Object)
	}
	wantPath := "/seaplane/oidc/123456789012/test-oidc-config/signing-key"
	if len(es.Spec.Data) != 1 || es.Spec.Data[0].RemoteRef.Key != wantPath {
		t.Errorf("expected remote ref key %q, got %+v", wantPath, es.Spec.Data)
	}

	var hc *hypershiftv1beta1.HostedCluster
	for _, r := range resources {
		if r.Resource == "hostedclusters" {
			hc = r.Object.(*hypershiftv1beta1.HostedCluster)
			break
		}
	}
	if hc == nil {
		t.Fatal("no hostedcluster resource found")
	}
	if hc.Spec.ServiceAccountSigningKey == nil || hc.Spec.ServiceAccountSigningKey.Name != "oidc-signing-key" {
		t.Errorf("expected ServiceAccountSigningKey to reference oidc-signing-key, got %+v", hc.Spec.ServiceAccountSigningKey)
	}
}

// TestClusterResourcesWithoutOidcConfig_NoExternalSecret verifies the legacy
// path renders no OIDC signing key ExternalSecret.
func TestClusterResourcesWithoutOidcConfig_NoExternalSecret(t *testing.T) {
	resources, err := ClusterResources(testCluster(), false, "f7a3.0.example.com", "")
	if err != nil {
		t.Fatalf("ClusterResources: %v", err)
	}
	for _, r := range resources {
		if r.Resource == "externalsecrets" && r.Name == "oidc-signing-key" {
			t.Error("expected no oidc-signing-key ExternalSecret without OidcConfigID")
		}
	}

	var hc *hypershiftv1beta1.HostedCluster
	for _, r := range resources {
		if r.Resource == "hostedclusters" {
			hc = r.Object.(*hypershiftv1beta1.HostedCluster)
			break
		}
	}
	if hc == nil {
		t.Fatal("no hostedcluster resource found")
	}
	if hc.Spec.ServiceAccountSigningKey != nil {
		t.Errorf("expected ServiceAccountSigningKey to be nil, got %+v", hc.Spec.ServiceAccountSigningKey)
	}
}

// TestClusterResourcesWithManagedOidcConfig_NoExternalSecret verifies that a
// managed OidcConfig (OidcConfigID set, oidcSigningKeyExternal=false) renders
// no ExternalSecret/ServiceAccountSigningKey, since managed configs don't
// store a signing key in Secrets Manager for ESO to deliver.
func TestClusterResourcesWithManagedOidcConfig_NoExternalSecret(t *testing.T) {
	resources, err := ClusterResources(testClusterWithOidcConfig(), false, "f7a3.0.example.com", "")
	if err != nil {
		t.Fatalf("ClusterResources: %v", err)
	}
	for _, r := range resources {
		if r.Resource == "externalsecrets" && r.Name == "oidc-signing-key" {
			t.Error("expected no oidc-signing-key ExternalSecret for a managed OidcConfig")
		}
	}

	var hc *hypershiftv1beta1.HostedCluster
	for _, r := range resources {
		if r.Resource == "hostedclusters" {
			hc = r.Object.(*hypershiftv1beta1.HostedCluster)
			break
		}
	}
	if hc == nil {
		t.Fatal("no hostedcluster resource found")
	}
	if hc.Spec.ServiceAccountSigningKey != nil {
		t.Errorf("expected ServiceAccountSigningKey to be nil for a managed OidcConfig, got %+v", hc.Spec.ServiceAccountSigningKey)
	}
}

// TestClusterResourcesClearsStaleServiceAccountSigningKey verifies that a
// ServiceAccountSigningKey already present on the Cluster CR's spec (e.g. a
// stale value from a prior generation) is cleared when oidcSigningKeyExternal
// is false, rather than passed through to the rendered HostedCluster.
func TestClusterResourcesClearsStaleServiceAccountSigningKey(t *testing.T) {
	cluster := testCluster()
	cluster.Spec.HostedCluster.ServiceAccountSigningKey = &corev1.LocalObjectReference{Name: "stale-key"}

	resources, err := ClusterResources(cluster, false, "f7a3.0.example.com", "")
	if err != nil {
		t.Fatalf("ClusterResources: %v", err)
	}

	var hc *hypershiftv1beta1.HostedCluster
	for _, r := range resources {
		if r.Resource == "hostedclusters" {
			hc = r.Object.(*hypershiftv1beta1.HostedCluster)
			break
		}
	}
	if hc == nil {
		t.Fatal("no hostedcluster resource found")
	}
	if hc.Spec.ServiceAccountSigningKey != nil {
		t.Errorf("expected stale ServiceAccountSigningKey to be cleared, got %+v", hc.Spec.ServiceAccountSigningKey)
	}
}

func TestHostedClusterDNS(t *testing.T) {
	resources, err := ClusterResources(testCluster(), false, "f7a3.0.example.com", "")
	if err != nil {
		t.Fatalf("ClusterResources: %v", err)
	}

	var hc *hypershiftv1beta1.HostedCluster
	var servingCert *Certificate
	for _, m := range resources {
		switch m.Resource {
		case "hostedclusters":
			hc = m.Object.(*hypershiftv1beta1.HostedCluster)
		case "certificates":
			if m.Name == "api-serving-cert" {
				servingCert = m.Object.(*Certificate)
			}
		}
	}
	if hc == nil {
		t.Fatal("no hostedcluster resource found")
	}

	if got := hc.Spec.DNS.BaseDomain; got != "f7a3.0.example.com" {
		t.Errorf("dns.baseDomain = %q, want %q", got, "f7a3.0.example.com")
	}

	if want := "api.my-cluster.f7a3.0.example.com"; hc.Spec.KubeAPIServerDNSName != want {
		t.Errorf("kubeAPIServerDNSName = %q, want %q", hc.Spec.KubeAPIServerDNSName, want)
	}
	var apiRouteHostname, oauthRouteHostname string
	for _, service := range hc.Spec.Services {
		if service.Route == nil {
			continue
		}
		switch service.Service {
		case hypershiftv1beta1.APIServer:
			apiRouteHostname = service.Route.Hostname
		case hypershiftv1beta1.OAuthServer:
			oauthRouteHostname = service.Route.Hostname
		}
	}
	if apiRouteHostname != "api.my-cluster.f7a3.0.example.com" {
		t.Errorf("API Route hostname = %q, want customer-name fallback", apiRouteHostname)
	}
	if oauthRouteHostname != "oauth.my-cluster.f7a3.0.example.com" {
		t.Errorf("OAuth Route hostname = %q, want customer-name fallback", oauthRouteHostname)
	}
	if servingCert == nil {
		t.Fatal("rendered resources do not include the API serving Certificate")
	}
	if len(servingCert.Spec.DNSNames) != 1 || servingCert.Spec.DNSNames[0] != "*.my-cluster.f7a3.0.example.com" {
		t.Errorf("API serving cert DNSNames = %v, want customer-name wildcard", servingCert.Spec.DNSNames)
	}

	if got := hc.Spec.IssuerURL; got != "https://oidc.example.com/abc12345" {
		t.Errorf("issuerURL = %q, want %q", got, "https://oidc.example.com/abc12345")
	}

	if got := hc.Spec.Release.Image; got != "quay.io/ocp:4.17" {
		t.Errorf("release.image = %q, want %q", got, "quay.io/ocp:4.17")
	}

	// testCluster() has no OidcConfigID, so InfraID defaults to clusterID.
	if got := hc.Spec.InfraID; got != "abc12345" {
		t.Errorf("infraID = %q, want %q (clusterID)", got, "abc12345")
	}
}

func TestHostedClusterDNSClearsZoneIDs(t *testing.T) {
	cluster := testCluster()
	cluster.Spec.HostedCluster.DNS.PublicZoneID = "Z0123456789PUBLIC"
	cluster.Spec.HostedCluster.DNS.PrivateZoneID = "Z0123456789PRIVATE"

	resources, err := ClusterResources(cluster, false, "f7a3.0.example.com", "")
	if err != nil {
		t.Fatalf("ClusterResources: %v", err)
	}

	var hc *hypershiftv1beta1.HostedCluster
	for _, resource := range resources {
		if resource.Resource == "hostedclusters" {
			hc = resource.Object.(*hypershiftv1beta1.HostedCluster)
			break
		}
	}
	if hc == nil {
		t.Fatal("no hostedcluster resource found")
	}
	if hc.Spec.DNS.PublicZoneID != "" || hc.Spec.DNS.PrivateZoneID != "" {
		t.Fatalf("rendered DNS zone IDs = (%q, %q), want empty values", hc.Spec.DNS.PublicZoneID, hc.Spec.DNS.PrivateZoneID)
	}
}

// TestHostedClusterInfraIDManagedOidcConfig verifies InfraID is derived from issuerURL's trailing
// path segment for a managed OidcConfig, since that's the S3 key HyperShift uploads OIDC
// discovery docs to.
func TestHostedClusterInfraIDManagedOidcConfig(t *testing.T) {
	cluster := testClusterWithOidcConfig()
	cluster.Spec.HostedCluster.IssuerURL = "https://oidc.example.com/2130539814aa400396a3f3b860e04a1c"

	resources, err := ClusterResources(cluster, false, "f7a3.0.example.com", "")
	if err != nil {
		t.Fatalf("ClusterResources: %v", err)
	}
	hc := hostedClusterFrom(t, resources)

	want := "2130539814aa400396a3f3b860e04a1c"
	if got := hc.Spec.InfraID; got != want {
		t.Errorf("infraID = %q, want %q", got, want)
	}
}

// TestHostedClusterInfraIDUnmanagedOidcConfig verifies InfraID stays clusterID for an unmanaged
// OidcConfig, regardless of the customer-supplied issuerURL's shape.
func TestHostedClusterInfraIDUnmanagedOidcConfig(t *testing.T) {
	cluster := testClusterWithOidcConfig()
	cluster.Spec.HostedCluster.IssuerURL = "https://customer-idp.example.com/some/arbitrary/path"

	resources, err := ClusterResources(cluster, true, "f7a3.0.example.com", "")
	if err != nil {
		t.Fatalf("ClusterResources: %v", err)
	}
	hc := hostedClusterFrom(t, resources)

	const clusterID = "abc12345" // from namespace "cluster-abc12345"
	if got := hc.Spec.InfraID; got != clusterID {
		t.Errorf("infraID = %q, want %q", got, clusterID)
	}
}

// hostedClusterFrom returns the rendered HostedCluster from a resource slice.
func hostedClusterFrom(t *testing.T, resources []Resource) *hypershiftv1beta1.HostedCluster {
	t.Helper()
	for _, r := range resources {
		if r.Resource == "hostedclusters" {
			return r.Object.(*hypershiftv1beta1.HostedCluster)
		}
	}
	t.Fatal("no hostedcluster resource found")
	return nil
}

// renderHostedCluster renders cluster and returns its HostedCluster, failing the
// test if one was not produced.
func renderHostedCluster(t *testing.T, cluster *seaplanev1alpha1.Cluster) *hypershiftv1beta1.HostedCluster {
	t.Helper()
	resources, err := ClusterResources(cluster, false, "f7a3.0.example.com", "")
	if err != nil {
		t.Fatalf("ClusterResources: %v", err)
	}
	return hostedClusterFrom(t, resources)
}

// TestHostedClusterControlPlaneOperatorImageAnnotation verifies the CPO image
// override is stamped as an annotation when set, and omitted when empty.
func TestHostedClusterControlPlaneOperatorImageAnnotation(t *testing.T) {
	const cpoImage = "quay.io/me/hypershift:pr-1234"

	resources, err := ClusterResources(testCluster(), false, "f7a3.0.example.com", cpoImage)
	if err != nil {
		t.Fatalf("ClusterResources: %v", err)
	}
	hc := hostedClusterFrom(t, resources)
	if got := hc.Annotations[hypershiftv1beta1.ControlPlaneOperatorImageAnnotation]; got != cpoImage {
		t.Errorf("CPO annotation = %q, want %q", got, cpoImage)
	}

	resources, err = ClusterResources(testCluster(), false, "f7a3.0.example.com", "")
	if err != nil {
		t.Fatalf("ClusterResources: %v", err)
	}
	hc = hostedClusterFrom(t, resources)
	if _, ok := hc.Annotations[hypershiftv1beta1.ControlPlaneOperatorImageAnnotation]; ok {
		t.Errorf("CPO annotation should be absent when override is empty, got %q",
			hc.Annotations[hypershiftv1beta1.ControlPlaneOperatorImageAnnotation])
	}
}

func tagPairs(tags []hypershiftv1beta1.AWSResourceTag) map[string]string {
	out := make(map[string]string, len(tags))
	for _, tag := range tags {
		out[tag.Key] = tag.Value
	}
	return out
}

func TestHostedClusterSystemTagsOnly(t *testing.T) {
	hc := renderHostedCluster(t, testCluster())

	got := tagPairs(hc.Spec.Platform.AWS.ResourceTags)
	if len(got) != 2 {
		t.Fatalf("resourceTags = %v, want only the 2 system tags", got)
	}
	if got["red-hat-managed"] != "true" {
		t.Errorf("red-hat-managed = %q, want %q", got["red-hat-managed"], "true")
	}
	if got["kubernetes.io/cluster/abc12345"] != "owned" {
		t.Errorf("kubernetes.io/cluster/abc12345 = %q, want %q", got["kubernetes.io/cluster/abc12345"], "owned")
	}
}

func TestHostedClusterCustomerTags(t *testing.T) {
	cluster := testCluster()
	cluster.Spec.Tags = map[string]string{
		"cost-center": "cc-1234",
		"environment": "production",
	}

	hc := renderHostedCluster(t, cluster)

	got := tagPairs(hc.Spec.Platform.AWS.ResourceTags)
	if got["cost-center"] != "cc-1234" {
		t.Errorf("cost-center = %q, want %q", got["cost-center"], "cc-1234")
	}
	if got["environment"] != "production" {
		t.Errorf("environment = %q, want %q", got["environment"], "production")
	}
	// Customer tags must not displace the system tags.
	if got["red-hat-managed"] != "true" {
		t.Errorf("red-hat-managed = %q, want %q", got["red-hat-managed"], "true")
	}
	if got["kubernetes.io/cluster/abc12345"] != "owned" {
		t.Errorf("kubernetes.io/cluster/abc12345 = %q, want %q", got["kubernetes.io/cluster/abc12345"], "owned")
	}
}

// A customer must not be able to overwrite the tags the platform relies on for
// ownership and billing attribution.
func TestHostedClusterCustomerTagsCannotShadowSystemTags(t *testing.T) {
	cluster := testCluster()
	cluster.Spec.Tags = map[string]string{
		"red-hat-managed":                "false",
		"kubernetes.io/cluster/abc12345": "shared",
	}

	hc := renderHostedCluster(t, cluster)

	tags := hc.Spec.Platform.AWS.ResourceTags
	if len(tags) != 2 {
		t.Fatalf("resourceTags = %v, want 2 entries with no duplicate keys", tags)
	}
	got := tagPairs(tags)
	if got["red-hat-managed"] != "true" {
		t.Errorf("red-hat-managed = %q, want %q", got["red-hat-managed"], "true")
	}
	if got["kubernetes.io/cluster/abc12345"] != "owned" {
		t.Errorf("kubernetes.io/cluster/abc12345 = %q, want %q", got["kubernetes.io/cluster/abc12345"], "owned")
	}
}

// Map iteration order is random, so renders of the same Cluster must still
// produce an identical tag slice or the operator would churn the HostedCluster.
func TestHostedClusterCustomerTagsDeterministicOrder(t *testing.T) {
	cluster := testCluster()
	cluster.Spec.Tags = map[string]string{
		"delta": "4", "alpha": "1", "charlie": "3", "bravo": "2", "echo": "5",
	}

	first := renderHostedCluster(t, cluster).Spec.Platform.AWS.ResourceTags
	for i := range 10 {
		got := renderHostedCluster(t, cluster).Spec.Platform.AWS.ResourceTags
		if !slices.Equal(got, first) {
			t.Fatalf("render %d produced %v, want %v", i, got, first)
		}
	}

	// System tags first, then customer tags sorted by key.
	want := []string{
		"red-hat-managed", "kubernetes.io/cluster/abc12345",
		"alpha", "bravo", "charlie", "delta", "echo",
	}
	for i, key := range want {
		if first[i].Key != key {
			t.Errorf("resourceTags[%d].Key = %q, want %q", i, first[i].Key, key)
		}
	}
}

// Root-level Cluster tags are the customer source of truth; direct HostedCluster
// resourceTags passthrough values are not rendered.
func TestHostedClusterUsesRootTagsInsteadOfPlatformPassthrough(t *testing.T) {
	cluster := testCluster()
	cluster.Spec.HostedCluster.Platform.AWS.ResourceTags = []hypershiftv1beta1.AWSResourceTag{
		{Key: "environment", Value: "passthrough"},
		{Key: "legacy", Value: "ignored"},
	}
	cluster.Spec.Tags = map[string]string{"environment": "root", "owner": "team"}

	hc := renderHostedCluster(t, cluster)

	tags := hc.Spec.Platform.AWS.ResourceTags
	if len(tags) != 4 {
		t.Fatalf("resourceTags = %v, want system tags and root-level customer tags only", tags)
	}
	got := tagPairs(tags)
	if got["environment"] != "root" {
		t.Errorf("environment = %q, want root-level value %q", got["environment"], "root")
	}
	if got["owner"] != "team" {
		t.Errorf("owner = %q, want root-level value %q", got["owner"], "team")
	}
	if _, exists := got["legacy"]; exists {
		t.Errorf("legacy passthrough tag should not be rendered: %v", got)
	}
}

func TestHostedClusterIgnoresPassthroughSystemTags(t *testing.T) {
	cluster := testCluster()
	cluster.Spec.HostedCluster.Platform.AWS.ResourceTags = []hypershiftv1beta1.AWSResourceTag{
		{Key: "red-hat-managed", Value: "false"},
		{Key: "kubernetes.io/cluster/abc12345", Value: "shared"},
	}

	hc := renderHostedCluster(t, cluster)
	tags := hc.Spec.Platform.AWS.ResourceTags
	if len(tags) != 2 {
		t.Fatalf("resourceTags = %v, want system tags only", tags)
	}
	got := tagPairs(tags)
	if got["red-hat-managed"] != "true" {
		t.Errorf("reserved system tag value = %q, want true", got["red-hat-managed"])
	}
	if got["kubernetes.io/cluster/abc12345"] != "owned" {
		t.Errorf("cluster ownership tag value = %q, want owned", got["kubernetes.io/cluster/abc12345"])
	}
}

func TestRegistryAdditionalCANormalizesHostnames(t *testing.T) {
	caCerts := map[string]string{
		"registry.example.com":      "cert1",
		"registry.example.com:5000": "cert2",
		"localhost:8080":            "cert3",
	}

	resource := registryAdditionalCA("test-cluster-id", "test-namespace", caCerts)

	cm, ok := resource.Object.(*corev1.ConfigMap)
	if !ok {
		t.Fatalf("expected *corev1.ConfigMap, got %T", resource.Object)
	}

	expectedKeys := []string{
		"registry.example.com",
		"registry.example.com..5000",
		"localhost..8080",
	}

	if len(cm.Data) != len(expectedKeys) {
		t.Fatalf("expected %d keys in ConfigMap data, got %d", len(expectedKeys), len(cm.Data))
	}

	for _, key := range expectedKeys {
		if _, ok := cm.Data[key]; !ok {
			t.Errorf("expected ConfigMap data to contain key %q, but it was missing", key)
		}
	}

	// Verify original keys are NOT present (they should be normalized)
	for originalKey := range caCerts {
		if strings.Contains(originalKey, ":") {
			if _, ok := cm.Data[originalKey]; ok {
				t.Errorf("ConfigMap data should not contain unnormalized key %q", originalKey)
			}
		}
	}
}

func TestClusterResourcesPreservesRegistryConfiguration(t *testing.T) {
	for _, imageJSON := range []string{
		`{}`,
		`{"registrySources":{"allowedRegistries":["quay.io","registry.redhat.io"],"insecureRegistries":["internal.example.com:5000"]}}`,
		`{"registrySources":{"blockedRegistries":["blocked.example.com"]}}`,
		`{"allowedRegistriesForImport":[{"domainName":"import.example.com:5000","insecure":true}]}`,
	} {
		t.Run(imageJSON, func(t *testing.T) {
			cluster := testCluster()
			if err := json.Unmarshal([]byte(`{"image":`+imageJSON+`}`), &cluster.Spec.HostedCluster.Configuration); err != nil {
				t.Fatal(err)
			}
			cluster.Spec.RegistryAdditionalTrustedCA = map[string]string{"registry.example.com:5000": "certificate"}
			resources, err := ClusterResources(cluster, false, "example.com", "")
			if err != nil {
				t.Fatal(err)
			}
			var hc *hypershiftv1beta1.HostedCluster
			var cm *corev1.ConfigMap
			for _, resource := range resources {
				if resource.Resource == "hostedclusters" {
					hc = resource.Object.(*hypershiftv1beta1.HostedCluster)
				}
				if resource.Resource == "configmaps" && resource.Name == registryAdditionalCAName {
					cm = resource.Object.(*corev1.ConfigMap)
				}
			}
			if hc == nil || hc.Spec.Configuration == nil || hc.Spec.Configuration.Image == nil {
				t.Fatal("missing rendered image configuration")
			}
			var expected configv1.ImageSpec
			if err := json.Unmarshal([]byte(imageJSON), &expected); err != nil {
				t.Fatal(err)
			}
			expected.AdditionalTrustedCA = configv1.ConfigMapNameReference{Name: registryAdditionalCAName}
			if !reflect.DeepEqual(*hc.Spec.Configuration.Image, expected) {
				t.Errorf("rendered image = %#v, want %#v", *hc.Spec.Configuration.Image, expected)
			}
			if cm == nil || cm.Data["registry.example.com..5000"] != "certificate" {
				t.Fatal("missing registry CA ConfigMap or normalized certificate")
			}
			if cm.Labels["seaplane.io/cluster-id"] != string(cluster.UID) {
				t.Errorf("unexpected registry CA ownership labels: %v", cm.Labels)
			}
		})
	}
}
