package render

import (
	"fmt"
	"maps"
	"path"
	"slices"

	configv1 "github.com/openshift/api/config/v1"
	hypershiftv1beta1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/api/util/ipnet"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	seaplanev1alpha1 "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-operator/internal/oidc"
)

// ClusterResources generates the Kubernetes resources for a cluster on the MC.
// baseDomain is the fully assembled DNS base domain from the DNSReservation
// (e.g. "f7a3.0.openshiftapps.com").
func ClusterResources(cluster *seaplanev1alpha1.Cluster, oidcSigningKeyExternal bool,
	baseDomain, controlPlaneOperatorImage string) ([]Resource, error) {
	clusterID, ns, err := managementClusterIdentity(cluster)
	if err != nil {
		return nil, err
	}
	clusterName := cluster.Name // customer-facing identity
	hostedClusterName := HostedClusterName(cluster)
	if hostedClusterName == "" {
		return nil, fmt.Errorf("cluster %s/%s has no valid HostedCluster name", cluster.Namespace, cluster.Name)
	}

	hc, err := hostedCluster(cluster, clusterID, ns, hostedClusterName, oidcSigningKeyExternal, baseDomain, controlPlaneOperatorImage)
	if err != nil {
		return nil, err
	}

	resources := []Resource{
		namespace(clusterID, ns),
		clusterConfig(clusterID, clusterName, ns),
		pullSecret(clusterID, ns),
		apiServingCert(clusterID, hostedClusterName, baseDomain, ns),
		hc,
		sshKey(clusterID, ns),
	}
	if cluster.Spec.AdditionalTrustBundle != nil {
		resources = append(resources, additionalTrustBundle(clusterID, ns, *cluster.Spec.AdditionalTrustBundle))
	}

	if oidcSigningKeyExternal {
		resources = append(resources, oidcSigningKeySecret(cluster.Spec.AccountID, cluster.Spec.OidcConfigID, clusterID, ns))
	}

	return resources, nil
}

// oidcSigningKeyName names the ESO-materialized Secret and ExternalSecret;
// HostedCluster.Spec.ServiceAccountSigningKey references this same name.
const oidcSigningKeyName = "oidc-signing-key"

// oidcSigningKeySecretStorePath is the AWS Secrets Manager path the OidcConfig
// controller writes the signing key to and the ExternalSecret reads from.
func oidcSigningKeySecretStorePath(accountID, oidcConfigID string) string {
	return oidc.SecretName(accountID, oidcConfigID)
}

// oidcSigningKeySecret renders the ExternalSecret pulling the cluster's OIDC
// signing key from the RC's Secrets Manager into a Secret on the MC.
func oidcSigningKeySecret(accountID, oidcConfigID, clusterID, ns string) Resource {
	return Resource{
		Group: "external-secrets.io", Version: "v1", Resource: "externalsecrets",
		Name: oidcSigningKeyName, Namespace: ns,
		Object: &ExternalSecret{
			TypeMeta: metav1.TypeMeta{APIVersion: "external-secrets.io/v1", Kind: "ExternalSecret"},
			ObjectMeta: metav1.ObjectMeta{
				Name:      oidcSigningKeyName,
				Namespace: ns,
				Labels:    clusterResourceLabels(clusterID, "oidc-signing-key"),
			},
			Spec: ExternalSecretSpec{
				RefreshInterval: "1h",
				SecretStoreRef: SecretStoreRef{
					Name: "rc-secrets-manager",
					Kind: "ClusterSecretStore",
				},
				Target: ExternalSecretTarget{
					Name:           oidcSigningKeyName,
					CreationPolicy: "Owner",
				},
				Data: []ExternalSecretDataEntry{
					{
						SecretKey: "key",
						RemoteRef: ExternalRemoteRef{Key: oidcSigningKeySecretStorePath(accountID, oidcConfigID)},
					},
				},
			},
		},
	}
}

func namespace(clusterID, ns string) Resource {
	labels := clusterResourceLabels(clusterID, "namespace")
	labels[managedByLabel] = managedByOperator
	return Resource{
		Group: "", Version: "v1", Resource: "namespaces",
		Name: ns, Namespace: "",
		Object: &corev1.Namespace{
			TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"},
			ObjectMeta: metav1.ObjectMeta{
				Name:   ns,
				Labels: labels,
			},
		},
	}
}

