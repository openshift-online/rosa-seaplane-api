package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"
	hypershiftv1beta1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	seaplanev1alpha1 "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/clients/seaplanedb"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/middleware"
)

const spotUnitTestAccountID = "123456789012"

func newSpotUnitTestHandler(t *testing.T, objects ...client.Object) *ClusterHandler {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("registering core API types: %v", err)
	}
	if err := seaplanev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("registering Seaplane API types: %v", err)
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewClusterHandler(seaplanedb.NewClientFrom(fakeClient, logger), "", 0, clusterAuthorizer(t, lifecyclePermit), logger)
}

func spotUnitTestCluster(clusterName string) *seaplanev1alpha1.Cluster {
	return &seaplanev1alpha1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      clusterName,
			UID:       types.UID(clusterName),
			Namespace: "account-" + spotUnitTestAccountID,
			Labels:    map[string]string{seaplanev1alpha1.AccountIDLabel: spotUnitTestAccountID},
		},
		Spec: seaplanev1alpha1.ClusterSpec{
			HostedCluster: seaplanev1alpha1.HostedClusterSpecPassthrough{
				Platform: hypershiftv1beta1.PlatformSpec{
					Type: hypershiftv1beta1.AWSPlatform,
					AWS:  &hypershiftv1beta1.AWSPlatformSpec{Region: "us-east-1"},
				},
			},
		},
	}
}

func spotUnitTestContext() context.Context {
	ctx := context.WithValue(context.Background(), middleware.ContextKeyAccountID, spotUnitTestAccountID)
	return context.WithValue(ctx, middleware.ContextKeyCallerARN, "arn:aws:iam::"+spotUnitTestAccountID+":user/test")
}

func TestSpotUnit_CreateRejectsDuplicateTerminationQueueURL(t *testing.T) {
	const queueURL = "https://sqs.us-east-1.amazonaws.com/123456789012/spot-termination"
	existing := spotUnitTestCluster("existing")
	existing.Spec.HostedCluster.Platform.AWS.TerminationHandlerQueueURL = queueURL
	handler := newSpotUnitTestHandler(t, existing)
	body, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"name": "new-cluster"},
		"spec": map[string]any{
			"hostedCluster": map[string]any{
				"release":    map[string]any{"image": ""},
				"networking": map[string]any{},
				"platform": map[string]any{
					"type": "AWS",
					"aws": map[string]any{
						"region":                     "us-east-1",
						"terminationHandlerQueueURL": queueURL,
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("marshaling create request: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v0/clusters", bytes.NewReader(body))
	request = request.WithContext(spotUnitTestContext())
	response := httptest.NewRecorder()
	handler.Create(response, request)

	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "unique across clusters") {
		t.Errorf("expected queue uniqueness validation error, got %q", response.Body.String())
	}
}

func TestSpotUnit_UpdateRejectsDuplicateTerminationQueueURL(t *testing.T) {
	const queueURL = "https://sqs.us-east-1.amazonaws.com/123456789012/spot-termination"
	target := spotUnitTestCluster("target")
	existing := spotUnitTestCluster("existing")
	existing.Spec.HostedCluster.Platform.AWS.TerminationHandlerQueueURL = queueURL
	handler := newSpotUnitTestHandler(t, target, existing)
	request := newSpotUnitUpdateRequest(t, map[string]any{
		"hostedCluster": map[string]any{
			"platform": map[string]any{
				"aws": map[string]any{"terminationHandlerQueueURL": queueURL},
			},
		},
	})
	response := httptest.NewRecorder()
	handler.Update(response, request)

	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "unique across clusters") {
		t.Errorf("expected queue uniqueness validation error, got %q", response.Body.String())
	}
}

func TestSpotUnit_UpdateValidatesQueueWhenRegionChanges(t *testing.T) {
	const queueURL = "https://sqs.us-east-1.amazonaws.com/123456789012/spot-termination"
	target := spotUnitTestCluster("target")
	target.Spec.HostedCluster.Platform.AWS.TerminationHandlerQueueURL = queueURL
	handler := newSpotUnitTestHandler(t, target)
	request := newSpotUnitUpdateRequest(t, map[string]any{
		"hostedCluster": map[string]any{
			"platform": map[string]any{"aws": map[string]any{"region": "us-west-2"}},
		},
	})
	response := httptest.NewRecorder()
	handler.Update(response, request)

	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "same AWS region") {
		t.Errorf("expected region validation error, got %q", response.Body.String())
	}
}

