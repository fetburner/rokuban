//go:build s3conformance

package conformance

import (
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

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

func TestParseExpectations(t *testing.T) {
	got, err := parseExpectations("conditional-writes=unsupported, presigned-get-range=supported")
	if err != nil || len(got) != 2 || got[featureConditionalWrites] != statusUnsupported || got[featurePresignedRange] != statusSupported {
		t.Fatalf("parseExpectations() = %v, %v", got, err)
	}
	if got, err := parseExpectations(""); err != nil || got != nil {
		t.Fatalf("empty = %v, %v; want no expectations", got, err)
	}
	for _, bad := range []string{"unknown=supported", "conditional-writes", "conditional-writes=maybe"} {
		if _, err := parseExpectations(bad); err == nil {
			t.Fatalf("parseExpectations(%q) accepted", bad)
		}
	}
}

func TestExpectationError(t *testing.T) {
	expect := map[string]string{featureHeadChecksum: statusSupported}
	if err := expectationError(featureResult{Feature: featureHeadChecksum, Status: statusSupported}, expect); err != nil {
		t.Fatalf("match failed: %v", err)
	}
	if err := expectationError(featureResult{Feature: featureHeadChecksum, Status: statusPartial}, expect); err == nil {
		t.Fatal("mismatch passed")
	}
	if err := expectationError(featureResult{Feature: featureHeadChecksum, Status: statusUnknown}, expect); err == nil {
		t.Fatal("unknown passed against an expectation")
	}
	// 期待値が無い機能と、期待値そのものが無い実行は、どんな記録でも失敗にしない。
	for _, status := range []string{statusUnsupported, statusPartial, statusUnknown} {
		if err := expectationError(featureResult{Feature: featureMultipart, Status: status}, expect); err != nil {
			t.Fatalf("feature without expectation failed: %v", err)
		}
		if err := expectationError(featureResult{Feature: featureHeadChecksum, Status: status}, nil); err != nil {
			t.Fatalf("run without expectations failed: %v", err)
		}
	}
}

func TestAbortedStatus(t *testing.T) {
	tests := []struct {
		name    string
		parts   *s3.ListPartsOutput
		err     error
		want    string
		wantErr bool
	}{
		{name: "NoSuchUpload", err: testAPIError("NoSuchUpload"), want: statusSupported},
		{name: "empty parts", parts: &s3.ListPartsOutput{}, want: statusSupported},
		{name: "parts remain", parts: &s3.ListPartsOutput{Parts: []types.Part{{}}}, want: statusUnsupported},
		{name: "other error", err: testAPIError("AccessDenied"), wantErr: true},
		{name: "connection error", err: errors.New("connection refused"), wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := abortedStatus(test.parts, test.err)
			if (err != nil) != test.wantErr || got != test.want {
				t.Fatalf("abortedStatus() = %q, %v; want %q, error %t", got, err, test.want, test.wantErr)
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
