package handlers

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gorilla/mux"
	hypershiftv1beta1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	seaplanev1alpha1 "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1"
	public "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1/public"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/authz"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/clients/seaplanedb"
)

func TestValidateNodePoolReplicas(t *testing.T) {
	negative := int32(-1)
	zero := int32(0)
	positive := int32(3)
	tests := []struct {
		name      string
		spec      *public.NodePoolSpec
		wantError bool
	}{
		{name: "nil spec"},
		{name: "replicas omitted", spec: &public.NodePoolSpec{}},
		{name: "zero replicas", spec: &public.NodePoolSpec{NodePool: public.NodePoolSpecPassthrough{Replicas: &zero}}},
		{name: "positive replicas", spec: &public.NodePoolSpec{NodePool: public.NodePoolSpecPassthrough{Replicas: &positive}}},
		{name: "negative replicas", spec: &public.NodePoolSpec{NodePool: public.NodePoolSpecPassthrough{Replicas: &negative}}, wantError: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errs := validateNodePoolReplicas(tt.spec)
			if gotError := len(errs) > 0; gotError != tt.wantError {
				t.Fatalf("validateNodePoolReplicas() returned errors %v, wantError=%t", errs, tt.wantError)
			}
			if tt.wantError && (errs[0].Field != "spec.nodePool.replicas" || errs[0].Reason != "must be greater than or equal to 0") {
				t.Fatalf("unexpected validation error: %+v", errs[0])
			}
		})
	}
}

func TestNodePoolHandlerCreateRejectsNegativeReplicas(t *testing.T) {
	const accountID = "123456789012"
	handler := newNodePoolValidationTestHandler(t)
	body := `{"metadata":{"name":"test-cluster.pool-a","namespace":"account-` + accountID + `"},"spec":{"nodePool":{"replicas":-1}}}`
	request := httptest.NewRequest(http.MethodPost, "/api/v0/nodepools", strings.NewReader(body))
	request = request.WithContext(testContext(accountID))
	response := httptest.NewRecorder()

	handler.Create(response, request)

	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("Create() status = %d, want %d; body=%s", response.Code, http.StatusUnprocessableEntity, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "spec.nodePool.replicas") {
		t.Fatalf("Create() response does not identify replicas: %s", response.Body.String())
	}
}

func TestNodePoolHandlerCreateRejectsInvalidEC2InstanceMetadataHttpTokens(t *testing.T) {
	const accountID = "123456789012"
	tests := []struct {
		name  string
		value string
	}{
		{name: "unsupported enum", value: `"disabled"`},
		{name: "explicit empty", value: `""`},
		{name: "explicit null", value: "null"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := newNodePoolValidationTestHandler(t)
			body := `{"metadata":{"name":"my-cluster.pool-a","namespace":"account-` + accountID + `"},"spec":{"ec2InstanceMetadataHttpTokens":` + tt.value + `}}`
			request := httptest.NewRequest(http.MethodPost, "/api/v0/nodepools", strings.NewReader(body))
			request = request.WithContext(testContext(accountID))
			response := httptest.NewRecorder()

			handler.Create(response, request)

			if response.Code != http.StatusUnprocessableEntity {
				t.Fatalf("Create() status = %d, want %d; body=%s", response.Code, http.StatusUnprocessableEntity, response.Body.String())
			}
			if !strings.Contains(response.Body.String(), "spec.ec2InstanceMetadataHttpTokens") {
				t.Fatalf("Create() response does not identify ec2InstanceMetadataHttpTokens: %s", response.Body.String())
			}
		})
	}
}

func TestNodePoolHandlerUpdateRejectsNegativeReplicas(t *testing.T) {
	const (
		accountID = "123456789012"
		poolName  = "pool-a"
	)
	object := &seaplanev1alpha1.NodePool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      poolName,
			Namespace: "account-" + accountID,
			Labels:    map[string]string{seaplanev1alpha1.AccountIDLabel: accountID},
		},
	}
	handler := newNodePoolValidationTestHandler(t, object)
	body := `{"spec":{"nodePool":{"replicas":-1}}}`
	request := httptest.NewRequest(http.MethodPut, "/api/v0/nodepools/"+poolName, strings.NewReader(body))
	request = request.WithContext(testContext(accountID))
	request = mux.SetURLVars(request, map[string]string{"id": poolName})
	response := httptest.NewRecorder()

	handler.Update(response, request)

	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("Update() status = %d, want %d; body=%s", response.Code, http.StatusUnprocessableEntity, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "spec.nodePool.replicas") {
		t.Fatalf("Update() response does not identify replicas: %s", response.Body.String())
	}
	if !json.Valid(response.Body.Bytes()) {
		t.Fatalf("Update() response contains multiple JSON documents: %s", response.Body.String())
	}
}

