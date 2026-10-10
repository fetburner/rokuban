//go:build conformance

package conformance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/mirakc"
	"github.com/fetburner/rokuban/internal/mirakc/conformance/fixture"
	"github.com/fetburner/rokuban/internal/programid"
)

func TestIssue1317ActualBinaryResumesAcrossSlicesOnMirakcRecording(t *testing.T) {
	dbURL := os.Getenv("ROKUBAN_CONFORMANCE_TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("ROKUBAN_CONFORMANCE_TEST_DATABASE_URL is required for the isolated binary conformance run")
	}

	dir := testDir(t)
	root := repoRoot(t)
	binary := buildIssue1317Binary(t, root, dir)
	tunerBin := buildFixtureTuner(t, dir)
	container := startMirakc(t, dir, tunerBin, "")
	client := mirakc.NewClient(container.baseURL, nil)
	ctx := context.Background()

	dbConfig := parseIssue1317DBURL(t, dbURL)
	if dbConfig.password == "" {
		dbConfig.password = "test"
	}
	mediaDir := filepath.Join(dir, "media")
	scratchDir := filepath.Join(dir, "scratch")
	if err := os.MkdirAll(mediaDir, 0o755); err != nil {
		t.Fatalf("creating media dir: %v", err)
	}
	if err := os.MkdirAll(scratchDir, 0o755); err != nil {
		t.Fatalf("creating scratch dir: %v", err)
	}
	configPath := filepath.Join(dir, "rokuban.yml")
	configText := fmt.Sprintf(`db:
  host: %s
  port: %s
  user: %s
  password: %s
  database: %s
  sslmode: disable
storage:
  media_dir: %s
  scratch_dir: %s
mirakcs:
  - site: default
    url: %s
ingest:
  concurrency: 3
  stall_timeout: 30s
worker:
  periodic_jobs: false
  rescue_stuck_jobs_after: 6m
`, strconv.Quote(dbConfig.host), dbConfig.port, strconv.Quote(dbConfig.user), strconv.Quote(dbConfig.password),
		strconv.Quote(dbConfig.database), strconv.Quote(mediaDir), strconv.Quote(scratchDir), strconv.Quote(container.baseURL))
	if err := os.WriteFile(configPath, []byte(configText), 0o600); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	runIssue1317Binary(t, binary, "--config", configPath, "config", "validate")
	runIssue1317Binary(t, binary, "--config", configPath, "migrate", "up")

	serviceID := programid.ServiceID(fixture.NetworkID, fixture.ServiceID)
	programID := programid.ComposeProgramID(fixture.NetworkID, fixture.ServiceID, fixture.EventID)
	waitForService(t, ctx, client, serviceID)
	waitForProgram(t, ctx, client, programID)
	if _, err := client.CreateSchedule(ctx, mirakc.ScheduleInput{
		ProgramID: programID,
		Options:   mirakc.Options{ContentPath: strPtr("conformance/issue-1317-slice.ts"), Priority: 1},
		Tags:      []string{mirakc.ProgramTag(programID)},
	}); err != nil {
		t.Fatalf("creating fixture recording schedule: %v", err)
	}
	recordID := waitForRecord(t, ctx, client, programID)
	waitForRecordingStatus(t, ctx, client, recordID, "recording", recordingStartTimeout)

	runIssue1317Binary(t, binary, "--config", configPath, "enqueue", "record-sweep", "--site", "default")
	runIssue1317Binary(t, binary, "--config", configPath, "server", "--roles", "worker", "--sites", "default",
		"--queues", "watcher", "--once", "--once-idle-timeout", "5s")

	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connecting to isolated test DB: %v", err)
	}
	defer pool.Close()
	var recordings int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM recordings WHERE site = 'default'").Scan(&recordings); err != nil {
		t.Fatalf("checking record_sweep result: %v", err)
	}
	if recordings != 1 {
		t.Fatalf("record_sweep imported %d recordings, want 1", recordings)
	}

	serverOnce := func() string {
		return runIssue1317Binary(t, binary, "--config", configPath, "server", "--roles", "worker", "--sites", "default",
			"--queues", "ingest", "--once", "--once-idle-timeout", "5s")
	}
	firstOutput := serverOnce()
	if !strings.Contains(firstOutput, "job_done") {
		t.Fatalf("first ingest process did not consume one Work item:\n%s", firstOutput)
	}
	if got := countIssue1317Checkpoints(t, mediaDir); got == 0 {
		t.Fatalf("first Work did not leave a checkpoint; process output:\n%s", firstOutput)
	}
	firstRecord, err := client.GetRecord(ctx, recordID)
	if err != nil {
		t.Fatalf("checking recording after first bounded Work: %v", err)
	}
	if firstRecord.Recording.Status != "recording" {
		t.Fatalf("mirakc recording status after first Work = %q, want recording", firstRecord.Recording.Status)
	}
	// Ingest deletes the mirakc record after committing the original, so save its
	// completed SHA before allowing a later slice to finish the job.
	waitForRecordingStatus(t, ctx, client, recordID, "finished", recordingFinishTimeout)
	finished, err := client.GetRecord(ctx, recordID)
	if err != nil {
		t.Fatalf("getting finished mirakc record before ingest resumes: %v", err)
	}
	// content.sha256 may be published asynchronously after status becomes finished.
	// Hash the completed stream now so the expected bytes remain available after
	// ingest deletes the mirakc record.
	stream, streamLength, err := client.StreamRecord(ctx, recordID, 0)
	if err != nil {
		t.Fatalf("opening finished mirakc record stream: %v", err)
	}
	wantHasher := sha256.New()
	streamBytes, copyErr := io.Copy(wantHasher, stream)
	closeErr := stream.Close()
	if copyErr != nil {
		t.Fatalf("reading finished mirakc record stream: %v", copyErr)
	}
	if closeErr != nil {
		t.Fatalf("closing finished mirakc record stream: %v", closeErr)
	}
	if streamLength >= 0 && streamBytes != streamLength {
		t.Fatalf("finished mirakc stream length = %d, want %d", streamBytes, streamLength)
	}
	if finished.Content.Length != nil && streamBytes != int64(*finished.Content.Length) {
		t.Fatalf("finished mirakc stream length = %d, content.length = %d", streamBytes, *finished.Content.Length)
	}
	want := wantHasher.Sum(nil)
	if finished.Content.Sha256 != nil {
		metadataSHA256, err := hex.DecodeString(*finished.Content.Sha256)
		if err != nil {
			t.Fatalf("decoding mirakc SHA-256: %v", err)
		}
		if !bytes.Equal(metadataSHA256, want) {
			t.Fatalf("mirakc content.sha256 = %x, finished stream SHA-256 = %x", metadataSHA256, want)
		}
	}

	var relPath string
	workItems := 1
	for i := 0; i < 8; i++ {
		err := pool.QueryRow(ctx, "SELECT rel_path FROM media_assets WHERE kind = 'original' AND state = 'active'").Scan(&relPath)
		if err == nil {
			break
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("checking ingest commit: %v", err)
		}
		output := serverOnce()
		workItems++
		if !strings.Contains(output, "job_done") && !strings.Contains(output, "idle_timeout") {
			t.Fatalf("ingest process ended without consuming or idling:\n%s", output)
		}
	}
	if relPath == "" {
		t.Fatal("actual rokuban binary did not commit an original media asset after resuming slices")
	}

	assetPath := filepath.Join(mediaDir, filepath.FromSlash(relPath))
	data, err := os.ReadFile(assetPath)
	if err != nil {
		t.Fatalf("reading committed original: %v", err)
	}
	got := sha256.Sum256(data)
	if !bytes.Equal(got[:], want) {
		t.Fatalf("committed file SHA-256 = %x, mirakc = %x", got, want)
	}
	t.Logf("actual binary resumed %d once-mode Work items and committed %d bytes from a pinned mirakc recording", workItems, len(data))
}

