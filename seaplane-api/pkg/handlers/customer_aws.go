package handlers

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/clients/customeraws"
)

type CustomerAWSFactory interface {
	Get(context.Context, string, string) (*customeraws.Client, error)
}

var errAWSInquiryInput = APIError{
	Code: "AWS_INQUIRY_INVALID_INPUT", HTTPStatus: http.StatusBadRequest,
	Message: "Supply one JSON object containing installerRoleArn for an IAM role in your AWS account",
}

func customerAWSError(logger *slog.Logger, err error) APIError {
	var accessErr *customeraws.Error
	if !errors.As(err, &accessErr) {
		logger.Error("customer AWS operation failed", "code", "CUSTOMER_ACCESS_INTERNAL")
		return APIError{Code: "CUSTOMER_ACCESS_INTERNAL", HTTPStatus: 500, Message: "Customer AWS operation failed"}
	}
	status := http.StatusUnprocessableEntity
	switch accessErr.Kind {
	case customeraws.IdentityUnavailable, customeraws.DependencyUnavailable:
		status = http.StatusServiceUnavailable
	case customeraws.Timeout:
		status = http.StatusGatewayTimeout
	case customeraws.InvalidResponse:
		status = http.StatusBadGateway
	case customeraws.TrustFailed, customeraws.AccountMismatch, customeraws.PermissionDenied:
	default:
		return APIError{Code: "CUSTOMER_ACCESS_INTERNAL", HTTPStatus: 500, Message: "Customer AWS operation failed"}
	}
	logger.Warn("customer AWS operation failed", "code", accessErr.Kind, "aws_code", accessErr.AWSCode)
	return APIError{Code: string(accessErr.Kind), HTTPStatus: status, Message: "Customer AWS operation failed"}
}
