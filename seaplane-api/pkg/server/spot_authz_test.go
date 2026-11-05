package server

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestRejectedCreateKeepsQueue(t *testing.T) {
	// Rejected requests must not reserve the customer's Spot termination queue.
	for _, tc := range []struct {
		name, labels string
		status       int
	}{
		{"denied", `{}`, http.StatusForbidden},
		{"invalid labels", `{"bad key":"value"}`, http.StatusUnprocessableEntity},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writes := 0
			hooks := interceptor.Funcs{Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
				writes++
				return fmt.Errorf("unexpected create")
			}}
			srv, _ := resourceServer(t, nil, hooks)
			body := `{"metadata":{"name":"new","annotations":` + tc.labels + `},"spec":{"hostedCluster":{"release":{"image":"` + resourceTestReleaseImage + `"},"platform":{"type":"AWS","aws":{"region":"us-east-1","terminationHandlerQueueURL":"https://sqs.us-east-1.amazonaws.com/123456789012/spot"}}}}}`
			w := resourceRequest(srv, "POST", "clusters", body)
			if w.Code != tc.status || writes != 0 {
				t.Fatalf("status=%d writes=%d, want status=%d writes=0: %s", w.Code, writes, tc.status, w.Body.String())
			}
		})
	}
}
