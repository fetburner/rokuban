package s3store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"

	"github.com/fetburner/rokuban/internal/objstore"
)

var errSimulatedStop = errors.New("simulated process stop")

// failAfterReader は limit バイトを読んだ後に errSimulatedStop を返す。プロセスが途中で落ちた模擬。
type failAfterReader struct {
	reader *bytes.Reader
	limit  int64
	read   int64
}

func (r *failAfterReader) Read(p []byte) (int, error) {
	if r.read >= r.limit {
		return 0, errSimulatedStop
	}
	if remaining := r.limit - r.read; int64(len(p)) > remaining {
		p = p[:remaining]
	}
	n, err := r.reader.Read(p)
	r.read += int64(n)
	return n, err
}

func testBytes(n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = byte((i*17 + i/253) % 251)
	}
	return out
}

func resumableStore(client *fakeAPI) *Store {
	return &Store{client: client, presigner: fakePresigner{}, bucket: "derived"}
}

// TestResumeAfterStopCompletesWithStatedParts は、パートを 2 つ送って止まり、state だけを引き継いだ
// 別の Store で残りを送って完了するとき、CompleteMultipartUpload に state のパート一覧がそのまま渡ることを確かめる。
// ListParts は api interface に無いので、呼ばないことは型で保証される。
func TestResumeAfterStopCompletesWithStatedParts(t *testing.T) {
	client := &fakeAPI{}
	data := testBytes(12 << 20)
	digest := sha256.Sum256(data)
	ctx := context.Background()

	first := resumableStore(client)
	state, err := first.StartUpload(ctx, "encoded/resume.mp4", int64(len(data)), digest, "video/mp4")
	if err != nil {
		t.Fatalf("StartUpload: %v", err)
	}
	state, offset, err := first.ContinueUpload(ctx, state, &failAfterReader{reader: bytes.NewReader(data), limit: 10 << 20})
	if !errors.Is(err, errSimulatedStop) {
		t.Fatalf("first ContinueUpload error = %v, want the simulated stop", err)
	}
	if offset != 10<<20 {
		t.Fatalf("first ContinueUpload offset = %d, want %d (two 5 MiB parts)", offset, 10<<20)
	}

	second := resumableStore(client)
	saved := bytes.Clone(state)
	state, offset, err = second.ContinueUpload(ctx, saved, bytes.NewReader(data[offset:]))
	if err != nil {
		t.Fatalf("second ContinueUpload: %v", err)
	}
	if offset != int64(len(data)) {
		t.Fatalf("second ContinueUpload offset = %d, want %d", offset, len(data))
	}
	if err := second.CompleteUpload(ctx, state); err != nil {
		t.Fatalf("CompleteUpload: %v", err)
	}

	var st uploadState
	if err := json.Unmarshal(state, &st); err != nil {
		t.Fatal(err)
	}
	if len(client.completeInputs) != 1 {
		t.Fatalf("CompleteMultipartUpload calls = %d, want 1", len(client.completeInputs))
	}
	got := client.completeInputs[0].MultipartUpload.Parts
	if len(got) != len(st.Parts) || len(got) != 3 {
		t.Fatalf("completed %d parts, state records %d, want 3", len(got), len(st.Parts))
	}
	for i, part := range got {
		want := st.Parts[i]
		if aws.ToInt32(part.PartNumber) != want.Number || aws.ToString(part.ETag) != want.ETag {
			t.Fatalf("completed part %d = (%d, %q), state records (%d, %q)",
				i, aws.ToInt32(part.PartNumber), aws.ToString(part.ETag), want.Number, want.ETag)
		}
	}
	var assembled []byte
	for number := int32(1); number <= 3; number++ {
		assembled = append(assembled, client.parts[number]...)
	}
	if !bytes.Equal(assembled, data) {
		t.Fatalf("uploaded parts assemble to %d bytes that differ from the source", len(assembled))
	}
	if client.putPartCalls != 3 {
		t.Fatalf("UploadPart successes = %d, want 3 (no part resent after resume)", client.putPartCalls)
	}
}