func TestNodePoolHandlerUpdateRejectsEC2InstanceMetadataHttpTokensChanges(t *testing.T) {
	const (
		accountID = "123456789012"
		poolName  = "pool-a"
	)
	tests := []struct {
		name  string
		value string
	}{
		{name: "change enum value", value: `"optional"`},
		{name: "clear with empty string", value: `""`},
		{name: "clear with null", value: "null"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			object := &seaplanev1alpha1.NodePool{
				ObjectMeta: metav1.ObjectMeta{
					Name:      poolName,
					Namespace: "account-" + accountID,
					Labels:    map[string]string{seaplanev1alpha1.AccountIDLabel: accountID},
				},
				Spec: seaplanev1alpha1.NodePoolSpec{
					EC2InstanceMetadataHttpTokens: seaplanev1alpha1.EC2InstanceMetadataHttpTokensRequired,
				},
			}
			handler := newNodePoolValidationTestHandler(t, object)
			body := `{"metadata":{"namespace":"account-` + accountID + `"},"spec":{"ec2InstanceMetadataHttpTokens":` + tt.value + `}}`
			request := httptest.NewRequest(http.MethodPut, "/api/v0/nodepools/"+poolName, strings.NewReader(body))
			request = request.WithContext(testContext(accountID))
			request = mux.SetURLVars(request, map[string]string{"id": poolName})
			response := httptest.NewRecorder()

			handler.Update(response, request)

			if response.Code != http.StatusUnprocessableEntity {
				t.Fatalf("Update() status = %d, want %d; body=%s", response.Code, http.StatusUnprocessableEntity, response.Body.String())
			}
			if !strings.Contains(response.Body.String(), "spec.ec2InstanceMetadataHttpTokens") {
				t.Errorf("Update() response does not identify ec2InstanceMetadataHttpTokens: %s", response.Body.String())
			}
			if !strings.Contains(response.Body.String(), "immutable") {
				t.Errorf("Update() response does not identify the field as immutable: %s", response.Body.String())
			}
		})
	}
}

func TestNodePoolHandlerUpdateAllowsUnchangedEC2InstanceMetadataHttpTokens(t *testing.T) {
	const (
		accountID = "123456789012"
		poolName  = "pool-a"
	)
	object := &seaplanev1alpha1.NodePool{
		ObjectMeta: metav1.ObjectMeta{
			Name:       poolName,
			Namespace:  "account-" + accountID,
			Labels:     map[string]string{seaplanev1alpha1.AccountIDLabel: accountID},
			Generation: 3,
		},
		Spec: seaplanev1alpha1.NodePoolSpec{
			EC2InstanceMetadataHttpTokens: seaplanev1alpha1.EC2InstanceMetadataHttpTokensRequired,
		},
	}
	handler := newNodePoolValidationTestHandler(t, object)
	body := `{"metadata":{"namespace":"account-` + accountID + `"},"spec":{"ec2InstanceMetadataHttpTokens":"required"}}`
	request := httptest.NewRequest(http.MethodPut, "/api/v0/nodepools/"+poolName, strings.NewReader(body))
	request = request.WithContext(testContext(accountID))
	request = mux.SetURLVars(request, map[string]string{"id": poolName})
	response := httptest.NewRecorder()

	handler.Update(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("Update() status = %d, want %d; body=%s", response.Code, http.StatusOK, response.Body.String())
	}
	stored, err := handler.db.GetNodePool(request.Context(), poolName)
	if err != nil {
		t.Fatalf("GetNodePool: %v", err)
	}
	if stored.Spec.EC2InstanceMetadataHttpTokens != seaplanev1alpha1.EC2InstanceMetadataHttpTokensRequired {
		t.Errorf("stored ec2InstanceMetadataHttpTokens = %q, want %q", stored.Spec.EC2InstanceMetadataHttpTokens, seaplanev1alpha1.EC2InstanceMetadataHttpTokensRequired)
	}
	if stored.Generation != 3 {
		t.Errorf("generation = %d, want 3 for an unchanged spec", stored.Generation)
	}
}

