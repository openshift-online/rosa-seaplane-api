package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCustomerAccessConfiguration(t *testing.T) {
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("workload-token"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "projected-token")
	if err := os.Symlink(token, link); err != nil {
		t.Fatal(err)
	}
	valid := NewConfig().CustomerAccess
	valid.IssuerURL, valid.Subject, valid.TokenFile = "https://issuer.example/id/region", "system:serviceaccount:platform-api:platform-api-sa", link
	for _, tc := range []struct {
		name   string
		change func(*CustomerAccessConfig)
		fail   bool
	}{
		{"projected symlink", func(*CustomerAccessConfig) {}, false},
		{"missing configuration", func(c *CustomerAccessConfig) { *c = CustomerAccessConfig{} }, true},
		{"http issuer", func(c *CustomerAccessConfig) { c.IssuerURL = "http://issuer.example" }, true},
		{"empty fragment", func(c *CustomerAccessConfig) { c.IssuerURL += "#" }, true},
		{"trailing slash", func(c *CustomerAccessConfig) { c.IssuerURL += "/" }, true},
		{"wildcard subject", func(c *CustomerAccessConfig) { c.Subject = "system:serviceaccount:platform-api:*" }, true},
		{"audience", func(c *CustomerAccessConfig) { c.Audience = "kubernetes" }, true},
		{"missing file", func(c *CustomerAccessConfig) { c.TokenFile += "-missing" }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := valid
			tc.change(&c)
			if err := c.Validate(); (err != nil) != tc.fail {
				t.Fatalf("Validate()=%v", err)
			}
		})
	}
}
