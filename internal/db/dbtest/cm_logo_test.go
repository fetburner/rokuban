package dbtest_test

import (
	"testing"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/testutil"
)

func TestCMLogo_DefaultsAndOptions(t *testing.T) {
	pool := testutil.SetupDB(t)

	dbtest.CMLogo(t, pool)
	var lgd []byte
	var learnedFrom *int64
	testutil.RequireNoError(t, pool.QueryRow(t.Context(), `SELECT lgd, learned_from FROM cm_logos WHERE network_id = 32736 AND service_id = 1024`).Scan(&lgd, &learnedFrom), "reading default CM logo")
	if string(lgd) != "lgd" || learnedFrom != nil {
		t.Fatalf("default CM logo = lgd %q, learned_from %v; want %q and nil", lgd, learnedFrom, "lgd")
	}

	id := dbtest.Recording(t, pool)
	dbtest.CMLogo(t, pool, func(p *dbtest.CMLogoParams) {
		p.ServiceID = 1025
		p.LGD = []byte("other")
		p.LearnedFrom = &id
		p.CodedWidth = 1920
		p.PreviewPNG = []byte{1, 2, 3}
	})
	var codedWidth int32
	var preview []byte
	testutil.RequireNoError(t, pool.QueryRow(t.Context(), `SELECT learned_from, coded_width, preview_png FROM cm_logos WHERE network_id = 32736 AND service_id = 1025`).Scan(&learnedFrom, &codedWidth, &preview), "reading overridden CM logo")
	if learnedFrom == nil || *learnedFrom != id || codedWidth != 1920 || string(preview) != "\x01\x02\x03" {
		t.Fatalf("overridden CM logo learned_from = %v, coded_width = %d, preview = %v, want %d, 1920 and [1 2 3]", learnedFrom, codedWidth, preview, id)
	}
}
