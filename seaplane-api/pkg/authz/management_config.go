package authz

import (
	"fmt"
	"regexp"
)

// ServiceConfig identifies the authorization tables.
type ServiceConfig struct {
	PolicyTable, GlobalTable, RegionalTable string
}

var tablePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{3,255}$`)

// checkServiceConfig validates configuration independently of backend availability.
func checkServiceConfig(cfg ServiceConfig) error {
	tables := map[string]bool{}
	for _, table := range []string{cfg.PolicyTable, cfg.GlobalTable, cfg.RegionalTable} {
		if !tablePattern.MatchString(table) || tables[table] {
			return fmt.Errorf("%w: three distinct DynamoDB table names required", ErrValidation)
		}
		tables[table] = true
	}
	return nil
}
