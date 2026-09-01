package epgstation_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/fetburner/rokuban/internal/epgstation"
)

// TestListRules_SinglePage は 1 ページで完結するケースを確認する。
func TestListRules_SinglePage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{
			"rules": [
				{"id": 1, "isTimeSpecification": false,
				 "searchOption": {"keyword": "ニュース", "name": true, "keyRegExp": false,
				                   "channelIds": [3273601024], "isFree": true},
				 "reserveOption": {"enable": true, "allowEndLack": true, "avoidDuplicate": false},
				 "saveOption": {"recordedFormat": "%YEAR%%MONTH%%DAY%_%TITLE%"}}
			],
			"total": 1
		}`)
	}))
	defer srv.Close()

	c := epgstation.NewClient(srv.URL, srv.Client())
	got, err := c.ListRules(context.Background())
	if err != nil {
		t.Fatalf("ListRules: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1", len(got))
	}
	if got[0].ID != 1 || got[0].SearchOption.Keyword != "ニュース" || !got[0].SearchOption.Name {
		t.Errorf("got[0] = %+v", got[0])
	}
	if got[0].SearchOption.IsFree == nil || !*got[0].SearchOption.IsFree {
		t.Errorf("got[0].SearchOption.IsFree = %v, want true", got[0].SearchOption.IsFree)
	}
	if got[0].SaveOption == nil || got[0].SaveOption.RecordedFormat != "%YEAR%%MONTH%%DAY%_%TITLE%" {
		t.Errorf("got[0].SaveOption = %+v", got[0].SaveOption)
	}
}

// TestListRules_TwoPages はページングが offset を進めて全件回収することを確認する。
func TestListRules_TwoPages(t *testing.T) {
	const total = 150
	var offsets []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		offset := r.URL.Query().Get("offset")
		offsets = append(offsets, offset)

		limit := 100
		start := 0
		_, _ = fmt.Sscanf(offset, "%d", &start)
		end := start + limit
		if end > total {
			end = total
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"rules": [`)
		for i := start; i < end; i++ {
			if i > start {
				_, _ = fmt.Fprint(w, ",")
			}
			_, _ = fmt.Fprintf(w, `{"id": %d, "isTimeSpecification": false,
				"searchOption": {"keyword": "k%d", "name": true},
				"reserveOption": {"enable": true, "allowEndLack": true, "avoidDuplicate": false}}`, i, i)
		}
		_, _ = fmt.Fprintf(w, `], "total": %d}`, total)
	}))
	defer srv.Close()

	c := epgstation.NewClient(srv.URL, srv.Client())
	got, err := c.ListRules(context.Background())
	if err != nil {
		t.Fatalf("ListRules: %v", err)
	}
	if len(got) != total {
		t.Fatalf("len(got) = %d, want %d", len(got), total)
	}
	if len(offsets) != 2 || offsets[0] != "0" || offsets[1] != "100" {
		t.Errorf("offsets = %v, want [0 100]", offsets)
	}
}
