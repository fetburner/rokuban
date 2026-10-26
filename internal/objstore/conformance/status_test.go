//go:build s3conformance

package conformance

import (
	"testing"
)

func TestConditionalRetryStatus(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus string
		wantErr    bool
	}{
		{name: "precondition failure proves support", err: testAPIError("PreconditionFailed"), wantStatus: statusSupported},
		{name: "ignored condition is unsupported", wantStatus: statusUnsupported},
		{name: "explicit unsupported response", err: testAPIError("NotImplemented"), wantStatus: statusUnsupported},
		{name: "unexpected response is an error", err: testAPIError("AccessDenied"), wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			gotStatus, err := conditionalRetryStatus(test.err)
			if (err != nil) != test.wantErr {
				t.Fatalf("conditionalRetryStatus() error = %v, want error %t", err, test.wantErr)
			}
			if gotStatus != test.wantStatus {
				t.Fatalf("conditionalRetryStatus() = %q, want %q", gotStatus, test.wantStatus)
			}
		})
	}
}

func TestConditionalWriteUnsupportedIsOptional(t *testing.T) {
	result := featureResult{Feature: "conditional-writes", Status: statusUnsupported}
	if err := requiredFeatureError(result, false); err != nil {
		t.Fatalf("unsupported conditional writes must not fail conformance: %v", err)
	}
}

func TestRequiredFeatureMustBeSupported(t *testing.T) {
	for _, status := range []string{statusUnsupported, statusPartial} {
		t.Run(status, func(t *testing.T) {
			err := requiredFeatureError(featureResult{Feature: "multipart", Status: status}, true)
			if err == nil {
				t.Fatalf("required feature with status %q passed", status)
			}
		})
	}
}

func TestCombineStatuses(t *testing.T) {
	tests := []struct {
		name     string
		statuses []string
		want     string
	}{
		{name: "all supported", statuses: []string{statusSupported, statusSupported}, want: statusSupported},
		{name: "all unsupported", statuses: []string{statusUnsupported, statusUnsupported}, want: statusUnsupported},
		{name: "mixed", statuses: []string{statusSupported, statusUnsupported}, want: statusPartial},
		{name: "unsupported and partial", statuses: []string{statusUnsupported, statusPartial}, want: statusPartial},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := combineStatuses(test.statuses...); got != test.want {
				t.Fatalf("combineStatuses() = %q, want %q", got, test.want)
			}
		})
	}
}

type testAPIError string

func (e testAPIError) Error() string {
	return string(e)
}

func (e testAPIError) ErrorCode() string {
	return string(e)
}
