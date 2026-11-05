package config

import (
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	DefaultWorkloadAudience        = "sts.amazonaws.com"
	DefaultWorkloadContractVersion = "regional-workload-v1"
	DefaultCustomerAccessTokenFile = "/var/run/seaplane/customer-access/token"
)

// CustomerAccessConfig describes the regional workload, never a customer role.
type CustomerAccessConfig struct {
	IssuerURL       string
	Subject         string
	Audience        string
	ContractVersion string
	TokenFile       string
}

var identityVersionPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

func (c CustomerAccessConfig) Validate() error {
	u, err := url.Parse(c.IssuerURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || strings.ContainsAny(c.IssuerURL, "?#") || strings.HasSuffix(c.IssuerURL, "/") {
		return fmt.Errorf("regional workload issuer must be an HTTPS URL without userinfo, query, fragment, or trailing slash")
	}
	parts := strings.Split(c.Subject, ":")
	if len(parts) != 4 || parts[0] != "system" || parts[1] != "serviceaccount" || len(validation.IsDNS1123Label(parts[2])) != 0 || len(validation.IsDNS1123Subdomain(parts[3])) != 0 {
		return fmt.Errorf("regional workload subject must identify an exact Kubernetes service account")
	}
	if c.Audience != DefaultWorkloadAudience || !identityVersionPattern.MatchString(c.ContractVersion) {
		return fmt.Errorf("regional workload requires sts.amazonaws.com audience and a 1-128 character identity version")
	}
	f, err := os.Open(c.TokenFile)
	if err != nil {
		return fmt.Errorf("regional workload token file is not readable")
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil || !stat.Mode().IsRegular() || stat.Size() == 0 || stat.Size() > 1<<20 {
		return fmt.Errorf("regional workload token must be a nonempty regular file of at most 1 MiB")
	}
	return nil
}