func TestCompleteInvalidPartIsUploadInvalidAndRestartSucceeds(t *testing.T) {
	ctx := context.Background()
	data := []byte("restart after invalid part")
	digest := sha256.Sum256(data)
	client := &fakeAPI{completeErr: &smithy.GenericAPIError{Code: "InvalidPart", Message: "bad etag", Fault: smithy.FaultClient}}
	store := resumableStore(client)

	state, err := store.StartUpload(ctx, "encoded/invalid.mp4", int64(len(data)), digest, "video/mp4")
	if err != nil {
		t.Fatalf("StartUpload: %v", err)
	}
	state, _, err = store.ContinueUpload(ctx, state, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("ContinueUpload: %v", err)
	}
	if err := store.CompleteUpload(ctx, state); !errors.Is(err, objstore.ErrUploadInvalid) {
		t.Fatalf("CompleteUpload with InvalidPart = %v, want ErrUploadInvalid", err)
	}

	// 呼び出し側は状態を捨てて最初から送る。同じ key でも新しい UploadId で完了できる。
	client.completeErr = nil
	state, err = store.StartUpload(ctx, "encoded/invalid.mp4", int64(len(data)), digest, "video/mp4")
	if err != nil {
		t.Fatalf("StartUpload after restart: %v", err)
	}
	state, _, err = store.ContinueUpload(ctx, state, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("ContinueUpload after restart: %v", err)
	}
	if err := store.CompleteUpload(ctx, state); err != nil {
		t.Fatalf("CompleteUpload after restart: %v", err)
	}
}

func TestCompleteWithWrongDigestSendsNothing(t *testing.T) {
	ctx := context.Background()
	data := []byte("untrusted multipart bytes")
	client := &fakeAPI{}
	store := resumableStore(client)
	state, err := store.StartUpload(ctx, "encoded/wrong.mp4", int64(len(data)), [32]byte{1}, "video/mp4")
	if err != nil {
		t.Fatalf("StartUpload: %v", err)
	}
	state, _, err = store.ContinueUpload(ctx, state, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("ContinueUpload: %v", err)
	}
	if err := store.CompleteUpload(ctx, state); !errors.Is(err, objstore.ErrChecksumMismatch) {
		t.Fatalf("CompleteUpload with wrong digest = %v, want ErrChecksumMismatch", err)
	}
	if client.completeCalls != 0 {
		t.Fatalf("CompleteMultipartUpload calls = %d, want 0", client.completeCalls)
	}
}

// abortTestState は AbortUpload に渡す、UploadId を持つ送信状態を作る。
func abortTestState(t *testing.T, store *Store) []byte {
	t.Helper()
	state, err := store.StartUpload(context.Background(), "encoded/abort.mp4", 10, sha256.Sum256(make([]byte, 10)), "video/mp4")
	if err != nil {
		t.Fatalf("StartUpload: %v", err)
	}
	return state
}

// TestAbortUploadIsGoneOnlyWhenListPartsSaysNoSuchUpload は、Abort が成功を返し続けても、
// Abort の後の ListParts が NoSuchUpload を返すまでは gone にならないことを確かめる（MinIO は Abort 済みの UploadId に成功を返す）。
func TestAbortUploadIsGoneOnlyWhenListPartsSaysNoSuchUpload(t *testing.T) {
	noSuchUpload := &smithy.GenericAPIError{Code: "NoSuchUpload", Message: "gone", Fault: smithy.FaultClient}
	client := &fakeAPI{listPartsErrs: []error{nil, nil, noSuchUpload}}
	store := resumableStore(client)
	state := abortTestState(t, store)

	for i, want := range []bool{false, false, true} {
		gone, err := store.AbortUpload(context.Background(), state)
		if err != nil || gone != want {
			t.Fatalf("AbortUpload #%d = (%v, %v), want (%v, nil)", i+1, gone, err, want)
		}
	}
	if client.abortCalls != 3 || client.listPartsCalls != 3 {
		t.Fatalf("Abort calls = %d, ListParts calls = %d, want 3 and 3", client.abortCalls, client.listPartsCalls)
	}
}

