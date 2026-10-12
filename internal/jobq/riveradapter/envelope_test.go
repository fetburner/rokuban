package riveradapter

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/fetburner/rokuban/internal/jobs"
)

func TestRiverEnvelopePreservesLegacyPayloadShape(t *testing.T) {
	args := jobs.EncodeJobArgs{RecordingID: 42, Profile: "h264"}

	want, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshaling job args: %v", err)
	}
	got, err := json.Marshal(riverEnvelope{args: args})
	if err != nil {
		t.Fatalf("marshaling River envelope: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("River payload = %s, want legacy job args payload %s", got, want)
	}

	var decoded riverArgs[jobs.EncodeJobArgs]
	if err := json.Unmarshal(want, &decoded); err != nil {
		t.Fatalf("decoding legacy payload through adapter args: %v", err)
	}
	if !reflect.DeepEqual(decoded.args, args) {
		t.Errorf("decoded args = %#v, want %#v", decoded.args, args)
	}
}