type issue1317DBConfig struct{ host, port, user, password, database string }

func parseIssue1317DBURL(t *testing.T, value string) issue1317DBConfig {
	t.Helper()
	u, err := url.Parse(value)
	if err != nil {
		t.Fatalf("parsing test DB URL: %v", err)
	}
	user := ""
	password := ""
	if u.User != nil {
		user = u.User.Username()
		password, _ = u.User.Password()
	}
	return issue1317DBConfig{host: u.Hostname(), port: u.Port(), user: user, password: password, database: strings.TrimPrefix(u.Path, "/")}
}

func buildIssue1317Binary(t *testing.T, root, dir string) string {
	t.Helper()
	sourcePath, err := filepath.Abs(filepath.Join(root, "internal", "worker", "ingest.go"))
	if err != nil {
		t.Fatalf("resolving ingest source: %v", err)
	}
	source, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatalf("reading ingest source: %v", err)
	}
	const original = "var ingestTransferSlice = 4 * time.Minute"
	const shortened = "var ingestTransferSlice = 2 * time.Second"
	if !bytes.Contains(source, []byte(original)) {
		t.Fatalf("could not find slice assignment %q", original)
	}
	overlaySource := filepath.Join(dir, "ingest-short.go")
	if err := os.WriteFile(overlaySource, bytes.Replace(source, []byte(original), []byte(shortened), 1), 0o600); err != nil {
		t.Fatalf("writing overlay source: %v", err)
	}
	overlayPath := filepath.Join(dir, "overlay.json")
	overlay, err := json.Marshal(struct {
		Replace map[string]string `json:"Replace"`
	}{Replace: map[string]string{sourcePath: overlaySource}})
	if err != nil {
		t.Fatalf("encoding Go overlay: %v", err)
	}
	if err := os.WriteFile(overlayPath, overlay, 0o600); err != nil {
		t.Fatalf("writing Go overlay: %v", err)
	}
	binary := filepath.Join(dir, "rokuban")
	cmd := exec.Command("go", "build", "-overlay", overlayPath, "-o", binary, "./cmd/rokuban")
	cmd.Dir = root
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Run(); err != nil {
		t.Fatalf("building actual binary with shortened slice: %v\n%s", err, output.String())
	}
	return binary
}

func runIssue1317Binary(t *testing.T, binary string, args ...string) string {
	t.Helper()
	cmd := exec.Command(binary, args...)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Run(); err != nil {
		t.Fatalf("running actual rokuban binary %v: %v\n%s", args, err, output.String())
	}
	return output.String()
}

func countIssue1317Checkpoints(t *testing.T, root string) int {
	t.Helper()
	count := 0
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".checkpoint") {
			count++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking ingest media dir: %v", err)
	}
	return count
}
