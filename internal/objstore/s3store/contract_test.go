//go:build s3conformance

package s3store

import (
	"os"
	"strings"
	"testing"

	"github.com/fetburner/rokuban/internal/objstore"
	"github.com/fetburner/rokuban/internal/objstore/contracttest"
)

func TestStoreContract(t *testing.T) {
	endpoint := strings.TrimRight(strings.TrimSpace(os.Getenv("ROKUBAN_S3_CONFORMANCE_ENDPOINT")), "/")
	bucket := strings.TrimSpace(os.Getenv("ROKUBAN_S3_CONFORMANCE_BUCKET"))
	if endpoint == "" || bucket == "" {
		t.Skip("ROKUBAN_S3_CONFORMANCE_ENDPOINT and ROKUBAN_S3_CONFORMANCE_BUCKET are required")
	}
	region := strings.TrimSpace(os.Getenv("ROKUBAN_S3_CONFORMANCE_REGION"))
	if region == "" {
		region = "us-east-1"
	}

	contracttest.Run(t, func(t *testing.T) objstore.Store {
		objects, err := New(t.Context(), Config{Endpoint: endpoint, Region: region, Bucket: bucket})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		return objects
	})
}
