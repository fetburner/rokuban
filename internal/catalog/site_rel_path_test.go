package catalog

import "testing"

func TestSiteRelPath(t *testing.T) {
	t.Run("prefixes the site namespace", func(t *testing.T) {
		got, err := SiteRelPath("tokyo", "archive/show.m2ts")
		if err != nil {
			t.Fatalf("SiteRelPath: %v", err)
		}
		const want = "sites/tokyo/archive/show.m2ts"
		if got != want {
			t.Errorf("SiteRelPath = %q, want %q", got, want)
		}
	})

	for _, relPath := range []string{"", ".", "/", "archive/", "archive/.", "archive/.."} {
		t.Run("rejects unusable rel_path "+relPath, func(t *testing.T) {
			if got, err := SiteRelPath("tokyo", relPath); err == nil {
				t.Errorf("SiteRelPath = %q, want an error", got)
			}
		})
	}

	for _, relPath := range []string{
		".rokuban-ingest-42.ts",
		"archive/.rokuban-rel-path-lock-deadbeef.lock",
		"archive/.rokuban-encode-job.ts",
		"archive/.rokuban-media-asset-stage",
	} {
		t.Run("rejects reserved filename "+relPath, func(t *testing.T) {
			if got, err := SiteRelPath("tokyo", relPath); err == nil {
				t.Errorf("SiteRelPath = %q, want an error", got)
			}
		})
	}
}
