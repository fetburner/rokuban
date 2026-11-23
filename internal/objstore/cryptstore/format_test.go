package cryptstore

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/contentpath"
	"github.com/fetburner/rokuban/internal/encode"
	"github.com/fetburner/rokuban/internal/mediapath"
	"github.com/fetburner/rokuban/internal/objstore"
)

func TestContentPathKeyLimit(t *testing.T) {
	s, err := New(struct{ objstore.DeclaredSHA256Store }{}, "test", "test password", "test salt", false)
	if err != nil {
		t.Fatal(err)
	}
	title := strings.Repeat("日本語の長い番組名", 50)
	data := contentpath.NewData(title, time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC), "地上波", 101, "GR")
	for _, tc := range []struct {
		name, tmpl string
		wantLength int
		wantErr    error
	}{
		{name: "default template", tmpl: contentpath.DefaultTemplate, wantLength: 517},
		{name: "repeated title directories", tmpl: "{{.Title}}/{{.Title}}/{{.Title}}", wantErr: errKey},
	} {
		t.Run(tc.name, func(t *testing.T) {
			content, err := contentpath.Build(tc.tmpl, data)
			if err != nil {
				t.Fatal(err)
			}
			original, err := mediapath.SiteRelPath("tokyo", content)
			if err != nil {
				t.Fatal(err)
			}
			derived, err := encode.EncodedRelPath(original, "h264", "mp4", 42, false)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := s.objectKey(derived)
			if !errors.Is(err, tc.wantErr) || len(raw) != tc.wantLength {
				t.Fatalf("encrypted key length = %d, error = %v, want length %d, error %v", len(raw), err, tc.wantLength, tc.wantErr)
			}
			if tc.wantErr == nil {
				if name, ok := s.openPath(raw); !ok || name != derived {
					t.Fatal("encrypted key does not recover the original rel_path")
				}
			}
		})
	}
}
