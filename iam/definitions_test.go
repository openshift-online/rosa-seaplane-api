package iam

import (
	"reflect"
	"testing"
)

// TestEmbeddedDefinitionSetValid guards the pinned floor: a malformed set
// can never ship.
func TestEmbeddedDefinitionSetValid(t *testing.T) {
	if err := ValidateEmbedded(); err != nil {
		t.Fatalf("embedded definition set invalid: %v", err)
	}
}

// TestDefaultDefinitionSetMatchesCLI pins the embedded set to the rosa
// CLI's getHCPOperatorRoles() behavior (D2 contract: pure refactor, no
// behavioral change). Suffix order must match seaplane.OperatorRoleSuffixes().
func TestDefaultDefinitionSetMatchesCLI(t *testing.T) {
	set := DefaultDefinitionSet()

	wantSuffixes := []string{
		"ingress",
		"cloud-controller-manager",
		"ebs-csi",
		"image-registry",
		"network-config",
		"control-plane-operator",
		"node-pool-management",
		"ROSA-Worker-Role",
	}
	gotSuffixes := make([]string, len(set.Roles))
	for i, role := range set.Roles {
		gotSuffixes[i] = role.Name
	}
	if !reflect.DeepEqual(wantSuffixes, gotSuffixes) {
		t.Fatalf("suffix order mismatch:\nwant %v\ngot  %v", wantSuffixes, gotSuffixes)
	}

	wantPolicies := map[string][]string{
		"ingress":                  {"service-role/ROSAIngressOperatorPolicy"},
		"cloud-controller-manager": {"service-role/ROSAKubeControllerPolicy"},
		"ebs-csi":                  {"service-role/ROSAAmazonEBSCSIDriverOperatorPolicy"},
		"image-registry":           {"service-role/ROSAImageRegistryOperatorPolicy"},
		"network-config":           {"service-role/ROSACloudNetworkConfigOperatorPolicy"},
		"control-plane-operator":   {"service-role/ROSAControlPlaneOperatorPolicy"},
		"node-pool-management":     {"service-role/ROSANodePoolManagementPolicy"},
		"ROSA-Worker-Role":         {"service-role/ROSAWorkerInstancePolicy", "AmazonSSMManagedInstanceCore"},
	}
	for _, role := range set.Roles {
		want := wantPolicies[role.Name]
		if !reflect.DeepEqual(want, role.ManagedPolicies) {
			t.Errorf("role %q policies mismatch:\nwant %v\ngot  %v", role.Name, want, role.ManagedPolicies)
		}
	}

	// Worker role: EC2 trust, no service accounts; exactly one worker role.
	workers := 0
	for _, role := range set.Roles {
		if role.IsWorkerRole {
			workers++
			if len(role.ServiceAccounts) != 0 {
				t.Errorf("worker role %q must not have service accounts", role.Name)
			}
		}
	}
	if workers != 1 {
		t.Fatalf("expected exactly 1 worker role, got %d", workers)
	}
}

// TestDefaultDefinitionSetIsACopy ensures callers cannot mutate the floor.
func TestDefaultDefinitionSetIsACopy(t *testing.T) {
	set := DefaultDefinitionSet()
	set.Roles[0].ServiceAccounts[0] = "mutated"
	set.Roles[0].ManagedPolicies[0] = "mutated"

	fresh := DefaultDefinitionSet()
	if fresh.Roles[0].ServiceAccounts[0] == "mutated" || fresh.Roles[0].ManagedPolicies[0] == "mutated" {
		t.Fatal("DefaultDefinitionSet returned shared state; mutations leaked into the embedded set")
	}
}

func TestDefinitionSetValidate(t *testing.T) {
	tests := []struct {
		name    string
		set     DefinitionSet
		wantErr bool
	}{
		{
			name: "valid",
			set: DefinitionSet{
				Version: "1.0.0",
				Roles:   []OperatorRoleSpec{{Name: "ingress", ServiceAccounts: []string{"sa"}, ManagedPolicies: []string{"p"}}},
			},
		},
		{name: "no version", set: DefinitionSet{Roles: []OperatorRoleSpec{{Name: "x"}}}, wantErr: true},
		{name: "no roles", set: DefinitionSet{Version: "1.0.0"}, wantErr: true},
		{
			name: "duplicate role",
			set: DefinitionSet{
				Version: "1.0.0",
				Roles: []OperatorRoleSpec{
					{Name: "x", ServiceAccounts: []string{"sa"}, ManagedPolicies: []string{"p"}},
					{Name: "x", ServiceAccounts: []string{"sa"}, ManagedPolicies: []string{"p"}},
				},
			},
			wantErr: true,
		},
		{
			name: "non-worker role without service accounts",
			set: DefinitionSet{
				Version: "1.0.0",
				Roles:   []OperatorRoleSpec{{Name: "x", ManagedPolicies: []string{"p"}}},
			},
			wantErr: true,
		},
		{
			name: "worker role with service accounts",
			set: DefinitionSet{
				Version: "1.0.0",
				Roles:   []OperatorRoleSpec{{Name: "x", ServiceAccounts: []string{"sa"}, ManagedPolicies: []string{"p"}, IsWorkerRole: true}},
			},
			wantErr: true,
		},
		{
			name: "role without policies",
			set: DefinitionSet{
				Version: "1.0.0",
				Roles:   []OperatorRoleSpec{{Name: "x", ServiceAccounts: []string{"sa"}}},
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.set.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestDefinitionSetRole(t *testing.T) {
	set := DefaultDefinitionSet()
	role, ok := set.Role(SuffixIngress)
	if !ok {
		t.Fatal("expected ingress role to be found")
	}
	if len(role.ServiceAccounts) != 1 {
		t.Fatalf("expected 1 service account for ingress, got %d", len(role.ServiceAccounts))
	}
	if _, ok := set.Role("does-not-exist"); ok {
		t.Fatal("expected unknown role lookup to fail")
	}
}

func TestDefinitionSetRoleNames(t *testing.T) {
	set := DefaultDefinitionSet()
	names := set.RoleNames("my-prefix")
	if len(names) != len(set.Roles) {
		t.Fatalf("expected %d names, got %d", len(set.Roles), len(names))
	}
	if names[0] != "my-prefix-ingress" {
		t.Errorf("expected first role 'my-prefix-ingress', got %q", names[0])
	}
}
