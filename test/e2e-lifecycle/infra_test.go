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

// Infra setup/teardown helpers for the lifecycle suite.
//
// The platform objects (DNSReservation, OidcConfig, Cluster, NodePool) are
// driven through the Go clientset in lifecycle_test.go. The customer-account
// AWS prerequisites (VPC, IAM operator roles, OIDC provider) are created with
// the rosa CLI and read back here.
//
// Notes:
//   - operator-role ARNs and the worker instance profile are derived from the
//     role prefix (deterministic naming in rosa's pkg/seaplane/roles.go), so no
//     CloudFormation parsing is needed for IAM.
//   - only `rosa create network` creates a CloudFormation stack, so that is the
//     only place we query CloudFormation outputs.
package e2e_lifecycle_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"

	v1alpha1 "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1/public"
	seaplane "github.com/openshift-online/rosa-seaplane-api/clientset"
	"github.com/openshift-online/rosa-seaplane-api/clientset/platform"
	hypershiftv1beta1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
)

// awsPartition is the IAM ARN partition. GovCloud/China are out of scope for e2e.
const awsPartition = "aws"

// operatorRoleSuffixes maps each HyperShift AWSRolesRef field to the rosa role
// suffix (rosa names roles "<prefix>-<suffix>"; see pkg/seaplane/roles.go).
// Keep in sync with rosa's ComputeRolesRef.
const (
	suffixIngress              = "ingress"
	suffixCloudController      = "cloud-controller-manager"
	suffixEBSCSI               = "ebs-csi"
	suffixImageRegistry        = "image-registry"
	suffixNetworkConfig        = "network-config"
	suffixControlPlaneOperator = "control-plane-operator"
	suffixNodePoolManagement   = "node-pool-management"
	suffixWorkerRole           = "ROSA-Worker-Role"
)

// rosaEnv returns the environment for every rosa/aws invocation: the customer
// AWS profile and region, plus a per-run OCM config path so `rosa login`
// persists the seaplane URL for subsequent commands.
func rosaEnv(baseEnv []string, customerProfile, region, ocmConfig string) []string {
	return append(baseEnv,
		"AWS_PROFILE="+customerProfile,
		"AWS_DEFAULT_REGION="+region,
		"AWS_REGION="+region,
		"OCM_CONFIG="+ocmConfig,
	)
}

// runPreCleanupHook runs PRE_CLEANUP_HOOK (set by the rosa-seaplane pre-suite to
// dump hosted-cluster diagnostics to S3) before teardown, so a failed run
// captures the cluster's state while it still exists. Best-effort - a hook
// failure must not abort cleanup.
func runPreCleanupHook() {
	hook := os.Getenv("PRE_CLEANUP_HOOK")
	if hook == "" {
		return
	}
	GinkgoWriter.Printf("Running PRE_CLEANUP_HOOK before teardown: %s\n", hook)
	cmd := exec.Command("bash", "-c", hook)
	cmd.Stdout = GinkgoWriter
	cmd.Stderr = GinkgoWriter
	if err := cmd.Run(); err != nil {
		GinkgoWriter.Printf("WARNING: PRE_CLEANUP_HOOK failed: %v (continuing cleanup)\n", err)
	}
}

// redactArgs masks sensitive values (endpoint URLs) before logging rosa args.
func redactArgs(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		if strings.HasPrefix(a, "--seaplane-url=") {
			out[i] = "--seaplane-url=<redacted>"
		} else {
			out[i] = a
		}
	}
	return out
}

// runRosa runs the rosa binary, streaming output to the Ginkgo log.
func runRosa(bin string, env []string, args ...string) error {
	GinkgoWriter.Printf("+ rosa %s\n", strings.Join(redactArgs(args), " "))
	cmd := exec.Command(bin, args...)
	cmd.Env = env
	cmd.Stdout = GinkgoWriter
	cmd.Stderr = GinkgoWriter
	return cmd.Run()
}

// rosaLogin persists the seaplane URL so every later rosa call auto-routes to
// the platform API (SigV4-signed with the customer AWS credentials).
func rosaLogin(bin string, env []string, baseURL string) error {
	return runRosa(bin, env, "login", "--seaplane-url="+baseURL)
}