func TestSpotUnit_UpdateSkipsQueueLookupWhenUnchanged(t *testing.T) {
	const queueURL = "https://sqs.us-east-1.amazonaws.com/123456789012/spot-termination"
	target := spotUnitTestCluster("target")
	target.Spec.HostedCluster.Platform.AWS.TerminationHandlerQueueURL = queueURL

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("registering core API types: %v", err)
	}
	if err := seaplanev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("registering Seaplane API types: %v", err)
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(target).WithInterceptorFuncs(interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			listOptions := &client.ListOptions{}
			for _, opt := range opts {
				opt.ApplyToList(listOptions)
			}
			if listOptions.Namespace == "" {
				return errors.New("unexpected account-wide list when the queue is unchanged")
			}
			return c.List(ctx, list, opts...)
		},
	}).Build()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := NewClusterHandler(seaplanedb.NewClientFrom(fakeClient, logger), "", 0, clusterAuthorizer(t, lifecyclePermit), logger)
	request := newSpotUnitUpdateRequest(t, map[string]any{"displayName": "updated-display"})
	response := httptest.NewRecorder()
	handler.Update(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", response.Code, response.Body.String())
	}
}

func TestSpotUnit_CreateClaimsTerminationQueue(t *testing.T) {
	const queueURL = "https://sqs.us-east-1.amazonaws.com/123456789012/spot-termination"
	handler := newSpotUnitTestHandler(t)
	body, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"name": "spot-cluster"},
		"spec": map[string]any{
			"hostedCluster": map[string]any{
				"release":    map[string]any{"image": ""},
				"networking": map[string]any{},
				"platform": map[string]any{
					"type": "AWS",
					"aws": map[string]any{
						"region":                     "us-east-1",
						"terminationHandlerQueueURL": queueURL,
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("marshaling create request: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v0/clusters", bytes.NewReader(body))
	request = request.WithContext(spotUnitTestContext())
	response := httptest.NewRecorder()
	handler.Create(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", response.Code, response.Body.String())
	}
}

func TestSpotUnit_UpdateClaimsChangedTerminationQueue(t *testing.T) {
	target := spotUnitTestCluster("target")
	handler := newSpotUnitTestHandler(t, target)
	request := newSpotUnitUpdateRequest(t, map[string]any{
		"hostedCluster": map[string]any{
			"platform": map[string]any{
				"aws": map[string]any{
					"terminationHandlerQueueURL": "https://sqs.us-east-1.amazonaws.com/123456789012/spot-termination",
				},
			},
		},
	})
	response := httptest.NewRecorder()
	handler.Update(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", response.Code, response.Body.String())
	}
}

func TestSpotUnit_CreateMapsQueueClaimConflictToValidation(t *testing.T) {
	const queueURL = "https://sqs.us-east-1.amazonaws.com/123456789012/spot-termination"
	reservation := &seaplanev1alpha1.Index{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: seaplanev1alpha1.SpotTerminationQueueReservationsNamespace,
			Name:      seaplanev1alpha1.SpotTerminationQueueIndexName(spotUnitTestAccountID, queueURL),
			Labels:    map[string]string{seaplanev1alpha1.SpotTerminationQueueOwnerLabel: "cluster-other"},
			Annotations: map[string]string{
				seaplanev1alpha1.SpotTerminationQueueClaimedAtAnnotation: time.Now().UTC().Format(time.RFC3339Nano),
			},
		},
	}
	handler := newSpotUnitTestHandler(t, reservation)
	body, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"name": "spot-cluster"},
		"spec": map[string]any{
			"hostedCluster": map[string]any{
				"release":    map[string]any{"image": ""},
				"networking": map[string]any{},
				"platform": map[string]any{
					"type": "AWS",
					"aws":  map[string]any{"region": "us-east-1", "terminationHandlerQueueURL": queueURL},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("marshaling create request: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v0/clusters", bytes.NewReader(body))
	request = request.WithContext(spotUnitTestContext())
	response := httptest.NewRecorder()
	handler.Create(response, request)
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "unique across clusters") {
		t.Errorf("expected queue conflict validation error, got %q", response.Body.String())
	}
}

func newSpotUnitUpdateRequest(t *testing.T, spec map[string]any) *http.Request {
	t.Helper()
	body, err := json.Marshal(map[string]any{"spec": spec})
	if err != nil {
		t.Fatalf("marshaling update request: %v", err)
	}
	request := httptest.NewRequest(http.MethodPut, "/api/v0/clusters/target", bytes.NewReader(body))
	request = request.WithContext(spotUnitTestContext())
	return mux.SetURLVars(request, map[string]string{"id": "target"})
}
