package iam

import (
	"reflect"
	"testing"
)

func TestRoleNameAndARN(t *testing.T) {
	if got := RoleName("myprefix", SuffixIngress); got != "myprefix-ingress" {
		t.Errorf("RoleName = %q", got)
	}
	if got := RoleARN("aws", "123456789012", "myprefix", SuffixIngress); got != "arn:aws:iam::123456789012:role/myprefix-ingress" {
		t.Errorf("RoleARN = %q", got)
	}
}

func TestRoleNames(t *testing.T) {
	names := RoleNames("pfx")
	want := []string{
		"pfx-ingress",
		"pfx-cloud-controller-manager",
		"pfx-ebs-csi",
		"pfx-image-registry",
		"pfx-network-config",
		"pfx-control-plane-operator",
		"pfx-node-pool-management",
		"pfx-ROSA-Worker-Role",
	}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("RoleNames mismatch:\nwant %v\ngot  %v", want, names)
	}
}

func TestRoleTags(t *testing.T) {
	tags := RoleTags("pfx")
	want := map[string]string{
		"red-hat-managed":         "true",
		"rosa_hcp_policies":       "true",
		"rosa_role_prefix":        "pfx",
		"rosa_managed_policies":   "true",
		"rosa_definition_version": DefinitionVersion(),
	}
	if !reflect.DeepEqual(tags, want) {
		t.Fatalf("RoleTags mismatch:\nwant %v\ngot  %v", want, tags)
	}
}

func TestComputeRolesRef(t *testing.T) {
	ref := ComputeRolesRef("pfx", "123456789012", "aws")
	want := RolesRef{
		IngressARN:              "arn:aws:iam::123456789012:role/pfx-ingress",
		KubeCloudControllerARN:  "arn:aws:iam::123456789012:role/pfx-cloud-controller-manager",
		StorageARN:              "arn:aws:iam::123456789012:role/pfx-ebs-csi",
		ImageRegistryARN:        "arn:aws:iam::123456789012:role/pfx-image-registry",
		NetworkARN:              "arn:aws:iam::123456789012:role/pfx-network-config",
		ControlPlaneOperatorARN: "arn:aws:iam::123456789012:role/pfx-control-plane-operator",
		NodePoolManagementARN:   "arn:aws:iam::123456789012:role/pfx-node-pool-management",
	}
	if !reflect.DeepEqual(ref, want) {
		t.Fatalf("ComputeRolesRef mismatch:\nwant %+v\ngot  %+v", want, ref)
	}
}

func TestComputeInstanceProfileAndWorkerRoleName(t *testing.T) {
	if got := ComputeInstanceProfile("pfx"); got != "pfx-ROSA-Worker-Role" {
		t.Errorf("ComputeInstanceProfile = %q", got)
	}
	if got := WorkerRoleName("pfx"); got != "pfx-ROSA-Worker-Role" {
		t.Errorf("WorkerRoleName = %q", got)
	}
}

func TestPrefixAndInstanceProfileFromRolesRef(t *testing.T) {
	ref := ComputeRolesRef("pfx", "123456789012", "aws")

	if got := OperatorRolesPrefixFromRolesRef(ref); got != "pfx" {
		t.Errorf("OperatorRolesPrefixFromRolesRef = %q, want pfx", got)
	}
	if got := InstanceProfileFromRolesRef(ref); got != "pfx-ROSA-Worker-Role" {
		t.Errorf("InstanceProfileFromRolesRef = %q", got)
	}

	// Unset / unparseable ARNs yield empty results.
	if got := OperatorRolesPrefixFromRolesRef(RolesRef{}); got != "" {
		t.Errorf("expected empty prefix for empty RolesRef, got %q", got)
	}
	if got := OperatorRolesPrefixFromRolesRef(RolesRef{NodePoolManagementARN: "not-an-arn"}); got != "" {
		t.Errorf("expected empty prefix for unparseable ARN, got %q", got)
	}
	if got := OperatorRolesPrefixFromRolesRef(RolesRef{NodePoolManagementARN: "arn:aws:iam::1:role/other-suffix"}); got != "" {
		t.Errorf("expected empty prefix for non-node-pool role, got %q", got)
	}
}
