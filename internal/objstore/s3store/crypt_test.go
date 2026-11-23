package s3store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/fetburner/rokuban/internal/objstore"
	"github.com/fetburner/rokuban/internal/objstore/contracttest"
	"github.com/fetburner/rokuban/internal/objstore/cryptstore"
)

const testPassword, testSalt = "contract private password", "contract private salt"

// rcloneOutput keeps stderr for diagnosis and omits the command line, which can
// contain the plaintext arguments to rclone obscure.
func rcloneOutput(cmd *exec.Cmd, operation string) (string, error) {
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		diagnostic := fmt.Sprintf("rclone %s failed: %v\n%s", operation, err, strings.TrimSpace(stderr.String()))
		redact := strings.NewReplacer(testPassword, "[redacted]", testSalt, "[redacted]")
		return "", errors.New(redact.Replace(diagnostic))
	}
	return strings.TrimSpace(string(out)), nil
}

func TestRcloneFailureReportsStderrWithoutKeys(t *testing.T) {
	cmd := exec.CommandContext(t.Context(), "sh", "-c", `printf 'directory not found\npassword=%s\nsalt=%s\n' "$1" "$2" >&2; exit 9`, "--", testPassword, testSalt)
	_, err := rcloneOutput(cmd, "copyto")
	if err == nil || !strings.Contains(err.Error(), "directory not found") || !strings.Contains(err.Error(), "exit status 9") {
		t.Fatalf("failure does not report exit status and stderr: %v", err)
	}
	if strings.Contains(err.Error(), testPassword) || strings.Contains(err.Error(), testSalt) {
		t.Fatal("rclone failure leaked plaintext key material")
	}
}

type memoryObject struct {
	body        []byte
	metadata    map[string]string
	contentType string
}

type memoryUpload struct {
	key    string
	object memoryObject
	parts  map[int32][]byte
}

type memoryS3 struct {
	mu        sync.Mutex
	objects   map[string]memoryObject
	uploads   map[string]memoryUpload
	nextID    int
	partCalls int
	failPart  int
	headCalls int
	getRanges []string
}

func newMemoryS3() *memoryS3 {
	return &memoryS3{objects: make(map[string]memoryObject), uploads: make(map[string]memoryUpload)}
}

func (m *memoryS3) PutObject(_ context.Context, in *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := aws.ToString(in.Key)
	if _, exists := m.objects[key]; exists && aws.ToString(in.IfNoneMatch) == "*" {
		return nil, &smithy.GenericAPIError{Code: "PreconditionFailed"}
	}
	body, err := io.ReadAll(in.Body)
	if err != nil {
		return nil, err
	}
	m.objects[key] = memoryObject{body: body, metadata: in.Metadata, contentType: aws.ToString(in.ContentType)}
	return &s3.PutObjectOutput{}, nil
}

func (m *memoryS3) HeadObject(_ context.Context, in *s3.HeadObjectInput, _ ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.headCalls++
	object, ok := m.objects[aws.ToString(in.Key)]
	if !ok {
		return nil, &types.NoSuchKey{}
	}
	return &s3.HeadObjectOutput{ContentLength: aws.Int64(int64(len(object.body))), Metadata: object.metadata}, nil
}

func (m *memoryS3) GetObject(_ context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.getRanges = append(m.getRanges, aws.ToString(in.Range))
	object, ok := m.objects[aws.ToString(in.Key)]
	if !ok {
		return nil, &types.NoSuchKey{}
	}
	var offset int
	end := len(object.body)
	var contentRange *string
	if in.Range != nil {
		if _, err := fmt.Sscanf(*in.Range, "bytes=%d-", &offset); err != nil {
			return nil, err
		}
		if offset >= len(object.body) {
			return nil, &smithy.GenericAPIError{Code: "InvalidRange"}
		}
		if _, last, _ := strings.Cut(*in.Range, "-"); last != "" {
			to, err := strconv.Atoi(last)
			if err != nil {
				return nil, err
			}
			end = min(end, to+1)
		}
		contentRange = aws.String(fmt.Sprintf("bytes %d-%d/%d", offset, end-1, len(object.body)))
	}
	return &s3.GetObjectOutput{Body: io.NopCloser(bytes.NewReader(object.body[offset:end])), ContentRange: contentRange}, nil
}