// rosaCreateNetwork creates the HCP VPC CloudFormation stack. hostedClusterName
// is the DNSReservation's internal hosted-cluster name (used for the private
// hosted zone). A single AZ with a single NAT gateway keeps e2e cost down.
func rosaCreateNetwork(bin string, env []string, stackName, hostedClusterName, region string) error {
	return runRosa(bin, env,
		"create", "network", "rosa-quickstart-hcp-vpc",
		"--param", "Name="+stackName,
		"--param", "ClusterName="+hostedClusterName,
		"--param", "Region="+region,
		"--param", "AvailabilityZoneCount=1",
		"--param", "SingleNatGateway=true",
		"--mode", "auto",
	)
}

// rosaCreateOidcProvider creates the customer-account AWS OIDC provider for the
// issuer of the given platform OidcConfig (resolved by name via the platform
// API). Idempotent: "already exists" returns 0.
func rosaCreateOidcProvider(bin string, env []string, oidcConfigName string) error {
	return runRosa(bin, env,
		"create", "oidc-provider",
		"--oidc-config-id", oidcConfigName,
		"--mode", "auto", "--yes",
	)
}

// rosaCreateOperatorRoles creates the 7 operator IAM roles + worker instance
// profile under prefix, trusting the OIDC config's issuer.
func rosaCreateOperatorRoles(bin string, env []string, prefix, oidcConfigName string) error {
	return runRosa(bin, env,
		"create", "operator-roles",
		"--prefix", prefix,
		"--oidc-config-id", oidcConfigName,
		"--mode", "auto", "--yes",
	)
}

// rosaDeleteOperatorRoles removes the operator roles + instance profile. Must
// run after the cluster is deleted (rosa refuses while a cluster uses them).
func rosaDeleteOperatorRoles(bin string, env []string, prefix string) error {
	return runRosa(bin, env, "delete", "operator-roles", "--prefix", prefix, "--mode", "auto", "--yes")
}

// rosaDeleteOidcConfig removes the platform OidcConfig and its AWS OIDC
// provider. Must run after the cluster is deleted.
func rosaDeleteOidcConfig(bin string, env []string, oidcConfigName string) error {
	return runRosa(bin, env, "delete", "oidc-config", "--oidc-config-id", oidcConfigName, "--mode", "auto", "--yes")
}

// awsDeleteStack deletes the network CloudFormation stack (rosa has no network
// delete subcommand).
func awsDeleteStack(env []string, stackName, region string) error {
	GinkgoWriter.Printf("+ aws cloudformation delete-stack --stack-name %s\n", stackName)
	del := exec.Command("aws", "cloudformation", "delete-stack", "--stack-name", stackName, "--region", region)
	del.Env = env
	del.Stdout = GinkgoWriter
	del.Stderr = GinkgoWriter
	if err := del.Run(); err != nil {
		return err
	}
	// Fire-and-forget: the delete is initiated, don't block on completion.
	if fireAndForget() {
		GinkgoWriter.Printf("fire-and-forget: %s delete initiated, not waiting\n", stackName)
		return nil
	}
	// Full teardown: wait so a DELETE_FAILED (e.g. a leftover ENI) doesn't pass
	// silently as success.
	wait := exec.Command("aws", "cloudformation", "wait", "stack-delete-complete", "--stack-name", stackName, "--region", region)
	wait.Env = env
	wait.Stdout = GinkgoWriter
	wait.Stderr = GinkgoWriter
	return wait.Run()
}

// computeRolesRef derives the operator role ARNs from the prefix + account ID,
// mirroring rosa's deterministic role naming.
func computeRolesRef(prefix, accountID string) hypershiftv1beta1.AWSRolesRef {
	arn := func(suffix string) string {
		return fmt.Sprintf("arn:%s:iam::%s:role/%s-%s", awsPartition, accountID, prefix, suffix)
	}
	return hypershiftv1beta1.AWSRolesRef{
		IngressARN:              arn(suffixIngress),
		ImageRegistryARN:        arn(suffixImageRegistry),
		StorageARN:              arn(suffixEBSCSI),
		NetworkARN:              arn(suffixNetworkConfig),
		KubeCloudControllerARN:  arn(suffixCloudController),
		NodePoolManagementARN:   arn(suffixNodePoolManagement),
		ControlPlaneOperatorARN: arn(suffixControlPlaneOperator),
	}
}

