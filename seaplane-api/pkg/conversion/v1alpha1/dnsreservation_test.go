package v1alpha1

import (
	"reflect"
	"testing"

	internal "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1"
	public "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1/public"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/conversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestProjectDNSReservation(t *testing.T) {
	input := &internal.DNSReservation{
		TypeMeta: metav1.TypeMeta{APIVersion: "seaplane.io/v1alpha1", Kind: "DNSReservation"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-reservation",
			Namespace: "account-123456789012",
			Labels:    map[string]string{internal.AccountIDLabel: "123456789012"},
		},
		Status: internal.DNSReservationStatus{
			Phase:                     internal.DNSReservationPhaseReady,
			BaseDomain:                "f7a3.0.example.com",
			InternalHostedClusterName: "hc-550e8400e29b41d",
			ObservedGeneration:        3,
		},
	}

	got := ProjectDNSReservation(input)
	if got == nil {
		t.Fatal("ProjectDNSReservation returned nil for non-nil input")
	}
	if got.TypeMeta != input.TypeMeta {
		t.Errorf("TypeMeta = %+v, want %+v", got.TypeMeta, input.TypeMeta)
	}
	if !reflect.DeepEqual(got.ObjectMeta, input.ObjectMeta) {
		t.Errorf("ObjectMeta = %+v, want %+v", got.ObjectMeta, input.ObjectMeta)
	}
	if string(got.Status.Phase) != string(input.Status.Phase) {
		t.Errorf("Status.Phase = %q, want %q", got.Status.Phase, input.Status.Phase)
	}
	if got.Status.BaseDomain != input.Status.BaseDomain {
		t.Errorf("Status.BaseDomain = %q, want %q", got.Status.BaseDomain, input.Status.BaseDomain)
	}
	if got.Status.InternalHostedClusterName != input.Status.InternalHostedClusterName {
		t.Errorf("Status.InternalHostedClusterName = %q, want %q", got.Status.InternalHostedClusterName, input.Status.InternalHostedClusterName)
	}
	if got.Status.ObservedGeneration != input.Status.ObservedGeneration {
		t.Errorf("Status.ObservedGeneration = %d, want %d", got.Status.ObservedGeneration, input.Status.ObservedGeneration)
	}
	if len(got.Status.Conditions) != len(input.Status.Conditions) {
		t.Errorf("Status.Conditions has length %d, want %d", len(got.Status.Conditions), len(input.Status.Conditions))
	}
}

func TestProjectDNSReservationNil(t *testing.T) {
	if got := ProjectDNSReservation(nil); got != nil {
		t.Errorf("ProjectDNSReservation(nil) = %+v, want nil", got)
	}
}

func TestUnprojectDNSReservation(t *testing.T) {
	got := UnprojectDNSReservation(&public.DNSReservationSpec{}, &conversion.ServiceSetFields{AccountID: "123456789012"})
	if got == nil {
		t.Fatal("UnprojectDNSReservation returned nil for non-nil spec")
	}
	if want := (internal.DNSReservationSpec{}); !reflect.DeepEqual(*got, want) {
		t.Errorf("UnprojectDNSReservation() = %+v, want %+v", *got, want)
	}
}

func TestUnprojectDNSReservationNil(t *testing.T) {
	if got := UnprojectDNSReservation(nil, nil); got != nil {
		t.Errorf("UnprojectDNSReservation(nil, nil) = %+v, want nil", got)
	}
}
