/*
Copyright 2026.

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

// Lifecycle E2E - the core rosa-seaplane acceptance test.
//
// Provisions its own hosted cluster from scratch through the platform API (Go
// clientset), using the rosa CLI for the customer-account AWS prerequisites,
// and asserts the live-cluster behaviours nothing else covers: DNS reservation,
// managed ingress DNS + DNS01 serving cert, KAS reachability, lifecycle alert
// silencing, the hcp availability metric, nodepool scaling, and clean teardown.
//
// Out of scope by design: the OIDC-config matrix (managed/unmanaged/reuse/
// cross-account) is owned by the e2e-api suite; here OIDC is setup only.
//
// Required environment variables:
//
//	E2E_BASE_URL / BASE_URL   - platform API base URL (seaplane URL)
//	ROSA_CLI_BIN              - path to the built rosa binary (the ci/e2e run
//	                            script builds it; set it for local runs)
//	CUSTOMER_AWS_PROFILE      - AWS profile for customer-account operations
//
// Optional:
//
//	AWS_REGION                - defaults to us-east-1
//	CLUSTER_PREFIX            - isolation prefix (set by rosa-seaplane pre-suite)
//	E2E_CUSTOMER_ACCOUNT_ID   - customer account ID (derived from STS if absent)
//	HCP_CLUSTER_NAME          - fixed cluster name (generated if absent)
//	OCP_IMAGE                 - release image; the managed-ingress/DNS01 assertion
//	                            requires 5.1.0-ec.2 or newer
//	SEAPLANE_INSTANCE_TYPE    - node instance type (defaults to m5.xlarge)
//	E2E_ALERTMANAGER_URL      - enables the lifecycle-silence assertions
//	E2E_RHOBS_API_URL         - enables the Thanos metric assertion
//	E2E_SKIP_CLEANUP          - skip DeferCleanup safety-net teardown
//	ROSA_REGIONAL_TEARDOWN_FIRE_AND_FORGET - "true" skips waiting for the VPC stack
//	                            deletion to complete
package e2e_lifecycle_test

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	seaplanev1alpha1 "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1"
	v1alpha1 "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1/public"
	seaplane "github.com/openshift-online/rosa-seaplane-api/clientset"
	"github.com/openshift-online/rosa-seaplane-api/clientset/platform"
	hfrest "github.com/openshift-online/rosa-seaplane-api/clientset/rest"
	amhelper "github.com/openshift-online/rosa-seaplane-api/test/helpers/alertmanager"
	awstest "github.com/openshift-online/rosa-seaplane-api/test/helpers/aws"
	"github.com/openshift-online/rosa-seaplane-api/test/helpers/thanos"
	hypershiftv1beta1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	clusterReadyTimeout  = 35 * time.Minute
	clusterReadyPoll     = 30 * time.Second
	clusterDeleteTimeout = 20 * time.Minute
	clusterDeletePoll    = 30 * time.Second

	dnsReservationReadyTimeout  = 5 * time.Minute
	dnsReservationReadyPoll     = 5 * time.Second
	dnsReservationDeleteTimeout = 5 * time.Minute
	dnsReservationDeletePoll    = 5 * time.Second

	nodepoolReadyTimeout = 30 * time.Minute
	nodepoolReadyPoll    = 30 * time.Second

	silenceClearedTimeout = 10 * time.Minute
	silenceClearedPoll    = 15 * time.Second

	// After the cluster is Ready, NS delegation to the ingress zone and the
	// ACME DNS01 serving-cert issuance still take a few minutes to converge.
	ingressReadyTimeout = 20 * time.Minute
	ingressReadyPoll    = 30 * time.Second

	defaultRegion       = "us-east-1"
	defaultInstanceType = "m5.xlarge"
	// 5.1.0-ec.2 is the minimum release that supports managed ingress DNS +
	// the DNS01 serving cert; older releases fail that spec.
	defaultReleaseImage = "quay.io/openshift-release-dev/ocp-release:5.1.0-ec.2-multi"
)

var _ = Describe("Lifecycle: hosted cluster end-to-end", Ordered, func() {
	var (
		ctx             context.Context
		baseURL         string
		rosaBin         string
		customerProfile string
		region          string
		ocmConfig       string
		accountID       string
		version         string
		instanceType    string
		amURL           string
		rhobsURL        string

		clusterName      string
		rolePrefix       string
		networkStackName string

		// captured as the flow progresses
		dnsName           string
		dnsUID            string
		clusterUID        string
		clusterNamespace  string
		oidcName          string
		oidcUID           string
		issuerURL         string
		hostedClusterName string
		baseDomain        string
		vpcID             string
		subnetID          string
		zone              string
		roles             hypershiftv1beta1.AWSRolesRef
		instanceProfile   string
		nodepoolName      string

		awsCfg aws.Config
		cs     *seaplane.Clientset
		rosaE  []string // environment for rosa/aws commands

		// what to tear down
		dnsCreated           bool
		oidcCreated          bool
		oidcProviderCreated  bool
		operatorRolesCreated bool
		networkCreated       bool
		clusterCreated       bool
	)

	BeforeAll(func() {
		ctx = context.Background()

		baseURL = firstNonEmpty(os.Getenv("E2E_BASE_URL"), os.Getenv("BASE_URL"))
		if baseURL == "" {
			Skip("E2E_BASE_URL/BASE_URL is not set")
		}
		rosaBin = os.Getenv("ROSA_CLI_BIN")
		if rosaBin == "" {
			Skip("ROSA_CLI_BIN is not set")
		}
		customerProfile = os.Getenv("CUSTOMER_AWS_PROFILE")
		if customerProfile == "" {
			Skip("CUSTOMER_AWS_PROFILE is not set")
		}

		region = firstNonEmpty(os.Getenv("AWS_REGION"), defaultRegion)
		version = firstNonEmpty(os.Getenv("OCP_IMAGE"), defaultReleaseImage)
		instanceType = firstNonEmpty(os.Getenv("SEAPLANE_INSTANCE_TYPE"), defaultInstanceType)
		amURL = os.Getenv("E2E_ALERTMANAGER_URL")
		rhobsURL = os.Getenv("E2E_RHOBS_API_URL")

		// Per-run OCM config so `rosa login` persists for this suite only. rosa
		// creates the file on login; it must NOT pre-exist empty (it would fail
		// to parse as JSON), so point at a fresh path inside a temp dir.
		ocmDir, err := os.MkdirTemp("", "lifecycle-ocm-*")
		Expect(err).ToNot(HaveOccurred(), "creating OCM config temp dir")
		ocmConfig = filepath.Join(ocmDir, "ocm.json")
		rosaE = rosaEnv(os.Environ(), customerProfile, region, ocmConfig)

		var awsErr error
		awsCfg, awsErr = awsconfig.LoadDefaultConfig(ctx,
			awsconfig.WithSharedConfigProfile(customerProfile),
			awsconfig.WithRegion(region),
		)
		Expect(awsErr).ToNot(HaveOccurred(), "loading customer AWS config")

		identity, err := sts.NewFromConfig(awsCfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
		Expect(err).ToNot(HaveOccurred(), "getting customer caller identity")
		accountID = firstNonEmpty(os.Getenv("E2E_CUSTOMER_ACCOUNT_ID"), *identity.Account)
		GinkgoWriter.Printf("Customer account resolved\n")

		cs, err = seaplane.NewForConfig(&hfrest.Config{
			Host:      baseURL,
			AccountID: *identity.Account,
			CallerARN: *identity.Arn,
			AWSConfig: awsCfg,
		})
		Expect(err).ToNot(HaveOccurred(), "building SDK clientset")

		By("logging the rosa CLI into the seaplane platform")
		Expect(rosaLogin(rosaBin, rosaE, baseURL)).To(Succeed(), "rosa login")

		// Names: cluster name is deliberately long to exercise the DNSReservation
		// path; the role prefix is kept short so "<prefix>-<suffix>" role names
		// stay within the 64-char IAM limit. CLUSTER_PREFIX (set by the
		// rosa-seaplane pre-suite) isolates concurrent suites.
		prefix := os.Getenv("CLUSTER_PREFIX")
		suffix := fmt.Sprintf("%08x", time.Now().UnixNano()&0xffffffff)
		clusterName = firstNonEmpty(os.Getenv("HCP_CLUSTER_NAME"),
			fmt.Sprintf("%slifecycle-long-%s", prefix, suffix))
		Expect(len(clusterName)).To(BeNumerically(">", seaplanev1alpha1.MaxClusterNameLengthWithoutDNSReservation()),
			"lifecycle must use a Cluster name longer than the automatic-DNS namespace budget")
		rolePrefix = "lc-" + suffix
		networkStackName = rolePrefix + "-net"
		GinkgoWriter.Printf("Cluster: %s  rolePrefix: %s\n", clusterName, rolePrefix)

		DeferCleanup(func() {
			if os.Getenv("E2E_SKIP_CLEANUP") != "" {
				GinkgoWriter.Printf("DeferCleanup: E2E_SKIP_CLEANUP set, skipping\n")
				return
			}
			// Dump diagnostics before deprovisioning (best-effort).
			runPreCleanupHook()
			cctx := context.Background()
			// Best-effort: the infra deletes are self-protecting while the cluster
			// still exists (rosa refuses role/oidc delete, the VPC stack delete fails
			// on live ENIs), so continue through all of them, but surface every
			// failure instead of swallowing it so a leaked resource is visible.
			warn := func(what string, err error) {
				if err != nil {
					GinkgoWriter.Printf("DeferCleanup WARNING: %s failed: %v\n", what, err)
				}
			}
			// Phase 1: platform resources (waited).
			if clusterCreated && clusterName != "" {
				warn("cluster delete", deleteCluster(cctx, cs, clusterName))
				clusterCreated = false
			}
			if dnsCreated && dnsName != "" {
				warn("DNS reservation delete", deleteDNSReservation(cctx, cs, dnsName))
				dnsCreated = false
			}
			// Phase 2: customer AWS infra (stack delete skips the wait when fireAndForget).
			if operatorRolesCreated {
				warn("operator-roles delete", rosaDeleteOperatorRoles(rosaBin, rosaE, rolePrefix))
				operatorRolesCreated = false
			}
			if oidcCreated || oidcProviderCreated {
				warn("oidc-config delete", rosaDeleteOidcConfig(rosaBin, rosaE, oidcName))
				oidcCreated, oidcProviderCreated = false, false
			}
			if networkCreated {
				warn("network stack delete", awsDeleteStack(rosaE, networkStackName, region))
				networkCreated = false
			}
			_ = os.RemoveAll(filepath.Dir(ocmConfig))
		})
	})

	It("reserves a DNS prefix", func() {
		dnsName = clusterName + "-dns"
		reservations := cs.SeaplaneV1alpha1().DNSReservations()
		dnsCreated = true
		reservation, err := reservations.Create(ctx, &v1alpha1.DNSReservation{
			ObjectMeta: metav1.ObjectMeta{Name: dnsName},
		}, platform.CreateOptions{})
		Expect(err).ToNot(HaveOccurred(), "DNSReservation create")
		dnsUID = string(reservation.UID)
		Expect(dnsUID).ToNot(BeEmpty(), "created DNSReservation should have a UID")

		Expect(reservations.WaitUntil(ctx, dnsName,
			func(r *v1alpha1.DNSReservation) bool {
				if r == nil {
					return false
				}
				hostedClusterName = r.Status.InternalHostedClusterName
				baseDomain = r.Status.BaseDomain
				return r.Status.Phase == v1alpha1.DNSReservationPhaseReady && baseDomain != "" && hostedClusterName != ""
			},
			dnsReservationReadyPoll, dnsReservationReadyTimeout,
		)).To(Succeed(), "DNSReservation should be Ready with a base domain and hosted-cluster name")
		Expect(hostedClusterName).ToNot(BeEmpty())
		Expect(baseDomain).ToNot(BeEmpty())
		GinkgoWriter.Printf("DNSReservation ready: baseDomain=%s hostedCluster=%s\n", baseDomain, hostedClusterName)
	})

	It("provisions the customer AWS prerequisites (VPC, OIDC, IAM)", func() {
		By("creating the VPC network")
		networkCreated = true
		Expect(rosaCreateNetwork(rosaBin, rosaE, networkStackName, hostedClusterName, region)).To(Succeed(), "rosa create network")
		var err error
		vpcID, subnetID, err = networkOutputs(rosaE, networkStackName, region)
		Expect(err).ToNot(HaveOccurred(), "reading network stack outputs")
		GinkgoWriter.Printf("VPC: %s  Subnet: %s\n", vpcID, subnetID)

		subnetOut, err := ec2.NewFromConfig(awsCfg).DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{
			SubnetIds: []string{subnetID},
		})
		Expect(err).ToNot(HaveOccurred(), "describing subnet %s", subnetID)
		Expect(subnetOut.Subnets).ToNot(BeEmpty(), "subnet %s not found", subnetID)
		zone = *subnetOut.Subnets[0].AvailabilityZone

		By("creating a managed OIDC config via the platform API")
		oidcName = clusterName + "-oidc"
		oidcConfigs := cs.SeaplaneV1alpha1().OidcConfigs()
		oidcCreated = true
		managed, err := oidcConfigs.Create(ctx, &v1alpha1.OidcConfig{
			ObjectMeta: metav1.ObjectMeta{Name: oidcName},
			Spec:       v1alpha1.OidcConfigSpec{Type: "managed"},
		}, platform.CreateOptions{})
		Expect(err).ToNot(HaveOccurred(), "OidcConfig create")
		oidcUID = string(managed.UID)
		issuerURL = managed.Spec.IssuerUrl
		Expect(issuerURL).ToNot(BeEmpty(), "managed OidcConfig should return its issuer URL")

		By("creating the customer AWS OIDC provider")
		oidcProviderCreated = true
		Expect(rosaCreateOidcProvider(rosaBin, rosaE, oidcName)).To(Succeed(), "rosa create oidc-provider")

		By("creating the operator IAM roles")
		operatorRolesCreated = true
		Expect(rosaCreateOperatorRoles(rosaBin, rosaE, rolePrefix, oidcName)).To(Succeed(), "rosa create operator-roles")
		roles = computeRolesRef(rolePrefix, accountID)
		instanceProfile = computeInstanceProfile(rolePrefix)
		GinkgoWriter.Printf("OIDC uid=%s issuer=%s  instanceProfile=%s\n", oidcUID, issuerURL, instanceProfile)
	})

	It("creates the cluster, silences it while provisioning, and reaches Ready", func() {
		subnetRef := subnetID
		clusterCreated = true
		cluster, err := cs.SeaplaneV1alpha1().Clusters().Create(ctx, &v1alpha1.Cluster{
			ObjectMeta: metav1.ObjectMeta{Name: clusterName},
			Spec: v1alpha1.ClusterSpec{
				DNSReservationID: dnsUID,
				OidcConfigID:     oidcUID,
				HostedCluster: v1alpha1.HostedClusterSpecPassthrough{
					Release: hypershiftv1beta1.Release{Image: version},
					Platform: v1alpha1.PlatformSpec{
						Type: hypershiftv1beta1.AWSPlatform,
						AWS: &v1alpha1.AWSPlatformSpec{
							Region:   region,
							RolesRef: roles,
							CloudProviderConfig: &hypershiftv1beta1.AWSCloudProviderConfig{
								VPC:    vpcID,
								Zone:   zone,
								Subnet: &hypershiftv1beta1.AWSResourceReference{ID: &subnetRef},
							},
						},
					},
				},
			},
		}, platform.CreateOptions{})
		Expect(err).ToNot(HaveOccurred(), "cluster create")
		clusterUID = string(cluster.UID)
		Expect(clusterUID).ToNot(BeEmpty())
		// Silence matchers use the Cluster CR namespace (account-<id>), see
		// silence.IdentityFromCluster.
		clusterNamespace = cluster.Namespace
		Expect(clusterNamespace).ToNot(BeEmpty())
		GinkgoWriter.Printf("Cluster %s created (uid=%s ns=%s)\n", clusterName, clusterUID, clusterNamespace)

		clusters := cs.SeaplaneV1alpha1().Clusters()
		sawInstalling := false
		Eventually(func(g Gomega) {
			c, err := clusters.Get(ctx, clusterName, platform.GetOptions{})
			g.Expect(err).ToNot(HaveOccurred())
			phase := c.Status.Phase
			GinkgoWriter.Printf("[%s] cluster phase=%s\n", time.Now().Format(time.RFC3339), phase)

			// Observe the installing silence during the provisioning window.
			if amURL != "" && !sawInstalling &&
				(phase == v1alpha1.ClusterPhaseProvisioning || phase == v1alpha1.ClusterPhaseWaitingForPlacement) {
				sils, serr := amhelper.ListManagedSilences(ctx, amURL, clusterNamespace, clusterName)
				if serr == nil && amhelper.HasInstallingSilence(sils) {
					sawInstalling = true
				}
			}
			g.Expect(phase).To(Equal(v1alpha1.ClusterPhaseReady))
		}, clusterReadyTimeout, clusterReadyPoll).Should(Succeed(), "cluster should reach Ready")

		if amURL != "" {
			Expect(sawInstalling).To(BeTrue(), "expected an installing lifecycle silence during provisioning")
			GinkgoWriter.Printf("Installing lifecycle silence observed during provisioning\n")
		} else {
			GinkgoWriter.Printf("WARNING: installing-silence check SKIPPED - E2E_ALERTMANAGER_URL unset; silencing NOT verified\n")
		}
		GinkgoWriter.Printf("Cluster %s is Ready\n", clusterName)
	})

	It("creates a nodepool that reaches Ready in the account namespace", func() {
		nodepoolName = clusterName + ".np"
		replicas := int32(2)
		subnetRef := subnetID
		nodepools := cs.SeaplaneV1alpha1().NodePools()
		_, err := nodepools.Create(ctx, &v1alpha1.NodePool{
			ObjectMeta: metav1.ObjectMeta{Name: nodepoolName},
			Spec: v1alpha1.NodePoolSpec{
				NodePool: v1alpha1.NodePoolSpecPassthrough{
					ClusterName: clusterName,
					Replicas:    &replicas,
					Platform: v1alpha1.NodePoolPlatform{
						Type: hypershiftv1beta1.AWSPlatform,
						AWS: &v1alpha1.AWSNodePoolPlatform{
							InstanceType:    instanceType,
							InstanceProfile: instanceProfile,
							Subnet:          hypershiftv1beta1.AWSResourceReference{ID: &subnetRef},
						},
					},
					Release: hypershiftv1beta1.Release{Image: version},
				},
			},
		}, platform.CreateOptions{})
		Expect(err).ToNot(HaveOccurred(), "nodepool create")

		Expect(nodepools.WaitUntil(ctx, nodepoolName,
			func(n *v1alpha1.NodePool) bool {
				if n == nil {
					return false
				}
				GinkgoWriter.Printf("[%s] nodepool phase=%s\n", time.Now().Format(time.RFC3339), n.Status.Phase)
				return n.Status.Phase == v1alpha1.NodePoolPhaseReady
			},
			nodepoolReadyPoll, nodepoolReadyTimeout,
		)).To(Succeed(), "nodepool should reach Ready")
	})

	It("clears lifecycle silences once Ready", func() {
		if amURL == "" {
			Skip("E2E_ALERTMANAGER_URL not set")
		}
		Eventually(func(g Gomega) {
			sils, err := amhelper.ListManagedSilences(ctx, amURL, clusterNamespace, clusterName)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(sils).To(BeEmpty())
		}, silenceClearedTimeout, silenceClearedPoll).Should(Succeed(),
			"lifecycle silences should expire once the cluster is Ready")
	})

	It("serves managed ingress DNS with a DNS01 certificate", func() {
		appsDomain := fmt.Sprintf("apps.in.%s.%s", hostedClusterName, baseDomain)
		hosts := managedIngressRouteHosts(appsDomain)
		Eventually(func() error {
			for _, host := range hosts {
				if err := verifyManagedIngressRoute(ctx, host); err != nil {
					return err
				}
			}
			return nil
		}, ingressReadyTimeout, ingressReadyPoll).Should(Succeed(),
			"console, oauth, and downloads routes should resolve and serve a DNS01-provisioned certificate")
		GinkgoWriter.Printf("Managed ingress verified under %s\n", appsDomain)
	})

	It("exposes the KAS endpoint over verified DNS/TLS", func() {
		c, err := cs.SeaplaneV1alpha1().Clusters().Get(ctx, clusterName, platform.GetOptions{})
		Expect(err).ToNot(HaveOccurred())
		ep := c.Status.ControlPlaneEndpoint
		Expect(ep.Host).ToNot(BeEmpty(), "controlPlaneEndpoint.host should be set once Ready")
		port := "6443"
		if ep.Port > 0 {
			port = fmt.Sprintf("%d", ep.Port)
		}
		hostPort := net.JoinHostPort(ep.Host, port)
		Eventually(func(g Gomega) {
			lctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			addrs, err := net.DefaultResolver.LookupHost(lctx, ep.Host)
			g.Expect(err).ToNot(HaveOccurred(), "DNS should resolve for %s", ep.Host)
			g.Expect(addrs).ToNot(BeEmpty())
			conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 10 * time.Second}, "tcp", hostPort, &tls.Config{})
			g.Expect(err).ToNot(HaveOccurred(), "TLS handshake should succeed for %s", hostPort)
			g.Expect(conn.Close()).To(Succeed())
		}, 2*time.Minute, 10*time.Second).Should(Succeed())
	})

	It("publishes the hcp:hostedcluster_available metric for this cluster", func() {
		if rhobsURL == "" {
			Skip("E2E_RHOBS_API_URL not set")
		}
		client := awstest.NewAPIClient(rhobsURL)
		// The rule is max by (namespace, name, cluster)(...), so `name` is this
		// hosted cluster; it emits value 1 while the KAS is available.
		query := fmt.Sprintf(`hcp:hostedcluster_available{name=%q}`, hostedClusterName)
		Eventually(func(g Gomega) {
			resp := thanos.Query(client, query)
			g.Expect(resp.Status).To(Equal("success"))
			g.Expect(resp.Data.Result).ToNot(BeEmpty(), "no availability series for hosted cluster %s", hostedClusterName)
			g.Expect(resp.Data.Result[0].Value).To(HaveLen(2))
			g.Expect(resp.Data.Result[0].Value[1]).To(Equal("1"))
		}, "5m", "15s").Should(Succeed(),
			"hcp:hostedcluster_available should report this cluster available")
	})

	It("tears down cleanly", func() {
		if os.Getenv("E2E_SKIP_CLEANUP") != "" {
			Skip("E2E_SKIP_CLEANUP set; leaving resources standing")
		}
		// Phase 1: platform resources (clientset), waited. The operators need their
		// roles to deprovision, so the cluster must be gone before infra teardown.
		By("deleting the cluster (cascades nodepool teardown)")
		Expect(deleteCluster(ctx, cs, clusterName)).To(Succeed())
		clusterCreated = false

		By("deleting the DNS reservation")
		Expect(deleteDNSReservation(ctx, cs, dnsName)).To(Succeed())
		dnsCreated = false

		// Phase 2: customer AWS infra (rosa/aws). The VPC stack delete waits for
		// completion unless ROSA_REGIONAL_TEARDOWN_FIRE_AND_FORGET is set.
		By("deleting the operator roles")
		Expect(rosaDeleteOperatorRoles(rosaBin, rosaE, rolePrefix)).To(Succeed())
		operatorRolesCreated = false

		By("deleting the OIDC config and provider")
		Expect(rosaDeleteOidcConfig(rosaBin, rosaE, oidcName)).To(Succeed())
		oidcCreated, oidcProviderCreated = false, false

		By("deleting the VPC network stack")
		Expect(awsDeleteStack(rosaE, networkStackName, region)).To(Succeed())
		networkCreated = false
	})
})

// managedIngressRouteHosts returns the default platform route hostnames that
// must resolve and serve a managed serving certificate once managed ingress DNS
// is reconciled. appsDomain is "apps.in.<hostedCluster>.<base>".
func managedIngressRouteHosts(appsDomain string) []string {
	return []string{
		"console-openshift-console." + appsDomain,
		"oauth-openshift." + appsDomain,
		"downloads-openshift-console." + appsDomain,
	}
}

// verifyManagedIngressRoute dials host over TLS on :443 and validates the served
// certificate against the system roots. Success proves both NS delegation to the
// ingress zone (the name resolved) and that cert-manager provisioned the serving
// cert via the ACME DNS01 challenge (the self-signed HyperShift wildcard would
// fail chain verification). A bounded context covers resolve + dial.
func verifyManagedIngressRoute(ctx context.Context, host string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if _, err := net.DefaultResolver.LookupHost(ctx, host); err != nil {
		return fmt.Errorf("resolve %s: %w", host, err)
	}
	dialer := tls.Dialer{Config: &tls.Config{ServerName: host}}
	conn, err := dialer.DialContext(ctx, "tcp", host+":443")
	if err != nil {
		return fmt.Errorf("tls dial %s: %w", host, err)
	}
	defer conn.Close()
	certs := conn.(*tls.Conn).ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return fmt.Errorf("%s served no certificate", host)
	}
	GinkgoWriter.Printf("  %s: TLS verified, issuer=%q\n", host, certs[0].Issuer)
	return nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
