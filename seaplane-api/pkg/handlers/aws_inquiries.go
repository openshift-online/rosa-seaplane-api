package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/api"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/authz"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/clients/customeraws"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/discovery"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/middleware"
)

type VPCInquiryRequest struct {
	InstallerRoleARN string `json:"installerRoleArn"`
}

type AWSInquiriesHandler struct {
	customers  CustomerAWSFactory
	authorizer *authz.Authorizer
	region     string
	logger     *slog.Logger
}

func NewAWSInquiriesHandler(customers CustomerAWSFactory, authorizer *authz.Authorizer, region string, logger *slog.Logger) *AWSInquiriesHandler {
	return &AWSInquiriesHandler{customers: customers, authorizer: authorizer, region: region, logger: logger}
}

func (h *AWSInquiriesHandler) ListVPCs(w http.ResponseWriter, r *http.Request) {
	account := middleware.GetAccountID(r.Context())
	prepared, attempt := authorizeCollection(w, r, h.authorizer, h.logger, authz.ListAWSVPCs, authz.Resource{Kind: authz.Collection, CollectionKind: authz.AWSVPC, AccountID: account})
	if prepared == nil {
		return
	}
	defer attempt.Finish(authz.OutcomeAllow, authz.StageNone)

	var request VPCInquiryRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	err := decoder.Decode(&request)
	if err == nil {
		var extra any
		if nextErr := decoder.Decode(&extra); nextErr != io.EOF {
			if nextErr == nil {
				nextErr = errors.New("multiple JSON values")
			}
			err = nextErr
		}
	}
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeAPIError(w, api.ErrRequestBodyTooLarge, h.logger)
		} else {
			writeAPIError(w, errAWSInquiryInput, h.logger)
		}
		return
	}
	if err := customeraws.ValidateRoleARN(request.InstallerRoleARN, account); err != nil {
		writeAPIError(w, errAWSInquiryInput, h.logger)
		return
	}
	logger := h.logger.With("account_id", account, "caller_arn", middleware.GetCallerARN(r.Context()), "installer_role_arn", request.InstallerRoleARN, "region", h.region)
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	customer, err := h.customers.Get(ctx, request.InstallerRoleARN, account)
	if err != nil {
		writeAPIError(w, customerAWSError(logger, err), logger)
		return
	}
	result, err := discovery.ListVPCs(ctx, ec2.NewFromConfig(customer.Config))
	if err != nil {
		writeAPIError(w, customerAWSError(logger, err), logger)
		return
	}
	logger.Info("customer VPC discovery completed", "session_arn", customer.SessionARN, "vpc_count", result.Total)
	if err := api.Write(w, http.StatusOK, result); err != nil {
		logger.Error("failed to write VPC discovery response", "error", err)
	}
}
