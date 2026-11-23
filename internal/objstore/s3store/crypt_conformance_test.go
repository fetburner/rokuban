//go:build s3conformance

package s3store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/fetburner/rokuban/internal/contentpath"
	"github.com/fetburner/rokuban/internal/objstore"
	"github.com/fetburner/rokuban/internal/objstore/contracttest"
	"github.com/fetburner/rokuban/internal/objstore/cryptstore"
)

// cryptBucket は鍵の初期化を検査できるよう、他の適合テストと独立した bucket を作る。
func cryptBucket(t *testing.T) (*s3.Client, objstore.DeclaredSHA256Store, string) {
	t.Helper()
	cfg := conformanceConfig(t)
	cfg.Bucket = fmt.Sprintf("rokuban-crypt-%d", time.Now().UnixNano())
	raw, err := NewDeclaredSHA256(t.Context(), cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	client := raw.(*declaredStore).client.(*s3.Client)
	if _, err := client.CreateBucket(t.Context(), &s3.CreateBucketInput{Bucket: aws.String(cfg.Bucket)}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		objects, err := raw.List(ctx, "")
		if err != nil {
			t.Errorf("listing crypt fixture objects for cleanup: %v", err)
		}
		for _, object := range objects {
			if err := raw.Delete(ctx, object.Key); err != nil {
				t.Errorf("deleting crypt fixture object: %v", err)
			}
		}
		if _, err := client.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String(cfg.Bucket)}); err != nil {
			t.Errorf("deleting crypt fixture bucket: %v", err)
		}
	})
	return client, raw, cfg.Bucket
}

func TestCryptStoreRustFSContract(t *testing.T) {
	_, raw, _ := cryptBucket(t)
	contracttest.Run(t, func(t *testing.T) objstore.Store {
		objects, err := cryptstore.New(raw, "conformance", testPassword, testSalt, false)
		if err != nil {
			t.Fatal(err)
		}
		return objects
	})
}

