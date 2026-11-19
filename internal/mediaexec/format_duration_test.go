package mediaexec_test

import (
	"context"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/mediaexec"
	"github.com/fetburner/rokuban/internal/testexec"
	"github.com/fetburner/rokuban/internal/testutil"
)

func TestProbeDuration_IgnoresStderr(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ffprobe := filepath.Join(dir, "ffprobe")
	const wantSeconds = "2546.360222"
	script := "#!/bin/sh\n" +
		"echo '[mpeg2video @ 0x1234] Invalid frame dimensions 0x0.' >&2\n" +
		"printf '%s\\n' '" + wantSeconds + "'\n"
	testexec.WriteExecutable(t, ffprobe, []byte(script))

	got, err := mediaexec.ProbeFormatDuration(context.Background(), ffprobe, "input.m2ts", mediaexec.CommandOutput)
	testutil.RequireNoError(t, err, "mediaexec.ProbeFormatDuration() error")
	wantSec, err := strconv.ParseFloat(wantSeconds, 64)
	testutil.RequireNoError(t, err, "strconv.ParseFloat")
	want := time.Duration(wantSec * float64(time.Second))
	if got != want {
		t.Errorf("mediaexec.ProbeFormatDuration() = %v, want %v", got, want)
	}
}