func (m *memoryS3) DeleteObject(_ context.Context, in *s3.DeleteObjectInput, _ ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objects, aws.ToString(in.Key))
	return &s3.DeleteObjectOutput{}, nil
}

func (m *memoryS3) ListObjectsV2(_ context.Context, in *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := &s3.ListObjectsV2Output{}
	for key, object := range m.objects {
		if strings.HasPrefix(key, aws.ToString(in.Prefix)) {
			out.Contents = append(out.Contents, types.Object{Key: aws.String(key), Size: aws.Int64(int64(len(object.body)))})
		}
	}
	return out, nil
}

func (m *memoryS3) CreateMultipartUpload(_ context.Context, in *s3.CreateMultipartUploadInput, _ ...func(*s3.Options)) (*s3.CreateMultipartUploadOutput, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextID++
	id := strconv.Itoa(m.nextID)
	m.uploads[id] = memoryUpload{
		key: aws.ToString(in.Key), object: memoryObject{metadata: in.Metadata, contentType: aws.ToString(in.ContentType)},
		parts: make(map[int32][]byte),
	}
	return &s3.CreateMultipartUploadOutput{UploadId: aws.String(id)}, nil
}

func (m *memoryS3) UploadPart(_ context.Context, in *s3.UploadPartInput, _ ...func(*s3.Options)) (*s3.UploadPartOutput, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.partCalls++
	if m.partCalls == m.failPart {
		return nil, errors.New("injected failure after reading a part")
	}
	upload, ok := m.uploads[aws.ToString(in.UploadId)]
	if !ok {
		return nil, &types.NoSuchUpload{}
	}
	body, err := io.ReadAll(in.Body)
	if err != nil {
		return nil, err
	}
	number := aws.ToInt32(in.PartNumber)
	upload.parts[number] = body
	return &s3.UploadPartOutput{ETag: aws.String(strconv.Itoa(int(number)))}, nil
}

func (m *memoryS3) CompleteMultipartUpload(_ context.Context, in *s3.CompleteMultipartUploadInput, _ ...func(*s3.Options)) (*s3.CompleteMultipartUploadOutput, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := aws.ToString(in.UploadId)
	upload, ok := m.uploads[id]
	if !ok {
		return nil, &types.NoSuchUpload{}
	}
	if _, exists := m.objects[upload.key]; exists && aws.ToString(in.IfNoneMatch) == "*" {
		return nil, &smithy.GenericAPIError{Code: "PreconditionFailed"}
	}
	for _, part := range in.MultipartUpload.Parts {
		partBytes, ok := upload.parts[aws.ToInt32(part.PartNumber)]
		if !ok {
			return nil, &smithy.GenericAPIError{Code: "InvalidPart"}
		}
		upload.object.body = append(upload.object.body, partBytes...)
	}
	m.objects[upload.key] = upload.object
	delete(m.uploads, id)
	return &s3.CompleteMultipartUploadOutput{}, nil
}

func (m *memoryS3) AbortMultipartUpload(_ context.Context, in *s3.AbortMultipartUploadInput, _ ...func(*s3.Options)) (*s3.AbortMultipartUploadOutput, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.uploads, aws.ToString(in.UploadId))
	return &s3.AbortMultipartUploadOutput{}, nil
}

func (m *memoryS3) ListParts(_ context.Context, in *s3.ListPartsInput, _ ...func(*s3.Options)) (*s3.ListPartsOutput, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.uploads[aws.ToString(in.UploadId)]; !ok {
		return nil, &types.NoSuchUpload{}
	}
	return &s3.ListPartsOutput{}, nil
}