func clusterConfig(clusterID, clusterName, ns string) Resource {
	return Resource{
		Group: "", Version: "v1", Resource: "configmaps",
		Name: "cluster-config", Namespace: ns,
		Object: &corev1.ConfigMap{
			TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
			ObjectMeta: metav1.ObjectMeta{
				Name:      "cluster-config",
				Namespace: ns,
				Labels:    clusterLabels(clusterID),
			},
			Data: map[string]string{
				"cluster_id":   clusterID,
				"cluster_name": clusterName,
			},
		},
	}
}

const additionalTrustBundleName = "additional-trust-bundle"

func additionalTrustBundle(clusterID, ns, bundle string) Resource {
	return Resource{
		Group: "", Version: "v1", Resource: "configmaps",
		Name: additionalTrustBundleName, Namespace: ns,
		Object: &corev1.ConfigMap{
			TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
			ObjectMeta: metav1.ObjectMeta{
				Name:      additionalTrustBundleName,
				Namespace: ns,
				Labels: map[string]string{
					clusterIDLabel:    clusterID,
					resourceTypeLabel: "additional-trust-bundle",
				},
			},
			Data: map[string]string{"ca-bundle.crt": bundle},
		},
	}
}

func pullSecret(clusterID, ns string) Resource {
	return Resource{
		Group: "external-secrets.io", Version: "v1", Resource: "externalsecrets",
		Name: "pull-secret", Namespace: ns,
		Object: &ExternalSecret{
			TypeMeta: metav1.TypeMeta{APIVersion: "external-secrets.io/v1", Kind: "ExternalSecret"},
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pull-secret",
				Namespace: ns,
				Labels:    clusterResourceLabels(clusterID, "pull-secret"),
			},
			Spec: ExternalSecretSpec{
				RefreshInterval: "1h",
				SecretStoreRef: SecretStoreRef{
					Name: "aws-parameter-store",
					Kind: "ClusterSecretStore",
				},
				Target: ExternalSecretTarget{
					Name:           "pull-secret",
					CreationPolicy: "Orphan",
					Template: ExternalSecretTargetTemplate{
						Type: "kubernetes.io/dockerconfigjson",
					},
				},
				Data: []ExternalSecretDataEntry{
					{
						SecretKey: ".dockerconfigjson",
						RemoteRef: ExternalRemoteRef{Key: "/infra/pull-secret"},
					},
				},
			},
		},
	}
}

func apiServingCert(clusterID, hostedClusterName, baseDomain, ns string) Resource {
	return Resource{
		Group: "cert-manager.io", Version: "v1", Resource: "certificates",
		Name: "api-serving-cert", Namespace: ns,
		Object: &Certificate{
			TypeMeta: metav1.TypeMeta{APIVersion: "cert-manager.io/v1", Kind: "Certificate"},
			ObjectMeta: metav1.ObjectMeta{
				Name:      "api-serving-cert",
				Namespace: ns,
				Labels:    clusterLabels(clusterID),
			},
			Spec: CertificateSpec{
				SecretName: "api-serving-cert",
				IssuerRef: CertificateIssuerRef{
					Name: "letsencrypt-dns01",
					Kind: "ClusterIssuer",
				},
				DNSNames: []string{
					fmt.Sprintf("*.%s.%s", hostedClusterName, baseDomain),
				},
			},
		},
	}
}

