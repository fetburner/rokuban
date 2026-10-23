// Package mirakctest は mirakc.Client を使うテスト向けの状態保持 HTTP fake を提供する。
//
// fake はテストが設定した状態を API 越しに返し、mirakc の時間経過や録画ライフサイクルは
// 模倣しない。実物 mirakc の契約は internal/mirakc/conformance が検査する。
package mirakctest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/fetburner/rokuban/internal/mirakc"
	"github.com/fetburner/rokuban/internal/programid"
)

const (
	servicesPath  = "/api/services"
	programsPath  = "/api/programs"
	tunersPath    = "/api/tuners"
	schedulesPath = "/api/recording/schedules"
	recordsPath   = "/api/recording/records"
)

// Call は fake が受け取った HTTP リクエストのメソッドとパスを記録する。
type Call struct {
	Method    string
	Path      string
	Query     string
	Range     string
	ProgramID int64
}

type failure struct {
	remaining int
	status    int
}

// Fake は mirakc.Client が使うテスト用 HTTP サーバーと、テストが置く API 状態を持つ。
type Fake struct {
	mu sync.Mutex

	server *httptest.Server

	services []mirakc.Service
	programs []mirakc.Program
	tuners   []mirakc.Tuner

	schedules []mirakc.Schedule
	records   []mirakc.Record
	streams   map[string][]byte

	calls    []Call
	failures map[string]failure
}

// New はテスト用の mirakc HTTP fake を作り、テスト終了時にサーバーを閉じる。
func New(t testing.TB) *Fake {
	t.Helper()
	f := &Fake{
		streams:  make(map[string][]byte),
		failures: make(map[string]failure),
	}
	f.server = httptest.NewServer(f)
	t.Cleanup(f.server.Close)
	return f
}

// URL は fake の HTTP サーバーの URL を返す。
func (f *Fake) URL() string { return f.server.URL }

// SetServices は GET /api/services が返すサービスを置く。
func (f *Fake) SetServices(services []mirakc.Service) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.services = clone(services)
}

// SetPrograms は GET /api/programs が返す番組を置く。
func (f *Fake) SetPrograms(programs []mirakc.Program) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.programs = clone(programs)
}

// SetTuners は GET /api/tuners が返すチューナーを置く。
func (f *Fake) SetTuners(tuners []mirakc.Tuner) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tuners = clone(tuners)
}

// SetSchedules は GET /api/recording/schedules が返すスケジュールを置き換える。
func (f *Fake) SetSchedules(schedules []mirakc.Schedule) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.schedules = clone(schedules)
}

// SetSchedule は programId が一致するスケジュールを置き換え、無ければ追加する。
func (f *Fake) SetSchedule(schedule mirakc.Schedule) {
	f.mu.Lock()
	defer f.mu.Unlock()
	schedule = clone(schedule)
	for i := range f.schedules {
		if f.schedules[i].Program.ID == schedule.Program.ID {
			f.schedules[i] = schedule
			return
		}
	}
	f.schedules = append(f.schedules, schedule)
}

// Schedules は現在のスケジュールを programId で引くコピーとして返す。
func (f *Fake) Schedules() map[int64]mirakc.Schedule {
	f.mu.Lock()
	defer f.mu.Unlock()
	schedules := make(map[int64]mirakc.Schedule, len(f.schedules))
	for _, schedule := range f.schedules {
		schedules[schedule.Program.ID] = clone(schedule)
	}
	return schedules
}

// SetScheduleState は指定したスケジュールの state を置き換える。
func (f *Fake) SetScheduleState(programID int64, state string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.schedules {
		if f.schedules[i].Program.ID == programID {
			f.schedules[i].State = state
		}
	}
}

// SetRecords は GET /api/recording/records が返す録画を置き換える。
func (f *Fake) SetRecords(records []mirakc.Record) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.records = clone(records)
}

// SetRecordStream は指定した録画の stream 本文を置く。
func (f *Fake) SetRecordStream(recordID string, data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.streams[recordID] = bytes.Clone(data)
}

// FailNext は method と path が一致する次の count 回のリクエストを status で失敗させる。
func (f *Fake) FailNext(method, path string, count, status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures[method+" "+path] = failure{remaining: count, status: status}
}

// Calls は fake が受け取ったリクエストの記録をコピーして返す。
func (f *Fake) Calls() []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Call(nil), f.calls...)
}

