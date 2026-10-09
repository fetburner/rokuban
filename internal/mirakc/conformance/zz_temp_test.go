//go:build conformance

package conformance

import "testing"

func TestZZTemporaryFailure(t *testing.T) { t.Fatal("temporary failure to verify shard exit code") }
