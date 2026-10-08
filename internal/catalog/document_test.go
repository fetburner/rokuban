package catalog

import (
	"encoding/json"
	"testing"
)

func TestDocumentUnmarshalDistinguishesMissingTSScans(t *testing.T) {
	var legacy Document
	if err := json.Unmarshal([]byte(`{"version":1}`), &legacy); err != nil {
		t.Fatalf("unmarshal legacy document: %v", err)
	}
	if legacy.mediaAssetTSScansPresent {
		t.Fatal("legacy document without mediaAssetTsScans was marked as containing the field")
	}

	var current Document
	if err := json.Unmarshal([]byte(`{"version":1,"mediaAssetTsScans":[]}`), &current); err != nil {
		t.Fatalf("unmarshal current document: %v", err)
	}
	if !current.mediaAssetTSScansPresent {
		t.Fatal("current document with an empty mediaAssetTsScans array was marked as legacy")
	}
}
