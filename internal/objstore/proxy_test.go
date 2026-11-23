package objstore

import (
	"context"
	"io"
	"testing"
	"time"
)

type proxyTestStore struct{ Store }

func (proxyTestStore) GetFrom(context.Context, string, int64) (io.ReadCloser, error) {
	return io.NopCloser(nilReader{}), nil
}

type nilReader struct{}

func (nilReader) Read([]byte) (int, error) { return 0, io.EOF }

type signedProxyTestStore struct{ proxyTestStore }

func (signedProxyTestStore) Presign(context.Context, string, string, time.Duration, string, string) (string, error) {
	return "https://example.invalid", nil
}

type presignOnlyTestStore struct{ Store }

func (presignOnlyTestStore) Presign(context.Context, string, string, time.Duration, string, string) (string, error) {
	return "https://example.invalid", nil
}

func TestNewProxyDeliveryExposesRangeReaderAndHidesSignedURL(t *testing.T) {
	wrapped, err := NewProxyDelivery(proxyTestStore{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := wrapped.(RangeReader); !ok {
		t.Fatalf("wrapped store type %T does not expose RangeReader", wrapped)
	}
	if _, ok := wrapped.(SignedURL); ok {
		t.Fatalf("wrapped store type %T exposes SignedURL", wrapped)
	}
}

func TestNewProxyDeliveryHidesUnderlyingSignedURL(t *testing.T) {
	wrapped, err := NewProxyDelivery(signedProxyTestStore{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := wrapped.(SignedURL); ok {
		t.Fatalf("wrapped store type %T exposes SignedURL", wrapped)
	}
}

func TestNewProxyDeliveryRequiresRangeReader(t *testing.T) {
	if _, err := NewProxyDelivery(presignOnlyTestStore{}); err == nil {
		t.Fatal("NewProxyDelivery succeeded without RangeReader")
	}
}