func TestNodePoolHandlerUpdateReturnsMultipleValidationErrors(t *testing.T) {
	const (
		accountID = "123456789012"
		poolName  = "pool-a"
	)
	object := &seaplanev1alpha1.NodePool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      poolName,
			Namespace: "account-" + accountID,
			Labels:    map[string]string{seaplanev1alpha1.AccountIDLabel: accountID},
		},
		Spec: seaplanev1alpha1.NodePoolSpec{
			NodePool: seaplanev1alpha1.NodePoolSpecPassthrough{
				Platform: hypershiftv1beta1.NodePoolPlatform{Type: hypershiftv1beta1.AWSPlatform},
			},
		},
	}
	handler := newNodePoolValidationTestHandler(t, object)
	body := `{"spec":{"nodePool":{"platform":{"type":"Azure"},"replicas":-1}}}`
	request := httptest.NewRequest(http.MethodPut, "/api/v0/nodepools/"+poolName, strings.NewReader(body))
	request = request.WithContext(testContext(accountID))
	request = mux.SetURLVars(request, map[string]string{"id": poolName})
	response := httptest.NewRecorder()

	handler.Update(response, request)

	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("Update() status = %d, want %d; body=%s", response.Code, http.StatusUnprocessableEntity, response.Body.String())
	}
	var status metav1.Status
	if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
		t.Fatalf("Update() response is not valid Status JSON: %v; body=%s", err, response.Body.String())
	}
	if status.Details == nil {
		t.Fatalf("Update() response has no validation causes: %s", response.Body.String())
	}
	gotCauses := make(map[string]metav1.StatusCause, len(status.Details.Causes))
	for _, cause := range status.Details.Causes {
		gotCauses[cause.Field] = cause
	}
	wantCauses := map[string]string{
		"spec.nodePool.platform.type": "field is immutable and cannot be changed after creation",
		"spec.nodePool.replicas":      "must be greater than or equal to 0",
	}
	for field, wantMessage := range wantCauses {
		cause, ok := gotCauses[field]
		if !ok {
			t.Errorf("Update() validation causes %v do not include %q", status.Details.Causes, field)
			continue
		}
		if cause.Type != metav1.CauseTypeFieldValueInvalid {
			t.Errorf("cause %q type = %q, want %q", field, cause.Type, metav1.CauseTypeFieldValueInvalid)
		}
		if cause.Message != wantMessage {
			t.Errorf("cause %q message = %q, want %q", field, cause.Message, wantMessage)
		}
	}
	if len(status.Details.Causes) != 2 {
		t.Errorf("Update() returned %d validation causes, want 2: %+v", len(status.Details.Causes), status.Details.Causes)
	}
}

// TestNodePoolHandlerCreateRejectsRootVolumeServiceSetFields verifies that a create request setting
// rootVolume.type/iops/encrypted/encryptionKey is rejected end-to-end via FieldValidator.
func TestNodePoolHandlerCreateRejectsRootVolumeServiceSetFields(t *testing.T) {
	const (
		accountID = "123456789012"
		poolName  = "test-cluster.pool-a"
	)
	handler := newNodePoolValidationTestHandler(t)

	tests := []struct {
		name      string
		rootVol   string
		wantField string
	}{
		{"type", `"size":500,"type":"io2"`, "spec.nodePool.platform.aws.rootVolume.type"},
		{"iops", `"size":500,"iops":3000`, "spec.nodePool.platform.aws.rootVolume.iops"},
		// Note: "encrypted":false is indistinguishable from omitted (bool zero value), so it's not rejected;
		// "encrypted":true is the value this check can actually detect as customer-supplied.
		{"encrypted", `"size":500,"encrypted":true`, "spec.nodePool.platform.aws.rootVolume.encrypted"},
		{"encryptionKey", `"size":500,"encryptionKey":"arn:aws:kms:us-east-1:123:key/abc"`, "spec.nodePool.platform.aws.rootVolume.encryptionKey"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := `{"metadata":{"name":"` + poolName + `","namespace":"account-` + accountID + `"},"spec":{"nodePool":{"platform":{"aws":{"rootVolume":{` + tt.rootVol + `}}}}}}`
			request := httptest.NewRequest(http.MethodPost, "/api/v0/nodepools", strings.NewReader(body))
			request = request.WithContext(testContext(accountID))
			response := httptest.NewRecorder()

			handler.Create(response, request)

			if response.Code != http.StatusUnprocessableEntity {
				t.Fatalf("Create() status = %d, want %d; body=%s", response.Code, http.StatusUnprocessableEntity, response.Body.String())
			}
			if !strings.Contains(response.Body.String(), tt.wantField) {
				t.Fatalf("Create() response does not identify %s: %s", tt.wantField, response.Body.String())
			}
		})
	}
}