// TestAbortUploadWithRemainingPartsIsNotGone は、Abort が NoSuchUpload を返しても ListParts が成功すれば gone ではないことを確かめる。
func TestAbortUploadWithRemainingPartsIsNotGone(t *testing.T) {
	noSuchUpload := &smithy.GenericAPIError{Code: "NoSuchUpload", Message: "gone", Fault: smithy.FaultClient}
	client := &fakeAPI{abortErrs: []error{noSuchUpload}, listPartsErrs: []error{nil}}
	store := resumableStore(client)
	gone, err := store.AbortUpload(context.Background(), abortTestState(t, store))
	if err != nil || gone {
		t.Fatalf("AbortUpload = (%v, %v), want (false, nil) when ListParts still succeeds", gone, err)
	}
}

// TestAbortUploadReturnsListPartsErrorWithCredentialClass は、ListParts の失敗を gone と誤らず、資格情報の失敗として返すことを確かめる。
func TestAbortUploadReturnsListPartsErrorWithCredentialClass(t *testing.T) {
	expired := &smithy.GenericAPIError{Code: "ExpiredToken", Message: "expired", Fault: smithy.FaultClient}
	client := &fakeAPI{listPartsErrs: []error{expired}}
	store := resumableStore(client)
	gone, err := store.AbortUpload(context.Background(), abortTestState(t, store))
	if gone || !errors.Is(err, objstore.ErrCredentials) {
		t.Fatalf("AbortUpload = (%v, %v), want (false, ErrCredentials) when ListParts fails", gone, err)
	}
}

// TestAbortUploadSkipsListPartsWhenAbortFails は、Abort が NoSuchUpload 以外で失敗したら ListParts を呼ばずに返すことを確かめる。
func TestAbortUploadSkipsListPartsWhenAbortFails(t *testing.T) {
	denied := &smithy.GenericAPIError{Code: "AccessDenied", Message: "denied", Fault: smithy.FaultClient}
	client := &fakeAPI{abortErrs: []error{denied}}
	store := resumableStore(client)
	gone, err := store.AbortUpload(context.Background(), abortTestState(t, store))
	if gone || err == nil || client.listPartsCalls != 0 {
		t.Fatalf("AbortUpload = (%v, %v) with %d ListParts calls, want (false, error) and no ListParts", gone, err, client.listPartsCalls)
	}
}

// TestPartSizeIsFixedInStateAndNotRecomputed は、state の partSize をいったん 6 MiB に書き換えると、
// 再開で切られるパートが 5 MiB（multipartPartSize の値）ではなく 6 MiB になることで、
// 再開時に multipartPartSize を呼び直していないことを確かめる。
func TestPartSizeIsFixedInStateAndNotRecomputed(t *testing.T) {
	ctx := context.Background()
	data := testBytes(12 << 20)
	digest := sha256.Sum256(data)
	client := &fakeAPI{}
	store := resumableStore(client)

	state, err := store.StartUpload(ctx, "encoded/fixed.mp4", int64(len(data)), digest, "video/mp4")
	if err != nil {
		t.Fatalf("StartUpload: %v", err)
	}
	var st uploadState
	if err := json.Unmarshal(state, &st); err != nil {
		t.Fatal(err)
	}
	if st.PartSize != multipartPartSize(int64(len(data))) {
		t.Fatalf("state part size = %d, want multipartPartSize = %d", st.PartSize, multipartPartSize(int64(len(data))))
	}
	st.PartSize = 6 << 20
	state, err = json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}

	if state, _, err = store.ContinueUpload(ctx, state, bytes.NewReader(data)); err != nil {
		t.Fatalf("ContinueUpload: %v", err)
	}
	if len(client.parts) != 2 || len(client.parts[1]) != 6<<20 || len(client.parts[2]) != 6<<20 {
		t.Fatalf("uploaded part sizes = %d parts, want 2 parts of 6 MiB", len(client.parts))
	}
	if err := store.CompleteUpload(ctx, state); err != nil {
		t.Fatalf("CompleteUpload: %v", err)
	}
}