// ServeHTTP は fake の状態を mirakc のテスト用 HTTP API として返す。
func (f *Fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	call := Call{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Range: r.Header.Get("Range")}
	if r.Method == http.MethodPost && r.URL.Path == schedulesPath {
		body, err := io.ReadAll(r.Body)
		if err == nil {
			var input mirakc.ScheduleInput
			if json.Unmarshal(body, &input) == nil {
				call.ProgramID = input.ProgramID
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
	} else if r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, schedulesPath+"/") {
		call.ProgramID, _ = strconv.ParseInt(strings.TrimPrefix(r.URL.Path, schedulesPath+"/"), 10, 64)
	}
	f.calls = append(f.calls, call)
	if injected, ok := f.failures[r.Method+" "+r.URL.Path]; ok && injected.remaining > 0 {
		injected.remaining--
		f.failures[r.Method+" "+r.URL.Path] = injected
		http.Error(w, "injected failure", injected.status)
		return
	}

	switch {
	case r.Method == http.MethodGet && r.URL.Path == servicesPath:
		writeJSON(w, http.StatusOK, nonNil(f.services))
	case r.Method == http.MethodGet && r.URL.Path == programsPath:
		writeJSON(w, http.StatusOK, nonNil(f.programs))
	case r.Method == http.MethodGet && r.URL.Path == tunersPath:
		writeJSON(w, http.StatusOK, nonNil(f.tuners))
	case r.Method == http.MethodGet && r.URL.Path == schedulesPath:
		writeJSON(w, http.StatusOK, nonNil(f.schedules))
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, schedulesPath+"/"):
		f.getSchedule(w, r)
	case r.Method == http.MethodPost && r.URL.Path == schedulesPath:
		f.createSchedule(w, r)
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, schedulesPath+"/"):
		f.deleteSchedule(w, r.URL.Path)
	case r.Method == http.MethodGet && r.URL.Path == recordsPath:
		writeJSON(w, http.StatusOK, nonNil(f.records))
	case strings.HasPrefix(r.URL.Path, recordsPath+"/"):
		f.recordRequest(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (f *Fake) createSchedule(w http.ResponseWriter, r *http.Request) {
	var input mirakc.ScheduleInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	_, _, eventID := programid.SplitProgramID(input.ProgramID)
	duration := int64(300000)
	schedule := mirakc.Schedule{
		State: "scheduled",
		Program: mirakc.Program{
			ID:       input.ProgramID,
			EventID:  eventID,
			Duration: &duration,
		},
		Options: input.Options,
		Tags:    input.Tags,
	}
	for i := range f.schedules {
		if f.schedules[i].Program.ID == input.ProgramID {
			f.schedules[i] = schedule
			writeJSON(w, http.StatusCreated, schedule)
			return
		}
	}
	f.schedules = append(f.schedules, schedule)
	writeJSON(w, http.StatusCreated, schedule)
}

func (f *Fake) deleteSchedule(w http.ResponseWriter, path string) {
	programID, err := strconv.ParseInt(strings.TrimPrefix(path, schedulesPath+"/"), 10, 64)
	if err != nil {
		http.Error(w, "invalid programId", http.StatusBadRequest)
		return
	}
	kept := f.schedules[:0]
	for _, schedule := range f.schedules {
		if schedule.Program.ID != programID {
			kept = append(kept, schedule)
		}
	}
	f.schedules = kept
	w.WriteHeader(http.StatusOK)
}

func (f *Fake) getSchedule(w http.ResponseWriter, r *http.Request) {
	programID, err := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, schedulesPath+"/"), 10, 64)
	if err != nil {
		http.Error(w, "invalid programId", http.StatusBadRequest)
		return
	}
	for _, schedule := range f.schedules {
		if schedule.Program.ID == programID {
			writeJSON(w, http.StatusOK, schedule)
			return
		}
	}
	http.NotFound(w, r)
}

func (f *Fake) recordRequest(w http.ResponseWriter, r *http.Request) {
	relative := strings.TrimPrefix(r.URL.Path, recordsPath+"/")
	if strings.HasSuffix(relative, "/stream") {
		id := strings.TrimSuffix(relative, "/stream")
		f.recordStream(w, r, id)
		return
	}

	for i := range f.records {
		if f.records[i].ID != relative {
			continue
		}
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, http.StatusOK, f.records[i])
		case http.MethodDelete:
			f.records = append(f.records[:i], f.records[i+1:]...)
			writeJSON(w, http.StatusOK, mirakc.RecordRemovalResult{RecordRemoved: true, ContentRemoved: true})
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
		return
	}
	http.NotFound(w, r)
}

func (f *Fake) recordStream(w http.ResponseWriter, r *http.Request, id string) {
	data, ok := f.streams[id]
	if !ok {
		http.NotFound(w, r)
		return
	}
	if r.Method == http.MethodHead {
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.WriteHeader(http.StatusOK)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	WriteRecordStream(w, r, data)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

// nonNil は nil の一覧を空配列として JSON に書かせる。実物の mirakc は空の一覧を [] で返す。
func nonNil[T any](list []T) []T {
	if list == nil {
		return []T{}
	}
	return list
}

func clone[T any](value T) T {
	encoded, err := json.Marshal(value)
	if err != nil {
		return value
	}
	var copied T
	if err := json.Unmarshal(encoded, &copied); err != nil {
		return value
	}
	return copied
}

// WriteRecordStream は data を Range ヘッダの開始位置から 206 で書く。
// 開始位置が範囲外なら 416 を返す。Range が無い、またはパースできないときの開始位置は 0 とする。
// mirakc の録画中配信の挙動（追従など）は真似しない。
func WriteRecordStream(w http.ResponseWriter, r *http.Request, data []byte) {
	offset := RangeOffset(r)
	if offset < 0 || offset >= int64(len(data)) {
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return
	}
	body := data[offset:]
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusPartialContent)
	_, _ = w.Write(body)
}

// CallCount は method と path が一致する記録済みリクエストの数を返す。
func (f *Fake) CallCount(method, path string) int {
	count := 0
	for _, call := range f.Calls() {
		if call.Method == method && call.Path == path {
			count++
		}
	}
	return count
}

// RangeOffset は Range ヘッダ（bytes=N-）の開始位置を返す。ヘッダが無い、またはパースできないときは 0 とする。
func RangeOffset(r *http.Request) int64 {
	var offset int64
	if h := r.Header.Get("Range"); h != "" {
		_, _ = fmt.Sscanf(h, "bytes=%d-", &offset)
	}
	return offset
}