func memoryCryptStore(t *testing.T, api *memoryS3, password, salt string, readOnly bool) *cryptstore.Store {
	t.Helper()
	inner := &declaredStore{&Store{client: api, bucket: "test", declaredSHA256: true, readOnly: readOnly}}
	store, err := cryptstore.New(inner, "test", password, salt, readOnly)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func TestCryptStoreContract(t *testing.T) {
	contracttest.Run(t, func(t *testing.T) objstore.Store {
		return memoryCryptStore(t, newMemoryS3(), testPassword, testSalt, false)
	})
}

func TestMarkerAndForeignObjects(t *testing.T) {
	ctx := t.Context()
	api := newMemoryS3()
	writer := memoryCryptStore(t, api, testPassword, testSalt, false)
	reader := memoryCryptStore(t, api, testPassword, testSalt, true)
	if _, err := reader.Head(ctx, "media.mp4"); err == nil || errors.Is(err, objstore.ErrNotFound) || len(api.objects) != 0 {
		t.Fatalf("read-only store without a marker = %v, objects = %d", err, len(api.objects))
	}

	key := "番組/特別編.mp4"
	payload := []byte("plain media body")
	digest := sha256.Sum256(payload)
	if err := writer.Put(ctx, key, bytes.NewReader(payload), int64(len(payload)), digest, "video/mp4"); err != nil {
		t.Fatal(err)
	}
	var hiddenKey string
	for raw, object := range api.objects {
		if raw == ".rokuban-crypt" {
			continue
		}
		hiddenKey = raw
		if strings.Contains(raw, "番組") || strings.Contains(raw, "特別編") || bytes.Contains(object.body, payload) {
			t.Fatal("raw store contains a clear name or body")
		}
		if !bytes.HasPrefix(object.body, []byte("RCLONE\x00\x00")) || len(object.body) != 64 ||
			object.contentType != "application/octet-stream" || object.metadata["rokuban-sha256"] != hex.EncodeToString(digest[:]) {
			t.Fatal("raw object has unexpected format or metadata")
		}
	}
	if hiddenKey == "" || len(api.objects) != 2 {
		t.Fatalf("raw store has %d objects, want marker and one encrypted object", len(api.objects))
	}
	if _, err := reader.Head(ctx, key); err != nil {
		t.Fatal(err)
	}
	if err := reader.Delete(ctx, key); !errors.Is(err, objstore.ErrReadOnly) {
		t.Fatalf("read-only Delete = %v", err)
	}
	api.objects["out-of-band.txt"] = memoryObject{body: []byte("unexpected")}
	listed, err := writer.List(ctx, "")
	if err != nil || len(listed) != 2 {
		t.Fatalf("List = %+v, %v", listed, err)
	}
	for _, object := range listed {
		switch object.Key {
		case key:
			if object.Size != 16 {
				t.Fatalf("clear listed size = %d", object.Size)
			}
		case "~crypt-foreign/b3V0LW9mLWJhbmQudHh0":
			if _, err := writer.Head(ctx, object.Key); err != nil {
				t.Fatal(err)
			}
			delete(api.objects, "out-of-band.txt")
			if _, err := writer.Head(ctx, object.Key); !errors.Is(err, objstore.ErrNotFound) {
				t.Fatalf("Head of removed foreign object = %v", err)
			}
		default:
			t.Fatalf("unexpected listed key %q", object.Key)
		}
	}
	listed, err = writer.List(ctx, "番組/特")
	if err != nil || len(listed) != 1 || listed[0].Key != key {
		t.Fatalf("partial prefix List = %+v, %v", listed, err)
	}
	wrongPassword := "deliberately wrong private password"
	wrong := memoryCryptStore(t, api, wrongPassword, testSalt, false)
	_, headErr := wrong.Head(ctx, key)
	if headErr == nil || errors.Is(headErr, objstore.ErrNotFound) || strings.Contains(headErr.Error(), wrongPassword) || strings.Contains(headErr.Error(), testSalt) {
		t.Fatalf("wrong-key Head should fail without exposing credentials: %v", headErr)
	}
	if err := wrong.Delete(ctx, key); !errors.Is(err, headErr) {
		t.Fatalf("wrong-key Delete = %v, want the latched marker error", err)
	}
	if err := wrong.Put(ctx, "new.mp4", bytes.NewReader(payload), 16, digest, "video/mp4"); !errors.Is(err, headErr) {
		t.Fatalf("wrong-key Put = %v, want the latched marker error", err)
	}
	wrongSalt := memoryCryptStore(t, api, testPassword, "different private salt", false)
	if _, err := wrongSalt.Head(ctx, key); err == nil || errors.Is(err, objstore.ErrNotFound) {
		t.Fatal("wrong salt was accepted or looked like a missing object")
	}

	if len(api.objects) != 2 {
		t.Fatal("wrong key changed the raw store")
	}

	legacy := newMemoryS3()
	legacy.objects["existing.mp4"] = memoryObject{}
	crypt := memoryCryptStore(t, legacy, testPassword, testSalt, false)
	if _, err := crypt.List(ctx, ""); err == nil || errors.Is(err, objstore.ErrNotFound) || len(legacy.objects) != 1 {
		t.Fatalf("enabling crypt on nonempty bucket: %v", err)
	}
}

func TestCryptResumeRejectsChangedUncommittedBytes(t *testing.T) {
	ctx := t.Context()
	api := newMemoryS3()
	api.failPart = 2
	store := memoryCryptStore(t, api, testPassword, testSalt, false)
	payload := bytes.Repeat([]byte("0123456789abcdef"), (11<<20)/16)
	digest := sha256.Sum256(payload)
	key := "番組/転送.mp4"
	state, err := store.StartUpload(ctx, key, int64(len(payload)), digest, "video/mp4")
	if err != nil {
		t.Fatal(err)
	}
	state, offset, err := store.ContinueUpload(ctx, state, bytes.NewReader(payload))
	if err == nil || offset != 5_177_344 || api.partCalls != 2 {
		t.Fatalf("interrupted upload offset = %d, part calls = %d, err = %v", offset, api.partCalls, err)
	}

	// 1 パート目の末尾はチャンクの途中。2 パート目は読んだが S3 に残っていない。
	changed := bytes.Clone(payload[offset:])
	changed[1<<20] ^= 0xff
	restarted := memoryCryptStore(t, api, testPassword, testSalt, false)
	for _, source := range [][]byte{changed, changed[:100]} {
		next, got, err := restarted.ContinueUpload(ctx, state, bytes.NewReader(source))
		if !errors.Is(err, objstore.ErrUploadInvalid) || got != 5_177_344 || !bytes.Equal(next, state) || api.partCalls != 2 {
			t.Fatalf("changed-source replay offset = %d, part calls = %d, err = %v", got, api.partCalls, err)
		}
	}
	state, offset, err = restarted.ContinueUpload(ctx, state, bytes.NewReader(payload[offset:]))
	if err != nil || offset != 11_534_336 {
		t.Fatalf("unchanged-source replay offset = %d, err = %v", offset, err)
	}
	if err := restarted.CompleteUpload(ctx, state); err != nil {
		t.Fatal(err)
	}
	body, err := restarted.GetFrom(ctx, key, 5_243_003)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(body)
	if err := errors.Join(err, body.Close()); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload[5_243_003:]) {
		t.Fatal("range after resume does not match clear source")
	}
}