func hostedCluster(cluster *seaplanev1alpha1.Cluster, clusterID, ns, hostedClusterName string, oidcSigningKeyExternal bool,
	baseDomain, controlPlaneOperatorImage string) (Resource, error) {
	clusterName := cluster.Name // customer-facing identity
	apiHost := fmt.Sprintf("api.%s.%s", hostedClusterName, baseDomain)

	hcSpec, err := toHostedClusterSpec(&cluster.Spec.HostedCluster)
	if err != nil {
		return Resource{}, fmt.Errorf("converting HostedClusterSpec for cluster %s/%s: %w", ns, clusterName, err)
	}

	// --- Platform-managed overrides (always set by the operator) ---
	hcSpec.InfraID = clusterID
	if cluster.Spec.OidcConfigID != "" && !oidcSigningKeyExternal {
		// Managed OIDC: InfraID must equal issuerURL's trailing path segment, since that's the S3 key HyperShift uploads discovery docs to.
		hcSpec.InfraID = path.Base(hcSpec.IssuerURL)
	}
	// Keep the customer-provided base-domain prefix while setting the reserved
	// domain used for cluster ingress. Clear any stale zone IDs: they are
	// platform-managed and must not be accepted from the Cluster CR.
	hcSpec.DNS.BaseDomain = baseDomain
	hcSpec.DNS.PublicZoneID = ""
	hcSpec.DNS.PrivateZoneID = ""
	hcSpec.PullSecret = corev1.LocalObjectReference{Name: "pull-secret"}
	hcSpec.SSHKey = corev1.LocalObjectReference{Name: "ssh-key"}
	hcSpec.KubeAPIServerDNSName = apiHost
	hcSpec.Services = servicePublishingStrategies(hostedClusterName, baseDomain)
	if hcSpec.Configuration == nil {
		hcSpec.Configuration = apiServerConfiguration()
	} else {
		hcSpec.Configuration.APIServer = apiServerConfiguration().APIServer
	}
	if cluster.Spec.AdditionalTrustBundle != nil {
		if *cluster.Spec.AdditionalTrustBundle == "" {
			hcSpec.AdditionalTrustBundle = nil
		} else {
			hcSpec.AdditionalTrustBundle = &corev1.LocalObjectReference{Name: additionalTrustBundleName}
		}
	}

	// --- Defaults (only set if customer didn't specify) ---
	if hcSpec.Etcd.ManagementType == "" {
		hcSpec.Etcd = defaultEtcdSpec()
	}
	if hcSpec.Networking.NetworkType == "" {
		hcSpec.Networking.NetworkType = hypershiftv1beta1.OVNKubernetes
	}
	if hcSpec.InfrastructureAvailabilityPolicy == "" {
		hcSpec.InfrastructureAvailabilityPolicy = hypershiftv1beta1.HighlyAvailable
	}
	if hcSpec.ControllerAvailabilityPolicy == "" {
		hcSpec.ControllerAvailabilityPolicy = hypershiftv1beta1.HighlyAvailable
	}
	if len(hcSpec.Networking.ClusterNetwork) == 0 {
		hcSpec.Networking.ClusterNetwork = []hypershiftv1beta1.ClusterNetworkEntry{
			{CIDR: mustParseCIDR("10.132.0.0/14")},
		}
	}
	if len(hcSpec.Networking.ServiceNetwork) == 0 {
		hcSpec.Networking.ServiceNetwork = []hypershiftv1beta1.ServiceNetworkEntry{
			{CIDR: mustParseCIDR("172.31.0.0/16")},
		}
	}
	if len(hcSpec.Networking.MachineNetwork) == 0 {
		hcSpec.Networking.MachineNetwork = []hypershiftv1beta1.MachineNetworkEntry{
			{CIDR: mustParseCIDR("10.0.0.0/16")},
		}
	}

	// --- Platform overrides ---
	if hcSpec.Platform.AWS != nil {
		hcSpec.Platform.AWS.EndpointAccess = hypershiftv1beta1.PublicAndPrivate
		hcSpec.Platform.AWS.ResourceTags = appendSystemTags(clusterID, cluster.Spec.Tags)
	}

	// References the Secret materialized by oidcSigningKeySecret's ExternalSecret.
	if oidcSigningKeyExternal {
		hcSpec.ServiceAccountSigningKey = &corev1.LocalObjectReference{Name: oidcSigningKeyName}
	} else {
		hcSpec.ServiceAccountSigningKey = nil
	}

	annotations := map[string]string{
		hypershiftv1beta1.PodSecurityAdmissionLabelOverrideAnnotation: "privileged",
		hypershiftv1beta1.CleanupCloudResourcesAnnotation:             "true",
	}
	// Development override: pin the control-plane-operator image so hosted
	// clusters run a chosen CPO build (e.g. from an openshift/hypershift PR).
	if controlPlaneOperatorImage != "" {
		annotations[hypershiftv1beta1.ControlPlaneOperatorImageAnnotation] = controlPlaneOperatorImage
	}

	return Resource{
		Group: "hypershift.openshift.io", Version: "v1beta1", Resource: "hostedclusters",
		Name: hostedClusterName, Namespace: ns,
		Object: &hypershiftv1beta1.HostedCluster{
			TypeMeta: metav1.TypeMeta{
				APIVersion: "hypershift.openshift.io/v1beta1",
				Kind:       "HostedCluster",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:        hostedClusterName,
				Namespace:   ns,
				Labels:      clusterLabels(clusterID),
				Annotations: annotations,
			},
			Spec: *hcSpec,
		},
	}, nil
}