// TestNodePoolHandlerCreateAllowsRootVolumeSizeOnly verifies a create request setting only
// rootVolume.size succeeds — size remains the one customer-settable field, matching v1.
func TestNodePoolHandlerCreateAllowsRootVolumeSizeOnly(t *testing.T) {
	const (
		accountID   = "123456789012"
		clusterName = "my-cluster"
	)
	cluster := &seaplanev1alpha1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      clusterName,
			Namespace: "account-" + accountID,
			Labels:    map[string]string{seaplanev1alpha1.AccountIDLabel: accountID},
		},
	}
	handler := newNodePoolValidationTestHandler(t, cluster)
	body := `{"metadata":{"name":"` + clusterName + `.pool-a","namespace":"account-` + accountID + `"},"spec":{"nodePool":{"platform":{"aws":{"rootVolume":{"size":500}}}}}}`
	request := httptest.NewRequest(http.MethodPost, "/api/v0/nodepools", strings.NewReader(body))
	request = request.WithContext(testContext(accountID))
	response := httptest.NewRecorder()

	handler.Create(response, request)

	if response.Code != http.StatusCreated {
		t.Fatalf("Create() status = %d, want %d; body=%s", response.Code, http.StatusCreated, response.Body.String())
	}
}

// TestNodePoolHandlerUpdateRejectsRootVolumeSizeChangesAndClears verifies that
// an update cannot change or clear the immutable rootVolume.size field.
func TestNodePoolHandlerUpdateRejectsRootVolumeSizeChangesAndClears(t *testing.T) {
	tests := []struct {
		name string
		size string
	}{
		{name: "change", size: "800"},
		{name: "clear with zero", size: "0"},
		{name: "clear with null", size: "null"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			const (
				accountID = "123456789012"
				poolName  = "pool-a"
			)
			object := &seaplanev1alpha1.NodePool{
				ObjectMeta: metav1.ObjectMeta{
					Name:      poolName,
					Namespace: "account-" + accountID,
					Labels:    map[string]string{seaplanev1alpha1.AccountIDLabel: accountID},
				},
				Spec: seaplanev1alpha1.NodePoolSpec{
					NodePool: seaplanev1alpha1.NodePoolSpecPassthrough{
						Platform: hypershiftv1beta1.NodePoolPlatform{
							AWS: &hypershiftv1beta1.AWSNodePoolPlatform{
								RootVolume: &hypershiftv1beta1.Volume{Size: 500},
							},
						},
					},
				},
			}
			handler := newNodePoolValidationTestHandler(t, object)
			body := `{"spec":{"nodePool":{"platform":{"aws":{"rootVolume":{"size":` + tt.size + `}}}}}}`
			request := httptest.NewRequest(http.MethodPut, "/api/v0/nodepools/"+poolName, strings.NewReader(body))
			request = request.WithContext(testContext(accountID))
			request = mux.SetURLVars(request, map[string]string{"id": poolName})
			response := httptest.NewRecorder()

			handler.Update(response, request)

			if response.Code != http.StatusUnprocessableEntity {
				t.Fatalf("Update() status = %d, want %d; body=%s", response.Code, http.StatusUnprocessableEntity, response.Body.String())
			}
			if !strings.Contains(response.Body.String(), "spec.nodePool.platform.aws.rootVolume.size") {
				t.Fatalf("Update() response does not identify rootVolume.size: %s", response.Body.String())
			}
		})
	}
}