// computeInstanceProfile derives the worker instance profile name (same as the
// worker role name) from the prefix.
func computeInstanceProfile(prefix string) string {
	return prefix + "-" + suffixWorkerRole
}

type cfOutput struct {
	OutputKey   string `json:"OutputKey"`
	OutputValue string `json:"OutputValue"`
}

// cfStackOutputs returns the Outputs of a CloudFormation stack.
func cfStackOutputs(env []string, stackName, region string) ([]cfOutput, error) {
	cmd := exec.Command("aws", "cloudformation", "describe-stacks",
		"--stack-name", stackName,
		"--region", region,
		"--query", "Stacks[0].Outputs",
		"--output", "json",
	)
	cmd.Env = env
	raw, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("cloudformation describe-stacks %s: %w", stackName, err)
	}
	var outputs []cfOutput
	if err := json.Unmarshal(raw, &outputs); err != nil {
		return nil, fmt.Errorf("parsing %s outputs: %w", stackName, err)
	}
	return outputs, nil
}

// networkOutputs reads the VPC ID and first private subnet ID from the network
// stack created by `rosa create network`.
func networkOutputs(env []string, stackName, region string) (vpcID, subnetID string, err error) {
	out, err := cfStackOutputs(env, stackName, region)
	if err != nil {
		return "", "", err
	}
	for _, o := range out {
		switch o.OutputKey {
		case "VPCId":
			vpcID = o.OutputValue
		case "PrivateSubnets":
			// Comma-joined subnet IDs; the single-AZ cluster takes the first.
			subnetID = strings.TrimSpace(strings.SplitN(o.OutputValue, ",", 2)[0])
		}
	}
	if vpcID == "" || subnetID == "" {
		return "", "", fmt.Errorf("VPCId or PrivateSubnets not found in %s outputs", stackName)
	}
	return vpcID, subnetID, nil
}

// fireAndForget reports whether Phase 2 infra teardown should skip waiting for
// the VPC stack deletion to complete (ROSA_REGIONAL_TEARDOWN_FIRE_AND_FORGET).
func fireAndForget() bool {
	return os.Getenv("ROSA_REGIONAL_TEARDOWN_FIRE_AND_FORGET") == "true"
}

// --- clientset delete helpers (each waits until the object is gone) ---

func deleteCluster(ctx context.Context, cs *seaplane.Clientset, name string) error {
	clusters := cs.SeaplaneV1alpha1().Clusters()
	if err := clusters.Delete(ctx, name, platform.DeleteOptions{}); err != nil {
		return fmt.Errorf("cluster delete: %w", err)
	}
	return clusters.WaitUntil(ctx, name,
		func(c *v1alpha1.Cluster) bool {
			if c == nil {
				GinkgoWriter.Printf("Cluster %s deleted\n", name)
				return true
			}
			GinkgoWriter.Printf("[%s] Cluster %s: phase=%s, waiting for deletion\n",
				time.Now().Format(time.RFC3339), name, c.Status.Phase)
			return false
		},
		clusterDeletePoll, clusterDeleteTimeout,
	)
}

func deleteDNSReservation(ctx context.Context, cs *seaplane.Clientset, name string) error {
	reservations := cs.SeaplaneV1alpha1().DNSReservations()
	if err := reservations.Delete(ctx, name, platform.DeleteOptions{}); err != nil {
		return fmt.Errorf("DNSReservation delete: %w", err)
	}
	return reservations.WaitUntil(ctx, name,
		func(r *v1alpha1.DNSReservation) bool {
			if r == nil {
				GinkgoWriter.Printf("DNSReservation %s deleted\n", name)
				return true
			}
			GinkgoWriter.Printf("[%s] DNSReservation %s: phase=%s, waiting for deletion\n",
				time.Now().Format(time.RFC3339), name, r.Status.Phase)
			return false
		},
		dnsReservationDeletePoll, dnsReservationDeleteTimeout,
	)
}
