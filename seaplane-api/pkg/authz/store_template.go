package authz

import (
	"fmt"
	"slices"
	"strings"
	"text/scanner"

	cedar "github.com/cedar-policy/cedar-go"
)

// bindPolicy validates a Cedar policy and binds any ?principal template slot to the supplied principal.
func (s *Store) bindPolicy(content, id string, principal principalARN) (*cedar.Policy, error) {
	if len(content) > maxPolicyBytes {
		return nil, fmt.Errorf("policy exceeds %d bytes", maxPolicyBytes)
	}
	start, end, operator, err := templateScope(content)
	if err != nil {
		return nil, err
	}
	if operator == "" {
		return checkedPolicy(content, id, s.validator)
	}
	policy, err := checkedPolicy(content[:start]+"principal"+content[end:], id, s.validator)
	if err != nil {
		return nil, err
	}
	kind := "Principal"
	if principal.kind == "role" {
		kind = "Role"
	}
	target := entityUID(kind, principal.original)
	ast := policy.AST()
	if operator == "in" {
		ast.PrincipalIn(target)
	} else {
		ast.PrincipalEq(target)
	}
	bound := cedar.NewPolicyFromAST(ast)
	if err := s.validator.Policy(id, expPolicy(bound)); err != nil {
		return nil, err
	}
	return bound, nil
}

// templateScope locates a principal == ?principal or principal in ?principal header.
// It skips annotations and comments and rejects other principal template forms.
func templateScope(content string) (int, int, string, error) {
	var scan scanner.Scanner
	scan.Init(strings.NewReader(content))
	scan.Mode = scanner.ScanIdents | scanner.ScanStrings | scanner.ScanComments | scanner.SkipComments
	var scanErr error
	scan.Error = func(_ *scanner.Scanner, message string) { scanErr = fmt.Errorf("template header: %s", message) }
	token := scan.Scan()
	for token == '@' {
		if scan.Scan() != scanner.Ident || scan.Scan() != '(' || scan.Scan() != scanner.String || scan.Scan() != ')' {
			return 0, 0, "", fmt.Errorf("invalid policy annotation")
		}
		token = scan.Scan()
	}
	if token != scanner.Ident || (scan.TokenText() != "permit" && scan.TokenText() != "forbid") || scan.Scan() != '(' {
		return 0, 0, "", nil
	}
	token = scan.Scan()
	start := scan.Offset
	header := []string{}
	for token != ',' && token != scanner.EOF && len(header) < 10 {
		header = append(header, scan.TokenText())
		token = scan.Scan()
	}
	if scanErr != nil {
		return 0, 0, "", scanErr
	}
	if !slices.Contains(header, "?") {
		return 0, 0, "", nil
	}
	if token == ',' && slices.Equal(header, []string{"principal", "in", "?", "principal"}) {
		return start, scan.Offset, "in", nil
	}
	if token == ',' && slices.Equal(header, []string{"principal", "=", "=", "?", "principal"}) {
		return start, scan.Offset, "==", nil
	}
	return 0, 0, "", fmt.Errorf("template requires principal == ?principal or principal in ?principal")
}
