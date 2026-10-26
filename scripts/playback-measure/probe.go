//go:build ignore

// Command probe measures append visibility across Kubernetes nodes. It never
// connects to mirakc or the application database. See README.md for usage.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"syscall"
	"time"
)

func emit(v any) {
	if err := json.NewEncoder(os.Stdout).Encode(v); err != nil {
		panic(err)
	}
}
func check(err error) {
	if err != nil {
		panic(err)
	}
}
func number(s string) int64 { n, e := strconv.ParseInt(s, 10, 64); check(e); return n }
func main() {
	if len(os.Args) < 2 {
		panic("expected probe mode")
	}
	switch os.Args[1] {
	case "clock":
		http.HandleFunc("/clock", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, time.Now().UnixNano()) })
		check(http.ListenAndServe(":18080", nil))
	case "calibrate":
		// offset is writer clock minus reader clock. RTT/2 bounds the uncertainty
		// if the two hosts' clocks do not step during the measurement.
		client := &http.Client{Timeout: 10 * time.Second}
		for i := 0; i < 30; i++ {
			a := time.Now().UnixNano()
			r, e := client.Get(os.Args[2] + "/clock")
			check(e)
			b, e := io.ReadAll(r.Body)
			check(e)
			check(r.Body.Close())
			z := time.Now().UnixNano()
			remote := number(string(b))
			emit(map[string]any{"kind": "clock", "at": z, "rtt_ns": z - a, "offset_ns": remote - (a+z)/2})
			time.Sleep(20 * time.Millisecond)
		}
	case "write":
		// start/end are half-second chunk indices into a completed TS fixture.
		data, e := os.ReadFile(os.Args[3])
		check(e)
		start, end := number(os.Args[4]), number(os.Args[5])
		total := number(os.Args[6])
		chunk := int64(len(data)) / 188 / total * 188
		f, e := os.OpenFile(os.Args[2], os.O_CREATE|os.O_RDWR|os.O_APPEND, 0666)
		check(e)
		check(syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB))
		s, e := f.Stat()
		check(e)
		if s.Size() != start*chunk {
			panic(fmt.Sprintf("size %d, expected %d", s.Size(), start*chunk))
		}
		emit(map[string]any{"kind": "open", "at": time.Now().UnixNano(), "start": start, "size": s.Size(), "chunk": chunk})
		epoch := time.Now()
		for i := start; i < end; i++ {
			a := time.Now().UnixNano()
			n, e := f.Write(data[i*chunk : (i+1)*chunk])
			check(e)
			if int64(n) != chunk {
				panic("short write")
			}
			emit(map[string]any{"kind": "write", "seq": i, "begin": a, "end": time.Now().UnixNano(), "size": (i + 1) * chunk})
			time.Sleep(time.Until(epoch.Add(time.Duration(i-start+1) * 500 * time.Millisecond)))
		}
		check(f.Sync())
		check(f.Close())
		emit(map[string]any{"kind": "close", "at": time.Now().UnixNano(), "end": end})
	case "read":
		path := os.Args[2]
		duration := time.Duration(number(os.Args[3])) * time.Second
		deadline := time.Now().Add(duration)
		var f *os.File
		var pos int64
		hash := sha256.New()
		buf := make([]byte, 1024*1024)
		for time.Now().Before(deadline) {
			a := time.Now().UnixNano()
			s, e := os.Stat(path)
			z := time.Now().UnixNano()
			if os.IsNotExist(e) {
				time.Sleep(20 * time.Millisecond)
				continue
			}
			check(e)
			if f == nil {
				f, e = os.Open(path)
				check(e)
			}
			fs, e := f.Stat()
			check(e)
			// ReadAt tests visibility independently from path stat and persistent fd stat.
			n, e := f.ReadAt(buf, pos)
			if e != nil && e != io.EOF {
				check(e)
			}
			if n > 0 {
				_, e = hash.Write(buf[:n])
				check(e)
				pos += int64(n)
			}
			emit(map[string]any{"kind": "read", "begin": a, "stat_end": z, "end": time.Now().UnixNano(), "stat": s.Size(), "fstat": fs.Size(), "read": pos, "n": n})
			time.Sleep(20 * time.Millisecond)
		}
		if f != nil {
			check(f.Close())
		}
		emit(map[string]any{"kind": "digest", "size": pos, "sha256": hex.EncodeToString(hash.Sum(nil))})
	default:
		panic("usage: probe clock | calibrate URL | write PATH SOURCE START END TOTAL | read PATH SECONDS")
	}
}