func TestCryptRcloneRoundTrip(t *testing.T) {
	client, raw, bucket := cryptBucket(t)
	ctx := t.Context()
	objects, err := cryptstore.New(raw, "conformance", testPassword, testSalt, false)
	if err != nil {
		t.Fatal(err)
	}

	// 257 チャンク以上で、初期値に関係なく nonce の先頭バイトからの桁上がりを通る。
	payload := bytes.Repeat([]byte("放送内容 0123456789\n"), 800_001)
	digest := sha256.Sum256(payload)
	start := time.Date(2026, 11, 24, 0, 0, 0, 0, time.UTC)
	key := rcloneContentPath(t, "特別編", start)
	if err := objects.Put(ctx, key, bytes.NewReader(payload), int64(len(payload)), digest, "video/mp2t"); err != nil {
		t.Fatal(err)
	}
	listed, err := raw.List(ctx, "")
	if err != nil || len(listed) != 2 {
		t.Fatalf("raw RustFS bucket contents = %+v, %v", listed, err)
	}
	var sealedKey string
	for _, object := range listed {
		if object.Key != ".rokuban-crypt" {
			sealedKey = object.Key
		}
	}
	if sealedKey == "" || strings.Contains(sealedKey, "特別編") || strings.Contains(sealedKey, ".m2ts") {
		t.Fatalf("unexpected raw object key %q", sealedKey)
	}
	body, err := raw.Get(ctx, sealedKey)
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := io.ReadAll(body)
	if err := errors.Join(err, body.Close()); err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(ciphertext, []byte("RCLONE\x00\x00")) || bytes.Contains(ciphertext, []byte("放送内容 0123456789")) {
		t.Fatal("raw RustFS body leaks clear content or lacks the crypt header")
	}
	header, err := client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &bucket, Key: &sealedKey})
	if err != nil {
		t.Fatal(err)
	}
	if aws.ToString(header.ContentType) != "application/octet-stream" || header.Metadata["rokuban-sha256"] != fmt.Sprintf("%x", digest) {
		t.Fatal("raw RustFS headers have wrong content type or declared hash")
	}

	dir := t.TempDir()
	files := filepath.Join(dir, "raw")
	sealedPath := filepath.Join(files, filepath.FromSlash(sealedKey))
	if err := os.MkdirAll(filepath.Dir(sealedPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sealedPath, ciphertext, 0o600); err != nil {
		t.Fatal(err)
	}
	remote := rcloneCryptEnv(t, dir)
	rclone(t, dir, remote, "copyto", "compat:"+key, "/fixture/from-rokuban")
	plain, err := os.ReadFile(filepath.Join(dir, "from-rokuban"))
	if err != nil || sha256.Sum256(plain) != digest {
		t.Fatalf("rclone could not recover the original payload: %v", err)
	}

	// rclone が作った暗号文を、rclone と同様に Rokuban の SHA-256 metadata 無しで置く。
	nextKey := rcloneContentPath(t, "再放送", start.Add(time.Hour))
	rclone(t, dir, remote, "copyto", "/fixture/from-rokuban", "compat:"+nextKey)
	if err := filepath.WalkDir(files, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() || path == sealedPath {
			return walkErr
		}
		newRawKey, err := filepath.Rel(files, path)
		if err != nil {
			return err
		}
		sealed, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		_, err = client.PutObject(ctx, &s3.PutObjectInput{
			Bucket: &bucket, Key: aws.String(filepath.ToSlash(newRawKey)), Body: bytes.NewReader(sealed),
			ContentLength: aws.Int64(int64(len(sealed))),
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}

	clear, err := objects.List(ctx, "")
	if err != nil || len(clear) != 2 {
		t.Fatalf("clear RustFS listing = %+v, %v", clear, err)
	}
	for _, object := range clear {
		if (object.Key != key && object.Key != nextKey) || object.Size != 19_200_024 {
			t.Fatalf("clear RustFS listing has an unexpected object: %+v", object)
		}
	}

	recovered, err := objects.Get(ctx, nextKey)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(recovered)
	if err := errors.Join(err, recovered.Close()); err != nil || sha256.Sum256(got) != digest {
		t.Fatalf("Rokuban could not recover the rclone payload: %v", err)
	}
	recovered, err = objects.GetFrom(ctx, nextKey, 16_777_255)
	if err != nil {
		t.Fatal(err)
	}
	got, err = io.ReadAll(recovered)
	if err := errors.Join(err, recovered.Close()); err != nil || !bytes.Equal(got, payload[16_777_255:]) {
		t.Fatalf("Rokuban could not decrypt a range past the nonce carry: %v", err)
	}
}

func rcloneContentPath(t *testing.T, title string, start time.Time) string {
	t.Helper()
	data := contentpath.NewData(title, start, "27", 5168, "GR")
	key, err := contentpath.Build("{{.Year}}/{{.Month}}/{{.Day}}/{{.Title}}_{{.Hour}}{{.Min}}", data)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func TestCryptRcloneNames(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "raw"), 0o700); err != nil {
		t.Fatal(err)
	}
	remote := rcloneCryptEnv(t, dir)
	clearNames := []string{"abc", "abc/def.mp4", "番組-v2026-11-24-090000-000/特別編-v2026-11-24-090000-001.mp4", "a-v2026-11-24-090000-001.mp4"}
	api := newMemoryS3()
	objects := memoryCryptStore(t, api, testPassword, testSalt, false)
	for _, key := range clearNames {
		if err := objects.Put(t.Context(), key, bytes.NewReader(nil), 0, sha256.Sum256(nil), ""); err != nil {
			t.Fatal(err)
		}
	}
	var sealedNames []string
	for key := range api.objects {
		if key != ".rokuban-crypt" {
			sealedNames = append(sealedNames, key)
		}
	}
	slices.Sort(sealedNames)

	runBackend := func(command string, paths []string) []string {
		t.Helper()
		out := rclone(t, dir, remote, append([]string{"backend", command, "compat:", "--json"}, paths...)...)
		var names []string
		if err := json.Unmarshal([]byte(out), &names); err != nil {
			t.Fatalf("reading rclone backend %s JSON: %v", command, err)
		}
		slices.Sort(names)
		return names
	}
	if encoded := runBackend("encode", clearNames); !slices.Equal(encoded, sealedNames) {
		t.Fatalf("rclone backend encode = %q, Rokuban = %q", encoded, sealedNames)
	}
	slices.Sort(clearNames)
	if decoded := runBackend("decode", sealedNames); !slices.Equal(decoded, clearNames) {
		t.Fatalf("rclone backend decode = %q, want %q", decoded, clearNames)
	}
	listed, err := objects.List(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	var openedNames []string
	for _, object := range listed {
		openedNames = append(openedNames, object.Key)
	}
	slices.Sort(openedNames)
	if !slices.Equal(openedNames, clearNames) {
		t.Fatalf("Rokuban decoded names = %q, want %q", openedNames, clearNames)
	}
}

func rcloneCryptEnv(t *testing.T, dir string) []string {
	t.Helper()
	password := rclone(t, dir, nil, "obscure", testPassword)
	salt := rclone(t, dir, nil, "obscure", testSalt)
	return []string{
		"RCLONE_CONFIG_COMPAT_TYPE=crypt",
		"RCLONE_CONFIG_COMPAT_REMOTE=/fixture/raw",
		"RCLONE_CONFIG_COMPAT_FILENAME_ENCRYPTION=standard",
		"RCLONE_CONFIG_COMPAT_FILENAME_ENCODING=base32",
		"RCLONE_CONFIG_COMPAT_DIRECTORY_NAME_ENCRYPTION=true",
		"RCLONE_CONFIG_COMPAT_PASSWORD=" + password,
		"RCLONE_CONFIG_COMPAT_PASSWORD2=" + salt,
	}
}

func rclone(t *testing.T, dir string, env []string, args ...string) string {
	t.Helper()
	const image = "rclone/rclone:1.76.0@sha256:7cbeec24a44f206067597c0368be4de90ae19a64d5113c7d1938bd737a81e466"
	dockerArgs := []string{"run", "--rm", "--network", "none", "--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()), "--volume", dir + ":/fixture"}
	for _, setting := range env {
		name, _, _ := strings.Cut(setting, "=")
		dockerArgs = append(dockerArgs, "--env", name)
	}
	dockerArgs = append(dockerArgs, image, "--config", "/dev/null")
	cmd := exec.CommandContext(t.Context(), "docker", append(dockerArgs, args...)...)
	cmd.Env = append(os.Environ(), env...)
	operation := args[0]
	if operation == "backend" {
		operation += " " + args[1]
	}
	out, err := rcloneOutput(cmd, operation)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