func TestCryptRangeCachesHeader(t *testing.T) {
	ctx := t.Context()
	api := newMemoryS3()
	store := memoryCryptStore(t, api, testPassword, testSalt, false)
	payload := bytes.Repeat([]byte("0123456789abcdef"), 8193)
	const key = "番組/シーク.mp4"
	if err := store.Put(ctx, key, bytes.NewReader(payload), int64(len(payload)), sha256.Sum256(payload), "video/mp4"); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		offset int64
		ranges []string
	}{
		{65_537, []string{"bytes=0-31", "bytes=65584-"}},
		{7, []string{"bytes=32-"}},
		{131_075, []string{"bytes=131136-"}},
	} {
		api.headCalls, api.getRanges = 0, nil
		body, err := store.GetFrom(ctx, key, tc.offset)
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(body)
		if err := errors.Join(err, body.Close()); err != nil || !bytes.Equal(got, payload[tc.offset:]) {
			t.Fatalf("read at %d: got %d bytes, err = %v", tc.offset, len(got), err)
		}
		if api.headCalls != 0 || !slices.Equal(api.getRanges, tc.ranges) {
			t.Fatalf("read at %d: HEAD = %d, GET ranges = %q, want 0 and %q", tc.offset, api.headCalls, api.getRanges, tc.ranges)
		}
	}

	body, err := store.GetFrom(ctx, key, 131_088)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(body)
	if err := errors.Join(err, body.Close()); err != nil || len(got) != 0 {
		t.Fatalf("read at EOF: got %d bytes, err = %v", len(got), err)
	}
	if _, err := store.GetFrom(ctx, key, 131_089); err == nil || !strings.Contains(err.Error(), "offset exceeds object size") {
		t.Fatalf("read past EOF: %v", err)
	}

	// 消すプロセスと読むプロセスが異なると、読む側にはヘッダが残る。
	writer := memoryCryptStore(t, api, testPassword, testSalt, false)
	if err := writer.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	for _, offset := range []int64{7, 131_088, 131_089} {
		if _, err := store.GetFrom(ctx, key, offset); !errors.Is(err, objstore.ErrNotFound) {
			t.Fatalf("read deleted object at %d: %v, want ErrNotFound", offset, err)
		}
	}
}

