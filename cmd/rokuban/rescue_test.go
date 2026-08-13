package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/catalog"
	"github.com/fetburner/rokuban/internal/testutil"
)

// 最新世代が不完全なら 1 世代前から復元し、**飛ばしたことを運用者に見せる**
// こと（docs/storage.md §8。黙って古い世代へ落ちない）。
func TestRunRescue_FallsBackAndReportsSkippedGeneration(t *testing.T) {
	pool := testutil.SetupDB(t)
	mediaDir := t.TempDir()

	if _, err := catalog.Write(mediaDir, &catalog.Document{
		Version:    catalog.Version,
		ExportedAt: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
	}, 7); err != nil {
		t.Fatalf("writing the previous generation: %v", err)
	}
	// 書き込み途中で止まった世代（manifest 未着）。
	torn := filepath.Join(catalog.Dir(mediaDir), "catalog-20260702T000000Z")
	if err := os.MkdirAll(torn, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(torn, catalog.DocumentFilename), []byte(`{"vers`), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runRescue(context.Background(), pool, mediaDir, "default", &out); err != nil {
		t.Fatalf("runRescue: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, filepath.Join("catalog-20260701T000000Z", catalog.DocumentFilename)) {
		t.Errorf("output = %q, want the previous generation as the source", got)
	}
	if !strings.Contains(got, "skipped incomplete generation catalog-20260702T000000Z") {
		t.Errorf("output = %q, want the skipped generation to be reported", got)
	}
}

// 旧形式のフラットファイルは世代ではないので、飛ばしたときの文言を世代と
// 分けること（運用者が「世代ディレクトリが壊れている」と誤読しない）。
func TestRunRescue_SkippedLegacyFileIsNotCalledAGeneration(t *testing.T) {
	pool := testutil.SetupDB(t)
	mediaDir := t.TempDir()
	catalogDir := catalog.Dir(mediaDir)
	if err := os.MkdirAll(catalogDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// 新しい方は壊れた旧形式ファイル、古い方は読める旧形式ファイル。
	if err := os.WriteFile(filepath.Join(catalogDir, "catalog-20260702T000000Z.json"),
		[]byte(`{"version":1,`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(catalogDir, "catalog-20260701T000000Z.json"),
		[]byte(`{"version":1,"exportedAt":"2026-07-01T00:00:00Z"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runRescue(context.Background(), pool, mediaDir, "default", &out); err != nil {
		t.Fatalf("runRescue: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "skipped unreadable catalog file catalog-20260702T000000Z.json") {
		t.Errorf("output = %q, want the legacy file reported as a file", got)
	}
	if strings.Contains(got, "incomplete generation catalog-20260702T000000Z.json") {
		t.Errorf("output = %q, a flat file must not be called a generation", got)
	}
	if !strings.Contains(got, "completeness was not verified") {
		t.Errorf("output = %q, want the legacy fallback to be flagged as unverified", got)
	}
}

// 完成世代が 1 つも無いときは走査に落ちるが、「catalog が無い」と「あったが
// 全部不完全」を区別して出すこと。
func TestRunRescue_DistinguishesNoCatalogFromNoCompleteGeneration(t *testing.T) {
	pool := testutil.SetupDB(t)

	t.Run("no catalog at all", func(t *testing.T) {
		var out bytes.Buffer
		if err := runRescue(context.Background(), pool, t.TempDir(), "default", &out); err != nil {
			t.Fatalf("runRescue: %v", err)
		}
		if !strings.Contains(out.String(), "catalog not found") {
			t.Errorf("output = %q, want %q", out.String(), "catalog not found")
		}
	})

	t.Run("only incomplete generations", func(t *testing.T) {
		mediaDir := t.TempDir()
		torn := filepath.Join(catalog.Dir(mediaDir), "catalog-20260702T000000Z")
		if err := os.MkdirAll(torn, 0o755); err != nil {
			t.Fatal(err)
		}

		var out bytes.Buffer
		if err := runRescue(context.Background(), pool, mediaDir, "default", &out); err != nil {
			t.Fatalf("runRescue: %v", err)
		}
		if !strings.Contains(out.String(), "no complete catalog generation") {
			t.Errorf("output = %q, want %q", out.String(), "no complete catalog generation")
		}
		if !strings.Contains(out.String(), "skipped incomplete generation catalog-20260702T000000Z") {
			t.Errorf("output = %q, want the incomplete generation to be reported", out.String())
		}
	})
}