func servicePublishingStrategies(hostedClusterName, baseDomain string) []hypershiftv1beta1.ServicePublishingStrategyMapping {
	apiHost := fmt.Sprintf("api.%s.%s", hostedClusterName, baseDomain)
	return []hypershiftv1beta1.ServicePublishingStrategyMapping{
		{
			Service: hypershiftv1beta1.APIServer,
			ServicePublishingStrategy: hypershiftv1beta1.ServicePublishingStrategy{
				Type:  hypershiftv1beta1.Route,
				Route: &hypershiftv1beta1.RoutePublishingStrategy{Hostname: apiHost},
			},
		},
		{
			Service: hypershiftv1beta1.OAuthServer,
			ServicePublishingStrategy: hypershiftv1beta1.ServicePublishingStrategy{
				Type:  hypershiftv1beta1.Route,
				Route: &hypershiftv1beta1.RoutePublishingStrategy{Hostname: fmt.Sprintf("oauth.%s.%s", hostedClusterName, baseDomain)},
			},
		},
		{
			Service: hypershiftv1beta1.Konnectivity,
			ServicePublishingStrategy: hypershiftv1beta1.ServicePublishingStrategy{
				Type: hypershiftv1beta1.Route,
			},
		},
		{
			Service: hypershiftv1beta1.Ignition,
			ServicePublishingStrategy: hypershiftv1beta1.ServicePublishingStrategy{
				Type: hypershiftv1beta1.Route,
			},
		},
	}
}

func apiServerConfiguration() *hypershiftv1beta1.ClusterConfiguration {
	return &hypershiftv1beta1.ClusterConfiguration{
		APIServer: &configv1.APIServerSpec{
			ServingCerts: configv1.APIServerServingCerts{
				NamedCertificates: []configv1.APIServerNamedServingCert{
					{
						ServingCertificate: configv1.SecretNameReference{
							Name: "api-serving-cert",
						},
					},
				},
			},
		},
	}
}

func defaultEtcdSpec() hypershiftv1beta1.EtcdSpec {
	return hypershiftv1beta1.EtcdSpec{
		ManagementType: hypershiftv1beta1.Managed,
		Managed: &hypershiftv1beta1.ManagedEtcdSpec{
			Storage: hypershiftv1beta1.ManagedEtcdStorageSpec{
				Type: hypershiftv1beta1.PersistentVolumeEtcdStorage,
				PersistentVolume: &hypershiftv1beta1.PersistentVolumeEtcdStorageSpec{
					Size:             ptr.To(resource.MustParse("32Gi")),
					StorageClassName: ptr.To("gp3"),
				},
			},
		},
	}
}

// appendSystemTags emits reserved system tags followed by non-conflicting
// customer tags. Customer tags are sorted so repeated renders produce an
// identical spec and do not cause spurious updates.
func appendSystemTags(clusterID string, customerTags map[string]string) []hypershiftv1beta1.AWSResourceTag {
	tags := []hypershiftv1beta1.AWSResourceTag{
		{Key: "red-hat-managed", Value: "true"},
	}
	if clusterID != "" {
		tags = append(tags, hypershiftv1beta1.AWSResourceTag{
			Key: fmt.Sprintf("kubernetes.io/cluster/%s", clusterID), Value: "owned",
		})
	}
	seen := make(map[string]struct{}, len(tags))
	for _, t := range tags {
		seen[t.Key] = struct{}{}
	}
	for _, key := range slices.Sorted(maps.Keys(customerTags)) {
		if _, taken := seen[key]; taken {
			continue
		}
		tags = append(tags, hypershiftv1beta1.AWSResourceTag{Key: key, Value: customerTags[key]})
	}
	return tags
}

func mustParseCIDR(s string) ipnet.IPNet {
	parsed, err := ipnet.ParseCIDR(s)
	if err != nil {
		panic(fmt.Sprintf("invalid CIDR %q: %v", s, err))
	}
	return *parsed
}

func sshKey(clusterID, ns string) Resource {
	return Resource{
		Group: "", Version: "v1", Resource: "secrets",
		Name: "ssh-key", Namespace: ns,
		Object: &corev1.Secret{
			TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
			ObjectMeta: metav1.ObjectMeta{
				Name:      "ssh-key",
				Namespace: ns,
				Labels:    clusterLabels(clusterID),
			},
			Type: corev1.SecretTypeOpaque,
			Data: map[string][]byte{
				"id_rsa.pub": {},
			},
		},
	}
}
