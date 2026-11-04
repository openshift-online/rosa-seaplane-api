package authz

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestDocumentationExamplesPinnedCedar(t *testing.T) {
	content, err := os.ReadFile("../../../docs/authz/README.md")
	if err != nil {
		t.Fatal(err)
	}
	examples := regexp.MustCompile("(?s)```cedar\\n(.*?)\\n```").FindAllSubmatch(content, -1)
	if len(examples) != 9 {
		t.Fatalf("expected 9 runnable examples, found %d", len(examples))
	}
	for _, example := range examples {
		policy := string(example[1])
		t.Run(strings.Split(policy, "\n")[0], func(t *testing.T) {
			b := bundleFixture()
			b["policies"].([]map[string]any)[0]["content"] = policy
			a := fixtureAuthorizer(t, b)
			p, err := a.Prepare(t.Context(), testIdentity(sessionARN), testRequestContext())
			if err != nil {
				t.Fatal(err)
			}
			np := Resource{Kind: NodePool, ID: "workers", AccountID: accountID, ParentCluster: &ParentCluster{ID: "550e8400-e29b-41d4-a716-446655440000", AccountID: accountID}}
			for _, check := range []struct {
				action   Action
				resource Resource
			}{{ListClusters, testCollection()}, {DescribeCluster, testCluster()}, {DeleteCluster, testCluster()}, {UpdateNodePool, np}, {ScaleNodePool, np}} {
				if _, err := p.Check(t.Context(), check.action, check.resource); err != nil {
					t.Fatalf("example failed evaluation %s: %v", check.action, err)
				}
			}
		})
	}
}
