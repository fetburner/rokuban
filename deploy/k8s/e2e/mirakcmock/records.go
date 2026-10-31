package main

// 録画の注入と配信。判定 6.8（取り込みを通す）のためだけにある。
//
// 実 mirakc では録画は予約された放送が流れたときに mirakc が作る。このモックは
// 放送を流さないので、**ハーネスが `POST /mock/records` で「終わった録画」を 1 件
// 置く**。そこから先は製品の経路がそのまま動く:
//
//	POST /mock/records → SSE `recording.record-saved`（mirakc の実物と同じ型）
//	  → 製品の watcher（GetRecord → ProcessRecord）→ ingest ジョブ
//	  → 製品の ingest（HEAD / GET Range / DELETE）
//
// 製品の ingest が読むのは `GET /api/recording/records{,/id}`、`HEAD` と `GET`
// の `…/{id}/stream`（Range 付き 206 / 416）、`DELETE …/{id}?purge=true`。
// ここにあるのはそれだけで、ほかの経路は足さない（mock の doc コメントの方針）。
//
// 注入した録画は programId の番組を名乗り、rokuban のタグ（mirakc.ProgramTag）を
// 付ける。タグが無いと製品は「自分が予約した録画ではない」として録画行を作らない
// （internal/store/pgstore の ProcessRecord が Managed で分岐する）。

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fetburner/rokuban/internal/mirakc"
)

// injectedRecordBytesLimit は POST /mock/records が受ける本文の上限。
// 判定 6.8 の TS は数 MB（20 秒の 800kbps 程度）。上限は取り違えた大きい入力を弾くため。
const injectedRecordBytesLimit = 64 << 20

// injectedRecordDurationMs は注入した録画の長さ（ミリ秒）。中身の TS の長さとは独立に
// 製品へ渡す値なので、判定が見るのはバイト数と SHA-256 であって、この長さではない。
const injectedRecordDurationMs = 60_000

// recordSavedEvent は mirakc の SSE の録画保存イベントの型名。
const recordSavedEvent = "recording.record-saved"

type injectedRecord struct {
	record mirakc.Record
	body   []byte
}

// recordStore は注入された録画と、/events の購読者（SSE の送信口）を持つ。
type recordStore struct {
	mu      sync.Mutex
	seq     int
	records map[string]injectedRecord

	subsMu sync.Mutex
	subs   map[chan string]struct{}
}

func newRecordStore() *recordStore {
	return &recordStore{records: map[string]injectedRecord{}, subs: map[chan string]struct{}{}}
}

// clear は注入した録画を全部消す（POST /mock/reset と同じ周回の掃除）。
// 購読者は残す（SSE の接続は reset で切らない）。
func (s *recordStore) clear() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.records)
	s.records = map[string]injectedRecord{}
	return n
}

func (s *recordStore) list() []mirakc.Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]mirakc.Record, 0, len(s.records))
	for _, r := range s.records {
		out = append(out, r.record)
	}
	return out
}

func (s *recordStore) get(id string) (injectedRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.records[id]
	return r, ok
}

// take は録画を取り除いて返す（DELETE 用）。
func (s *recordStore) take(id string) (injectedRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.records[id]
	if ok {
		delete(s.records, id)
	}
	return r, ok
}

func (s *recordStore) subscribe() chan string {
	ch := make(chan string, 16)
	s.subsMu.Lock()
	s.subs[ch] = struct{}{}
	s.subsMu.Unlock()
	return ch
}

func (s *recordStore) unsubscribe(ch chan string) {
	s.subsMu.Lock()
	delete(s.subs, ch)
	s.subsMu.Unlock()
}

// broadcast は全購読者へ SSE の 1 イベントを送る。送信口が詰まっている購読者には
// 送らない（録画の保存は watcher の sweep が取りこぼしを拾う。ここで止めると
// POST が詰まる）。送れなかった数を返す。
func (s *recordStore) broadcast(eventType string, data []byte) int {
	frame := fmt.Sprintf("event: %s\ndata: %s\n\n", eventType, data)
	s.subsMu.Lock()
	defer s.subsMu.Unlock()
	dropped := 0
	for ch := range s.subs {
		select {
		case ch <- frame:
		default:
			dropped++
		}
	}
	return dropped
}

