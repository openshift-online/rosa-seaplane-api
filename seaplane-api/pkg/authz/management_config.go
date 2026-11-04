package authz

import (
	"bytes"
	"fmt"
	"os"
	"regexp"

	"gopkg.in/yaml.v3"
)

// ManagementAccount configures the global writer and bootstrap administrators for an account.
type ManagementAccount struct {
	WriterRegion      string   `yaml:"writerRegion"`
	AdministratorARNs []string `yaml:"administratorARNs"`
}

// ManagementConfig identifies the stores and account-scoped bootstrap administrators.
type ManagementConfig struct {
	FormatVersion int                          `yaml:"formatVersion"`
	PolicyTable   string                       `yaml:"policyTable"`
	GlobalTable   string                       `yaml:"globalTable"`
	RegionalTable string                       `yaml:"regionalTable"`
	Accounts      map[string]ManagementAccount `yaml:"accounts"`
}

var tablePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{3,255}$`)

// LoadManagementConfig loads one strict, versioned YAML management configuration.
func LoadManagementConfig(path string) (ManagementConfig, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return ManagementConfig{}, err
	}
	if _, err := readBundleNode(content); err != nil {
		return ManagementConfig{}, fmt.Errorf("%w: %v", ErrValidation, err)
	}
	decoder := yaml.NewDecoder(bytes.NewReader(content))
	decoder.KnownFields(true)
	var cfg ManagementConfig
	if err := decoder.Decode(&cfg); err != nil {
		return ManagementConfig{}, fmt.Errorf("%w: %v", ErrValidation, err)
	}
	if err := checkManagementConfig(cfg); err != nil {
		return ManagementConfig{}, err
	}
	return cfg, nil
}

// checkManagementConfig validates configuration independently of backend availability.
func checkManagementConfig(cfg ManagementConfig) error {
	if cfg.FormatVersion != 1 || cfg.Accounts == nil {
		return fmt.Errorf("%w: version 1 and accounts are required", ErrValidation)
	}
	tables := map[string]bool{}
	for _, table := range []string{cfg.PolicyTable, cfg.GlobalTable, cfg.RegionalTable} {
		if !tablePattern.MatchString(table) || tables[table] {
			return fmt.Errorf("%w: three distinct DynamoDB table names required", ErrValidation)
		}
		tables[table] = true
	}
	for account, cfg := range cfg.Accounts {
		if !accountPattern.MatchString(account) || !regionPattern.MatchString(cfg.WriterRegion) || cfg.AdministratorARNs == nil {
			return fmt.Errorf("%w: invalid account configuration", ErrValidation)
		}
		seen := map[string]bool{}
		aliases := map[string]string{}
		for _, value := range cfg.AdministratorARNs {
			principal, err := parsePrincipal(value)
			if err != nil || principal.account != account || seen[value] {
				return fmt.Errorf("%w: invalid or duplicate administrator ARN", ErrValidation)
			}
			seen[value] = true
			if principal.kind == "role" {
				if err := addRoleAlias(aliases, principal.roleAlias(), principal.original); err != nil {
					return fmt.Errorf("%w: %v", ErrValidation, err)
				}
			}
		}
	}
	return nil
}
