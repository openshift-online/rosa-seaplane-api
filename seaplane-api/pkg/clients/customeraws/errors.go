package customeraws

import (
	"context"
	"errors"
	"net"
	"os"
	"strings"

	"github.com/aws/smithy-go"
)

type ErrorKind string

const (
	IdentityUnavailable   ErrorKind = "REGIONAL_IDENTITY_UNAVAILABLE"
	TrustFailed           ErrorKind = "CUSTOMER_ACCESS_TRUST_FAILED"
	AccountMismatch       ErrorKind = "CUSTOMER_ACCESS_ACCOUNT_MISMATCH"
	PermissionDenied      ErrorKind = "CUSTOMER_ACCESS_PERMISSION_DENIED"
	DependencyUnavailable ErrorKind = "CUSTOMER_ACCESS_DEPENDENCY_UNAVAILABLE"
	Timeout               ErrorKind = "CUSTOMER_ACCESS_TIMEOUT"
	InvalidResponse       ErrorKind = "CUSTOMER_ACCESS_INVALID_RESPONSE"
)

// Error carries safe classification only, never raw AWS messages or credentials.
type Error struct {
	Kind    ErrorKind
	AWSCode string
}

func (e *Error) Error() string { return string(e.Kind) }

func Classify(err error, fallback ErrorKind) error {
	var known *Error
	if errors.As(err, &known) {
		return known
	}
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		return &Error{Kind: IdentityUnavailable}
	}
	var malformed *smithy.DeserializationError
	if errors.As(err, &malformed) {
		return &Error{Kind: InvalidResponse}
	}
	var network net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) || (errors.As(err, &network) && network.Timeout()) {
		return &Error{Kind: Timeout}
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		code := apiErr.ErrorCode()
		kind := fallback
		switch {
		case strings.Contains(code, "Throttl"), code == "RequestLimitExceeded", code == "ServiceUnavailable", code == "InternalError", code == "IDPCommunicationError":
			kind = DependencyUnavailable
		case code == "AccessDenied", code == "AccessDeniedException", code == "UnauthorizedOperation":
			if fallback != TrustFailed {
				kind = PermissionDenied
			}
		case code == "InvalidIdentityToken", code == "ExpiredToken", code == "IDPRejectedClaim":
			kind = TrustFailed
		}
		return &Error{Kind: kind, AWSCode: code}
	}
	if errors.As(err, &network) {
		return &Error{Kind: DependencyUnavailable}
	}
	return &Error{Kind: fallback}
}