func TestContinueRejectsStateWhoseOffsetDoesNotMatchParts(t *testing.T) {
	ctx := context.Background()
	data := testBytes(12 << 20)
	store := resumableStore(&fakeAPI{})
	state, err := store.StartUpload(ctx, "encoded/mismatch.mp4", int64(len(data)), sha256.Sum256(data), "video/mp4")
	if err != nil {
		t.Fatalf("StartUpload: %v", err)
	}
	var st uploadState
	if err := json.Unmarshal(state, &st); err != nil {
		t.Fatal(err)
	}
	st.Offset = 5 << 20 // パートの記録が無いのに offset だけ進んでいる
	state, err = json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ContinueUpload(ctx, state, bytes.NewReader(data[5<<20:])); !errors.Is(err, objstore.ErrUploadInvalid) {
		t.Fatalf("ContinueUpload with mismatched parts = %v, want ErrUploadInvalid", err)
	}
}

func TestGetFromSendsRangeOnlyAfterZero(t *testing.T) {
	var ranges []string
	client := &fakeAPI{getFn: func(_ context.Context, in *s3.GetObjectInput) (*s3.GetObjectOutput, error) {
		ranges = append(ranges, aws.ToString(in.Range))
		return &s3.GetObjectOutput{Body: io.NopCloser(bytes.NewReader(nil))}, nil
	}}
	store := resumableStore(client)
	for _, offset := range []int64{0, 7} {
		body, err := store.GetFrom(context.Background(), "encoded/range.mp4", offset)
		if err != nil {
			t.Fatalf("GetFrom(%d): %v", offset, err)
		}
		_ = body.Close()
	}
	if len(ranges) != 2 || ranges[0] != "" || ranges[1] != "bytes=7-" {
		t.Fatalf("Range headers = %q, want no Range for offset 0 and \"bytes=7-\" for offset 7", ranges)
	}
}

// TestAbortUploadWithoutUploadIDIsGone は、0 バイトのオブジェクトには UploadId が無いので、Abort は何もせず gone になることを確かめる。
func TestAbortUploadWithoutUploadIDIsGone(t *testing.T) {
	client := &fakeAPI{}
	store := resumableStore(client)
	empty := sha256.Sum256(nil)
	state, err := store.StartUpload(context.Background(), "thumbnails/empty.jpg", 0, empty, "image/jpeg")
	if err != nil {
		t.Fatalf("StartUpload: %v", err)
	}
	gone, err := store.AbortUpload(context.Background(), state)
	if err != nil || !gone || client.abortCalls != 0 {
		t.Fatalf("AbortUpload = (%v, %v) with %d Abort calls, want (true, nil) and no call", gone, err, client.abortCalls)
	}
}

// TestContinueRejectsShortAndLongSource は、size より短い source と長い source を ErrChecksumMismatch で拒むことを確かめる。
func TestContinueRejectsShortAndLongSource(t *testing.T) {
	ctx := context.Background()
	data := []byte("exact length payload")
	store := resumableStore(&fakeAPI{})
	state, err := store.StartUpload(ctx, "encoded/length.mp4", int64(len(data)), sha256.Sum256(data), "video/mp4")
	if err != nil {
		t.Fatalf("StartUpload: %v", err)
	}
	if _, _, err := store.ContinueUpload(ctx, state, bytes.NewReader(data[:10])); !errors.Is(err, objstore.ErrChecksumMismatch) {
		t.Fatalf("ContinueUpload with a short source = %v, want ErrChecksumMismatch", err)
	}
	if _, _, err := store.ContinueUpload(ctx, state, bytes.NewReader(append(bytes.Clone(data), 'x'))); !errors.Is(err, objstore.ErrChecksumMismatch) {
		t.Fatalf("ContinueUpload with a long source = %v, want ErrChecksumMismatch", err)
	}
}
