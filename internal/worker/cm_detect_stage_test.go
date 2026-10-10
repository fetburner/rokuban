package worker

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/testutil"
)

// CMDetectWorker.Work の各失敗経路が、対応する stage を attempt 行に書く。
// stage はリテラルで比べる（実装の定数と比べると、ラベルを書き換えても通ってしまう）。
func TestCMDetectWorkWritesFailureStagePerPath(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	const ffprobeSizeOnly = `
case "$*" in
  *width,height*) echo '{"programs": [{"streams": [{"width": 1440, "height": 1080}]}], "streams": [{"width": 1440, "height": 1080}]}' ;;
  *) exit 1 ;;
esac
`
	for i, tt := range []struct {
		name  string
		stage string
		// tamper は正常なダミー群を、この経路だけが失敗する形に壊す。
		tamper func(t *testing.T, tools cmToolset, mediaDir string, id int64) (scratchDir string)
	}{
		{"logoframe fails", "logo", func(t *testing.T, tools cmToolset, _ string, _ int64) string {
			writeExecutable(t, filepath.Join(tools.binDir, "logoframe"), "exit 1\n")
			return ""
		}},
		{"join_logo_scp fails", "join", func(t *testing.T, tools cmToolset, _ string, _ int64) string {
			writeExecutable(t, filepath.Join(tools.binDir, "join_logo_scp"), "exit 1\n")
			return ""
		}},
		{"obs_cut.avs is not written", "parse", func(t *testing.T, tools cmToolset, _ string, _ int64) string {
			writeExecutable(t, filepath.Join(tools.binDir, "join_logo_scp"), "exit 0\n")
			return ""
		}},
		{"obs_cut.avs has no Trim", "parse", func(t *testing.T, tools cmToolset, _ string, _ int64) string {
			writeExecutable(t, filepath.Join(tools.binDir, "join_logo_scp"),
				"while [ $# -gt 0 ]; do case \"$1\" in -o) o=$2;; esac; shift; done\necho '# no Trim' > \"$o\"\n")
			return ""
		}},
		{"size probe fails", "probe", func(t *testing.T, tools cmToolset, _ string, _ int64) string {
			writeExecutable(t, tools.ffprobe, "exit 1\n")
			return ""
		}},
		{"duration probe fails", "probe", func(t *testing.T, tools cmToolset, _ string, _ int64) string {
			writeExecutable(t, tools.ffprobe, ffprobeSizeOnly)
			return ""
		}},
		{"original path escapes the media dir", "setup", func(t *testing.T, _ cmToolset, _ string, id int64) string {
			if _, err := pool.Exec(ctx, `UPDATE media_assets SET rel_path = '../escape.ts' WHERE recording_id = $1`, id); err != nil {
				t.Fatal(err)
			}
			return ""
		}},
		{"scratch root cannot be created", "setup", func(t *testing.T, _ cmToolset, mediaDir string, _ int64) string {
			blocker := filepath.Join(mediaDir, "blocker")
			if err := os.WriteFile(blocker, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			return blocker
		}},
		{"result row cannot be saved", "save", func(t *testing.T, _ cmToolset, _ string, _ int64) string {
			for _, stmt := range []string{
				`CREATE OR REPLACE FUNCTION cm_stage_test_reject() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'rejected'; END $$`,
				`CREATE TRIGGER cm_stage_test_reject BEFORE INSERT ON recording_cm_detections FOR EACH ROW EXECUTE FUNCTION cm_stage_test_reject()`,
			} {
				if _, err := pool.Exec(ctx, stmt); err != nil {
					t.Fatal(err)
				}
			}
			t.Cleanup(func() {
				_, _ = pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS cm_stage_test_reject ON recording_cm_detections`)
			})
			return ""
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			mediaDir := t.TempDir()
			id := seedCMRecording(t, pool, mediaDir, int32(1000+i))
			tools := newFakeCMTools(t, buildTestLGD(4, 3, 1000, 4080), 0, "Trim(0,299)", "10.010000")
			w := newCMDetectTestWorker(pool, mediaDir, tools)
			if scratch := tt.tamper(t, tools, mediaDir, id); scratch != "" {
				w.ScratchDir = scratch
			}
			if err := w.Work(ctx, cmJob(id, 3)); err == nil {
				t.Fatal("Work succeeded on a broken path")
			}
			var state string
			var stage *string
			if err := pool.QueryRow(ctx, `SELECT state, stage FROM recording_cm_attempts WHERE recording_id = $1`, id).Scan(&state, &stage); err != nil {
				t.Fatalf("attempt row: %v", err)
			}
			if state != "failed" || stage == nil || *stage != tt.stage {
				t.Errorf("attempt = %q / %v, want failed / %q", state, stage, tt.stage)
			}
		})
	}
}

// 再試行の開始（running）は、前の失敗が付けた stage を持ち越さない。
func TestBeginCMDetectionAttemptClearsStage(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	id := seedCMRecording(t, pool, t.TempDir(), 1100)
	q := sqlcgen.New(pool)
	firstAttempt := startCMDetectionTestAttempt(t, ctx, q, id)
	stage := "logo"
	markCMDetectionTestFailure(t, ctx, q, id, firstAttempt, "retrying", &stage, nil)
	if got := startCMDetectionTestAttempt(t, ctx, q, id); got != 2 {
		t.Fatalf("second attempt count = %d, want 2", got)
	}
	var got *string
	if err := pool.QueryRow(ctx, `SELECT stage FROM recording_cm_attempts WHERE recording_id = $1`, id).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("stage after running = %q, want NULL", *got)
	}
}
