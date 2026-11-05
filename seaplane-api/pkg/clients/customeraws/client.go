// Package customeraws obtains customer credentials using regional workload identity.
package customeraws

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/arn"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

type Client struct {
	Config     aws.Config
	AccountID  string
	SessionARN string
}

type Factory struct {
	region     string
	tokenFile  string
	httpClient aws.HTTPClient
}

func New(region, tokenFile string) *Factory {
	return &Factory{region: region, tokenFile: tokenFile, httpClient: &http.Client{Timeout: 10 * time.Second}}
}

// The SDK file retriever formats read errors without wrapping them. Preserve
// service-side classification here without retaining the path or token in errors.
type identityTokenFile string

func (f identityTokenFile) GetIdentityToken() ([]byte, error) {
	token, err := stscreds.IdentityTokenFile(f).GetIdentityToken()
	if err != nil || len(token) == 0 || len(token) > 1<<20 {
		return nil, &Error{Kind: IdentityUnavailable}
	}
	return token, nil
}

var (
	accountPattern  = regexp.MustCompile(`^[0-9]{12}$`)
	roleNamePattern = regexp.MustCompile(`^[A-Za-z0-9_+=,.@-]{1,64}$`)
)

// ValidateRoleARN accepts only commercial AWS IAM roles owned by the caller.
func ValidateRoleARN(value, account string) error {
	a, err := arn.Parse(value)
	if err != nil || len(value) > 2048 || !accountPattern.MatchString(account) || a.Partition != "aws" || a.Service != "iam" || a.Region != "" || a.AccountID != account || !strings.HasPrefix(a.Resource, "role/") {
		return fmt.Errorf("installerRoleArn must identify an IAM role in the caller's AWS account")
	}
	resource := strings.TrimPrefix(a.Resource, "role/")
	lastSlash := strings.LastIndex(resource, "/")
	name := resource[lastSlash+1:]
	// IAM paths include the leading and trailing slash, with at most 512 bytes.
	if !roleNamePattern.MatchString(name) || lastSlash+2 > 512 {
		return fmt.Errorf("installerRoleArn has an invalid IAM role name or path")
	}
	for _, c := range resource[:lastSlash+1] {
		if c < '!' || c > '~' {
			return fmt.Errorf("installerRoleArn has an invalid IAM role path")
		}
	}
	return nil
}

// Get freshly assumes this request's role. No platform credentials or
// cross-request credential cache participate in the exchange.
func (f *Factory) Get(ctx context.Context, roleARN, account string) (*Client, error) {
	if err := ValidateRoleARN(roleARN, account); err != nil {
		return nil, &Error{Kind: AccountMismatch}
	}
	cfg := aws.Config{
		Region:      f.region,
		HTTPClient:  f.httpClient,
		Credentials: aws.AnonymousCredentials{},
		Retryer: func() aws.Retryer {
			return retry.NewStandard(func(o *retry.StandardOptions) { o.MaxAttempts = 2 })
		},
	}
	provider := stscreds.NewWebIdentityRoleProvider(sts.NewFromConfig(cfg), roleARN, identityTokenFile(f.tokenFile), func(o *stscreds.WebIdentityRoleOptions) {
		o.RoleSessionName = fmt.Sprintf("seaplane-%d", time.Now().UnixNano())
		o.Duration = 15 * time.Minute
	})
	credentials := aws.NewCredentialsCache(provider, func(o *aws.CredentialsCacheOptions) { o.ExpiryWindow = time.Minute })
	if _, err := credentials.Retrieve(ctx); err != nil {
		return nil, Classify(err, TrustFailed)
	}
	cfg.Credentials = credentials
	identity, err := sts.NewFromConfig(cfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return nil, Classify(err, DependencyUnavailable)
	}
	if identity == nil || aws.ToString(identity.Account) == "" || aws.ToString(identity.Arn) == "" {
		return nil, &Error{Kind: InvalidResponse}
	}
	if aws.ToString(identity.Account) != account {
		return nil, &Error{Kind: AccountMismatch}
	}
	return &Client{Config: cfg, AccountID: account, SessionARN: aws.ToString(identity.Arn)}, nil
}