// TestNodePoolHandlerUpdateRejectsExplicitNullRootVolume verifies that an update
// explicitly setting rootVolume to null is rejected the same as changing its size
func TestNodePoolHandlerUpdateRejectsExplicitNullRootVolume(t *testing.T) {
	const (
		accountID = "123456789012"
		poolName  = "pool-a"
	)
	object := &seaplanev1alpha1.NodePool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      poolName,
			Namespace: "account-" + accountID,
			Labels:    map[string]string{seaplanev1alpha1.AccountIDLabel: accountID},
		},
		Spec: seaplanev1alpha1.NodePoolSpec{
			NodePool: seaplanev1alpha1.NodePoolSpecPassthrough{
				Platform: hypershiftv1beta1.NodePoolPlatform{
					AWS: &hypershiftv1beta1.AWSNodePoolPlatform{
						RootVolume: &hypershiftv1beta1.Volume{Size: 500},
					},
				},
			},
		},
	}
	handler := newNodePoolValidationTestHandler(t, object)
	body := `{"spec":{"nodePool":{"platform":{"aws":{"rootVolume":null}}}}}`
	request := httptest.NewRequest(http.MethodPut, "/api/v0/nodepools/"+poolName, strings.NewReader(body))
	request = request.WithContext(testContext(accountID))
	request = mux.SetURLVars(request, map[string]string{"id": poolName})
	response := httptest.NewRecorder()

	handler.Update(response, request)

	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("Update() status = %d, want %d; body=%s", response.Code, http.StatusUnprocessableEntity, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "spec.nodePool.platform.aws.rootVolume.size") {
		t.Fatalf("Update() response does not identify rootVolume.size: %s", response.Body.String())
	}

	// Confirm the stored RootVolume was NOT wiped out by a rejected request.
	stored, err := handler.db.GetNodePool(request.Context(), poolName)
	if err != nil {
		t.Fatalf("GetNodePool: %v", err)
	}
	if stored.Spec.NodePool.Platform.AWS == nil || stored.Spec.NodePool.Platform.AWS.RootVolume == nil {
		t.Fatal("RootVolume was wiped out despite the rejected update")
	}
	if stored.Spec.NodePool.Platform.AWS.RootVolume.Size != 500 {
		t.Errorf("RootVolume.Size = %d, want 500 (unchanged)", stored.Spec.NodePool.Platform.AWS.RootVolume.Size)
	}
}

// TestNodePoolHandlerUpdateAllowsOmittedRootVolume verifies that omitting rootVolume
// (vs. sending explicit null) is allowed and leaves the existing RootVolume untouched.
func TestNodePoolHandlerUpdateAllowsOmittedRootVolume(t *testing.T) {
	const (
		accountID = "123456789012"
		poolName  = "pool-a"
	)
	object := &seaplanev1alpha1.NodePool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      poolName,
			Namespace: "account-" + accountID,
			Labels:    map[string]string{seaplanev1alpha1.AccountIDLabel: accountID},
		},
		Spec: seaplanev1alpha1.NodePoolSpec{
			NodePool: seaplanev1alpha1.NodePoolSpecPassthrough{
				Platform: hypershiftv1beta1.NodePoolPlatform{
					AWS: &hypershiftv1beta1.AWSNodePoolPlatform{
						RootVolume: &hypershiftv1beta1.Volume{Size: 500},
					},
				},
			},
		},
	}
	parent := &seaplanev1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "my-cluster", Namespace: object.Namespace, Labels: object.Labels}}
	handler := newNodePoolValidationTestHandler(t, object, parent)
	body := `{"spec":{"nodePool":{"platform":{"aws":{"instanceType":"m5.xlarge"}}}}}`
	request := httptest.NewRequest(http.MethodPut, "/api/v0/nodepools/"+poolName, strings.NewReader(body))
	request = request.WithContext(testContext(accountID))
	request = mux.SetURLVars(request, map[string]string{"id": poolName})
	response := httptest.NewRecorder()

	handler.Update(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("Update() status = %d, want %d; body=%s", response.Code, http.StatusOK, response.Body.String())
	}

	stored, err := handler.db.GetNodePool(request.Context(), poolName)
	if err != nil {
		t.Fatalf("GetNodePool: %v", err)
	}
	if stored.Spec.NodePool.Platform.AWS == nil || stored.Spec.NodePool.Platform.AWS.RootVolume == nil {
		t.Fatal("RootVolume was unexpectedly cleared by an update that omitted it")
	}
	if stored.Spec.NodePool.Platform.AWS.RootVolume.Size != 500 {
		t.Errorf("RootVolume.Size = %d, want 500 (preserved)", stored.Spec.NodePool.Platform.AWS.RootVolume.Size)
	}
}