func TestDeclaredSHA256IsSeparateMode(t *testing.T) {
	ctx := t.Context()
	plain := &Store{client: newMemoryS3(), bucket: "plain"}
	if _, ok := any(plain).(objstore.DeclaredSHA256Store); ok {
		t.Fatal("verifying store advertises declared SHA-256 mode")
	}
	declared := &declaredStore{&Store{client: newMemoryS3(), bucket: "declared", declaredSHA256: true}}
	data := []byte("bytes received by S3")
	digest := sha256.Sum256([]byte("declared clear bytes"))
	if err := plain.Put(ctx, "sample", bytes.NewReader(data), 20, digest, "application/octet-stream"); !errors.Is(err, objstore.ErrChecksumMismatch) {
		t.Fatalf("verifying Put = %v", err)
	}
	if err := declared.Put(ctx, "sample", bytes.NewReader(data), 20, digest, "application/octet-stream"); err != nil {
		t.Fatalf("declared Put = %v", err)
	}
	meta, err := declared.Head(ctx, "sample")
	if err != nil || meta.Size != 20 || meta.SHA256 != hex.EncodeToString(digest[:]) {
		t.Fatalf("declared Head = %+v, %v", meta, err)
	}
}

func TestCryptLengthAndAuthentication(t *testing.T) {
	ctx := t.Context()
	api := newMemoryS3()
	store := memoryCryptStore(t, api, testPassword, testSalt, false)
	data := []byte("0123456789")
	digest := sha256.Sum256(data)
	for _, length := range []int64{9, 11} {
		if err := store.Put(ctx, "wrong-length", bytes.NewReader(data), length, digest, "video/mp4"); !errors.Is(err, objstore.ErrChecksumMismatch) {
			t.Fatalf("wrong length Put = %v", err)
		}
		if _, err := store.Head(ctx, "wrong-length"); !errors.Is(err, objstore.ErrNotFound) {
			t.Fatalf("rejected Put left visible object: %v", err)
		}
	}

	state, err := store.StartUpload(ctx, "empty", 0, sha256.Sum256(nil), "video/mp4")
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := store.CompleteUpload(ctx, state); err != nil {
			t.Fatal(err)
		}
	}
	empty, err := store.Head(ctx, "empty")
	if err != nil || empty.Size != 0 {
		t.Fatalf("resumable empty object = %+v, %v", empty, err)
	}

	if err := store.Put(ctx, "authenticated", bytes.NewReader(data), 10, digest, "video/mp4"); err != nil {
		t.Fatal(err)
	}
	for _, object := range api.objects {
		if len(object.body) == 58 {
			object.body[34] ^= 0x80
		}
	}
	body, err := store.Get(ctx, "authenticated")
	if err != nil {
		t.Fatal(err)
	}
	got, readErr := io.ReadAll(body)
	if err := body.Close(); err != nil {
		t.Fatal(err)
	}
	if readErr == nil || len(got) != 0 {
		t.Fatalf("tampered block returned %d clear bytes, err = %v", len(got), readErr)
	}
}

func TestCryptMarkerNeverFallsBackToUnconditionalCreation(t *testing.T) {
	api := &fakeAPI{putErrs: []error{&smithy.GenericAPIError{Code: "NotImplemented"}}}
	inner := &declaredStore{&Store{client: api, bucket: "test", declaredSHA256: true}}
	store, err := cryptstore.New(inner, "test", testPassword, testSalt, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.List(t.Context(), ""); err == nil || errors.Is(err, objstore.ErrNotFound) {
		t.Fatalf("initialization without conditional PutObject = %v", err)
	}
	if api.putCalls != 1 || aws.ToString(api.putInputs[0].IfNoneMatch) != "*" {
		t.Fatal("crypt marker was written without a condition")
	}
}