// postRecord は programId の番組の録画を 1 件注入する。本文が録画の中身（TS）。
//
// 応答は 201 と製品が読む録画の JSON。録画は status finished（終わった録画）で、
// 保存イベントを SSE に流す。
func (m *mock) postRecord(w http.ResponseWriter, r *http.Request) {
	programID, err := strconv.ParseInt(r.URL.Query().Get("programId"), 10, 64)
	if err != nil || programID <= 0 {
		http.Error(w, "bad programId", http.StatusBadRequest)
		return
	}
	now := m.now()
	program, ok := m.programByID(programID, now)
	if !ok {
		http.Error(w, "program not found", http.StatusNotFound)
		return
	}
	var service *mirakc.Service
	for _, s := range m.services() {
		if s.NetworkID == program.NetworkID && s.ServiceID == program.ServiceID {
			s := s
			service = &s
			break
		}
	}
	if service == nil {
		http.Error(w, "service of the program not found", http.StatusNotFound)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, injectedRecordBytesLimit))
	if err != nil {
		http.Error(w, "reading body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(body) == 0 {
		http.Error(w, "empty record body", http.StatusBadRequest)
		return
	}

	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	length := uint64(len(body))
	duration := int64(injectedRecordDurationMs)
	end := mirakc.Milliseconds(now)
	start := mirakc.Milliseconds(now.Add(-time.Duration(injectedRecordDurationMs) * time.Millisecond))

	m.records.mu.Lock()
	m.records.seq++
	id := fmt.Sprintf("mock-rec-%d", m.records.seq)
	rec := mirakc.Record{
		ID:      id,
		Program: program,
		Service: *service,
		Tags:    []string{mirakc.ProgramTag(programID)},
		Recording: mirakc.RecordInfo{
			Options:   mirakc.Options{},
			Status:    "finished",
			StartTime: start,
			EndTime:   &end,
			Duration:  &duration,
		},
		Content: mirakc.ContentInfo{
			Path:   "/mock/records/" + id + ".m2ts",
			Type:   "video/mp2t",
			Length: &length,
			Sha256: &digest,
		},
	}
	m.records.records[id] = injectedRecord{record: rec, body: body}
	m.records.mu.Unlock()

	payload, err := json.Marshal(mirakc.RecordSavedData{RecordID: id, RecordingStatus: "finished"})
	if err != nil {
		http.Error(w, "encoding event: "+err.Error(), http.StatusInternalServerError)
		return
	}
	dropped := m.records.broadcast(recordSavedEvent, payload)
	slog.Info("record injected", "recordId", id, "programId", programID, "bytes", len(body), "sha256", digest, "sse_dropped", dropped)
	writeJSON(w, http.StatusCreated, rec)
}

// getRecord は GET /api/recording/records/{id}。
func (m *mock) getRecord(w http.ResponseWriter, r *http.Request) {
	rec, ok := m.records.get(r.PathValue("id"))
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, rec.record)
}

// streamRecord は GET（と HEAD）/api/recording/records/{id}/stream。
//
// **Range は製品が送る形（`bytes=N-`）だけ受ける。** 応答は実 mirakc と同じく、
// Range があれば 206 + `Content-Range: bytes N-(総数-1)/総数`、N が総数以上なら 416。
// Range が無ければ 200 で全体。製品の StreamRecord は 206 を期待し、200 は offset 0 のときだけ
// 受理する（internal/mirakc/client.go）。
func (m *mock) streamRecord(w http.ResponseWriter, r *http.Request) {
	rec, ok := m.records.get(r.PathValue("id"))
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	total := int64(len(rec.body))
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Type", "video/mp2t")

	start, ranged, err := parseOpenRange(r.Header.Get("Range"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !ranged {
		w.Header().Set("Content-Length", strconv.FormatInt(total, 10))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(rec.body)
		return
	}
	if start >= total {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", total))
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return
	}
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, total-1, total))
	w.Header().Set("Content-Length", strconv.FormatInt(total-start, 10))
	w.WriteHeader(http.StatusPartialContent)
	_, _ = w.Write(rec.body[start:])
}

// parseOpenRange は `Range: bytes=N-` の N を返す。ヘッダが空なら ranged=false。
// 形が違うもの（複数範囲・末尾指定・接頭辞違い）はエラーにする。黙って全体を返すと、
// 製品の Range の扱いの誤りが緑になる。
func parseOpenRange(h string) (start int64, ranged bool, err error) {
	if h == "" {
		return 0, false, nil
	}
	spec, ok := strings.CutPrefix(h, "bytes=")
	if !ok || !strings.HasSuffix(spec, "-") {
		return 0, false, fmt.Errorf("unsupported Range %q (the mock accepts bytes=N- only)", h)
	}
	n, err := strconv.ParseInt(strings.TrimSuffix(spec, "-"), 10, 64)
	if err != nil || n < 0 {
		return 0, false, fmt.Errorf("unsupported Range %q", h)
	}
	return n, true, nil
}

// deleteRecord は DELETE /api/recording/records/{id}（?purge=true を含む）。
// 製品の ingest が取り込み後に呼ぶ。実 mirakc と同じく録画と内容を消して結果を返す。
func (m *mock) deleteRecord(w http.ResponseWriter, r *http.Request) {
	if _, ok := m.records.take(r.PathValue("id")); !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, mirakc.RecordRemovalResult{RecordRemoved: true, ContentRemoved: true})
}