// TestNodePoolHandlerUpdateRejectsExplicitNullAtParentLevels verifies null at aws,
// platform, or nodePool is rejected the same as null at rootVolume itself.
func TestNodePoolHandlerUpdateRejectsExplicitNullAtParentLevels(t *testing.T) {
	const (
		accountID = "123456789012"
	)

	tests := []struct {
		name string
		body string
	}{
		{"aws null", `{"spec":{"nodePool":{"platform":{"aws":null}}}}`},
		{"platform null", `{"spec":{"nodePool":{"platform":null}}}`},
		{"nodePool null", `{"spec":{"nodePool":null}}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			poolName := "pool-" + strings.ReplaceAll(tt.name, " ", "-")
			object := &seaplanev1alpha1.NodePool{
				ObjectMeta: metav1.ObjectMeta{
					Name:      poolName,
					Namespace: "account-" + accountID,
					Labels:    map[string]string{seaplanev1alpha1.AccountIDLabel: accountID},
				},
				Spec: seaplanev1alpha1.NodePoolSpec{
					NodePool: seaplanev1alpha1.NodePoolSpecPassthrough{
						Platform: hypershiftv1beta1.NodePoolPlatform{
							AWS: &hypershiftv1beta1.AWSNodePoolPlatform{
								RootVolume: &hypershiftv1beta1.Volume{Size: 500},
							},
						},
					},
				},
			}
			handler := newNodePoolValidationTestHandler(t, object)
			request := httptest.NewRequest(http.MethodPut, "/api/v0/nodepools/"+poolName, strings.NewReader(tt.body))
			request = request.WithContext(testContext(accountID))
			request = mux.SetURLVars(request, map[string]string{"id": poolName})
			response := httptest.NewRecorder()

			handler.Update(response, request)

			if response.Code != http.StatusUnprocessableEntity {
				t.Fatalf("Update() status = %d, want %d; body=%s", response.Code, http.StatusUnprocessableEntity, response.Body.String())
			}
			if !strings.Contains(response.Body.String(), "spec.nodePool.platform.aws.rootVolume.size") {
				t.Fatalf("Update() response does not identify rootVolume.size: %s", response.Body.String())
			}

			stored, err := handler.db.GetNodePool(request.Context(), poolName)
			if err != nil {
				t.Fatalf("GetNodePool: %v", err)
			}
			if stored.Spec.NodePool.Platform.AWS == nil || stored.Spec.NodePool.Platform.AWS.RootVolume == nil {
				t.Fatal("RootVolume was wiped out despite the rejected update")
			}
			if stored.Spec.NodePool.Platform.AWS.RootVolume.Size != 500 {
				t.Errorf("RootVolume.Size = %d, want 500 (unchanged)", stored.Spec.NodePool.Platform.AWS.RootVolume.Size)
			}
		})
	}
}

const nodePoolTestCaller = "arn:aws:iam::123456789012:user/test"

func newNodePoolValidationTestHandler(t *testing.T, objects ...client.Object) *NodePoolHandler {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := seaplanev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("register NodePool API types: %v", err)
	}
	// Keep parent-ownership checks from masking the validation under test.
	for _, object := range objects {
		switch resource := object.(type) {
		case *seaplanev1alpha1.Cluster:
			if resource.UID == "" {
				resource.UID = "validation-parent"
			}
		case *seaplanev1alpha1.NodePool:
			resource.UID = "validation-pool"
			parent := testClusterCR("validation-parent", resource.Name, resource.Labels[seaplanev1alpha1.AccountIDLabel])
			resource.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(parent, seaplanev1alpha1.GroupVersion.WithKind("Cluster"))}
			resource.Labels[clusterUIDLabel] = string(parent.UID)
			resource.Spec.NodePool.ClusterName = parent.Name
			objects = append(objects, parent)
		}
	}
	dbClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	path := filepath.Join(t.TempDir(), "authz.json")
	if err := os.WriteFile(path, []byte(`{"formatVersion":1,"registeredAccounts":["123456789012"],"policies":[{"id":"all","ownerAccountID":"123456789012","content":"permit(principal, action in Seaplane::Action::\"AllActions\", resource);"}],"attachments":[{"id":"all","policyID":"all","principalARN":"`+nodePoolTestCaller+`","scope":"global"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	authorizer, err := authz.LoadConfig(path, "us-east-1")
	if err != nil {
		t.Fatal(err)
	}
	return NewNodePoolHandler(seaplanedb.NewClientFrom(dbClient, logger), authorizer, logger)
}

// TestNodePoolHandlerUpdateRejectsClearedImageType verifies that raw-aware
// immutable validation rejects explicit empty and null imageType values, which
// would otherwise convert a Windows pool back to the Linux default on merge.
func TestNodePoolHandlerUpdateRejectsClearedImageType(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{name: "empty string", value: `""`},
		{name: "null", value: "null"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			const (
				accountID = "123456789012"
				poolName  = "pool-a"
			)
			object := &seaplanev1alpha1.NodePool{
				ObjectMeta: metav1.ObjectMeta{
					Name:      poolName,
					Namespace: "account-" + accountID,
					Labels:    map[string]string{seaplanev1alpha1.AccountIDLabel: accountID},
				},
				Spec: seaplanev1alpha1.NodePoolSpec{
					NodePool: seaplanev1alpha1.NodePoolSpecPassthrough{
						Platform: hypershiftv1beta1.NodePoolPlatform{
							AWS: &hypershiftv1beta1.AWSNodePoolPlatform{
								ImageType: hypershiftv1beta1.ImageTypeWindows,
							},
						},
					},
				},
			}
			handler := newNodePoolValidationTestHandler(t, object)
			body := `{"spec":{"nodePool":{"platform":{"aws":{"imageType":` + tt.value + `}}}}}`
			request := httptest.NewRequest(http.MethodPut, "/api/v0/nodepools/"+poolName, strings.NewReader(body))
			request = request.WithContext(testContext(accountID))
			request = mux.SetURLVars(request, map[string]string{"id": poolName})
			response := httptest.NewRecorder()

			handler.Update(response, request)

			if response.Code != http.StatusUnprocessableEntity {
				t.Fatalf("Update() status = %d, want %d; body=%s", response.Code, http.StatusUnprocessableEntity, response.Body.String())
			}
			if !strings.Contains(response.Body.String(), "spec.nodePool.platform.aws.imageType") {
				t.Fatalf("Update() response does not identify imageType: %s", response.Body.String())
			}

			stored, err := handler.db.GetNodePool(request.Context(), poolName)
			if err != nil {
				t.Fatalf("GetNodePool: %v", err)
			}
			if stored.Spec.NodePool.Platform.AWS.ImageType != hypershiftv1beta1.ImageTypeWindows {
				t.Errorf("ImageType = %q, want %q (unchanged)", stored.Spec.NodePool.Platform.AWS.ImageType, hypershiftv1beta1.ImageTypeWindows)
			}
		})
	}
}

// TestNodePoolHandlerUpdateAllowsOmittedImageType verifies that an ordinary
// update to a Windows pool still works: omitting imageType means "leave it
// alone", not "convert this pool back to Linux".
func TestNodePoolHandlerUpdateAllowsOmittedImageType(t *testing.T) {
	const (
		accountID = "123456789012"
		poolName  = "pool-a"
	)
	object := &seaplanev1alpha1.NodePool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      poolName,
			Namespace: "account-" + accountID,
			Labels:    map[string]string{seaplanev1alpha1.AccountIDLabel: accountID},
		},
		Spec: seaplanev1alpha1.NodePoolSpec{
			NodePool: seaplanev1alpha1.NodePoolSpecPassthrough{
				Platform: hypershiftv1beta1.NodePoolPlatform{
					AWS: &hypershiftv1beta1.AWSNodePoolPlatform{
						ImageType: hypershiftv1beta1.ImageTypeWindows,
					},
				},
			},
		},
	}
	handler := newNodePoolValidationTestHandler(t, object)
	body := `{"spec":{"labels":{"team":"platform"}}}`
	request := httptest.NewRequest(http.MethodPut, "/api/v0/nodepools/"+poolName, strings.NewReader(body))
	request = request.WithContext(testContext(accountID))
	request = mux.SetURLVars(request, map[string]string{"id": poolName})
	response := httptest.NewRecorder()

	handler.Update(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("Update() status = %d, want %d; body=%s", response.Code, http.StatusOK, response.Body.String())
	}

	stored, err := handler.db.GetNodePool(request.Context(), poolName)
	if err != nil {
		t.Fatalf("GetNodePool: %v", err)
	}
	if stored.Spec.NodePool.Platform.AWS.ImageType != hypershiftv1beta1.ImageTypeWindows {
		t.Errorf("ImageType = %q, want %q (unchanged)", stored.Spec.NodePool.Platform.AWS.ImageType, hypershiftv1beta1.ImageTypeWindows)
	}
	if stored.Spec.Labels["team"] != "platform" {
		t.Errorf("Labels[team] = %q, want %q", stored.Spec.Labels["team"], "platform")
	}
}
