package epgstation_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/fetburner/rokuban/internal/epgstation"
)

// TestListReserves_SinglePage は 1 ページで完結するケース（total <= limit）を確認する。
func TestListReserves_SinglePage(t *testing.T) {
	var gotQueries []url.Values

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQueries = append(gotQueries, r.URL.Query())
		_, _ = fmt.Fprint(w, `{
			"reserves": [
				{"id": 1, "ruleId": 5, "isSkip": false, "isConflict": false, "isOverlap": false,
				 "isTimeSpecified": false, "programId": 327360102415397,
				 "startAt": 1785000000000, "endAt": 1785001800000, "name": "番組A"},
				{"id": 2, "isSkip": false, "isConflict": false, "isOverlap": false,
				 "isTimeSpecified": true,
				 "startAt": 1785100000000, "endAt": 1785101800000, "name": "時刻指定予約"}
			],
			"total": 2
		}`)
	}))
	defer srv.Close()

	c := epgstation.NewClient(srv.URL, srv.Client())
	got, err := c.ListReserves(context.Background())
	if err != nil {
		t.Fatalf("ListReserves: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2", len(got))
	}
	if got[0].ID != 1 || got[0].RuleID == nil || *got[0].RuleID != 5 {
		t.Errorf("got[0] = %+v", got[0])
	}
	if got[0].ProgramID == nil || *got[0].ProgramID != 327360102415397 {
		t.Errorf("got[0].ProgramID = %v, want 327360102415397", got[0].ProgramID)
	}
	if got[1].ProgramID != nil {
		t.Errorf("got[1].ProgramID = %v, want nil (time-specified)", *got[1].ProgramID)
	}
	if !got[1].IsTimeSpecified {
		t.Errorf("got[1].IsTimeSpecified = false, want true")
	}

	if len(gotQueries) != 1 {
		t.Fatalf("request count = %d, want 1", len(gotQueries))
	}
	if q := gotQueries[0].Get("type"); q != "all" {
		t.Errorf("type = %q, want all", q)
	}
	if q := gotQueries[0].Get("offset"); q != "0" {
		t.Errorf("first request offset = %q, want 0", q)
	}
}

// TestListReserves_TwoPages は total がページサイズを超えるケースで
// offset を進めながら全ページを回収することを確認する。
func TestListReserves_TwoPages(t *testing.T) {
	const total = 150 // reservesPageLimit (100) を超えて 2 ページに跨る
	var offsets []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		offset := r.URL.Query().Get("offset")
		offsets = append(offsets, offset)

		limit := 100
		start := 0
		if _, err := fmt.Sscanf(offset, "%d", &start); err != nil {
			t.Errorf("parsing offset %q: %v", offset, err)
		}

		end := start + limit
		if end > total {
			end = total
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"reserves": [`)
		for i := start; i < end; i++ {
			if i > start {
				_, _ = fmt.Fprint(w, ",")
			}
			_, _ = fmt.Fprintf(w, `{"id": %d, "isSkip": false, "isConflict": false, "isOverlap": false,
				"isTimeSpecified": false, "programId": %d, "startAt": 0, "endAt": 0, "name": "p%d"}`,
				i, 1000+i, i)
		}
		_, _ = fmt.Fprintf(w, `], "total": %d}`, total)
	}))
	defer srv.Close()

	c := epgstation.NewClient(srv.URL, srv.Client())
	got, err := c.ListReserves(context.Background())
	if err != nil {
		t.Fatalf("ListReserves: %v", err)
	}
	if len(got) != total {
		t.Fatalf("len(got) = %d, want %d", len(got), total)
	}
	if len(offsets) != 2 {
		t.Fatalf("request count = %d, want 2 (100 + 50)", len(offsets))
	}
	if offsets[0] != "0" || offsets[1] != "100" {
		t.Errorf("offsets = %v, want [0 100]", offsets)
	}
	// 順序も保たれていること
	for i, r := range got {
		if r.ID != int64(i) {
			t.Fatalf("got[%d].ID = %d, want %d", i, r.ID, i)
		}
	}
}

// TestListReserves_ErrorStatus はエラーステータスが APIError として返ることを確認する。
func TestListReserves_ErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = fmt.Fprint(w, "boom")
	}))
	defer srv.Close()

	c := epgstation.NewClient(srv.URL, srv.Client())
	_, err := c.ListReserves(context.Background())
	if err == nil {
		t.Fatal("ListReserves: want error, got nil")
	}
	var apiErr *epgstation.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error = %v, want *epgstation.APIError in chain", err)
	}
	if apiErr.StatusCode != http.StatusInternalServerError {
		t.Errorf("StatusCode = %d, want 500", apiErr.StatusCode)
	}
}
