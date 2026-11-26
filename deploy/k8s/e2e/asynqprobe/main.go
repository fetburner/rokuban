// asynqprobe は受け入れ判定ハーネスが asynq 構成で「滞留」を作るための投入器。
//
// 製品の投入経路（`rokuban enqueue`）は合流（Unique）で 2 件目以降を黙って消すので、
// 「滞留を件数ぶん作る」には使えない。River 版の insert_probe_job は river_job へ直接
// INSERT するが、asynq では Redis の内部キー（asynq:{...}）を書くことになり、不変条件 14
// で禁じられている。そこで、rokuban の契約（internal/jobq）と asynq の adapter
// （internal/jobq/asynqadapter）を通して投入する。asynq を直接 import しない。
//
// probe の kind は `e2e_probe`。Coalesce を false にするので、同じ内容でも件数ぶんの
// タスクになる。製品の worker には登録されていないので、掴んだ Job は未登録 kind として
// 捨てる（River 版の e2e_probe と同じ意味。cmd/rokuban の job_unhandled）。
//
// 使い方（kube.sh の asynq_insert_probe が port-forward の先に対して呼ぶ）:
//
//	go run ./deploy/k8s/e2e/asynqprobe -redis redis://127.0.0.1:16379/0 -queue ingest_sitea -count 20
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"time"

	"github.com/fetburner/rokuban/internal/jobq"
	"github.com/fetburner/rokuban/internal/jobq/asynqadapter"
)

// probeArgs は滞留を作るための Args。Physical が投入先の物理キュー名である。
type probeArgs struct {
	Physical string `json:"physical"`
	Seq      int    `json:"seq"`
}

func (probeArgs) Kind() string { return "e2e_probe" }

// Spec は論理キュー名と実行方針を返す。Queue は QueueFunc が使わないので、論理名として
// 固定の値を置く。Coalesce は false（合流させない）。
func (probeArgs) Spec() jobq.Spec {
	return jobq.Spec{
		Queue:       "e2e_probe",
		Timeout:     func(jobq.Args) time.Duration { return time.Minute },
		MaxAttempts: 1,
		Coalesce:    false,
		Priority:    1,
	}
}

// probeWorker は登録のためだけに置く。probe は投入するだけで、この Work は呼ばれない。
type probeWorker struct{}

func (probeWorker) Work(context.Context, probeArgs) error { return jobq.ErrDiscard }

// queueFor は投入先の物理キュー名を返す。ZeroArgs（Physical が空）の検査は仮の名前で通す。
func queueFor(args jobq.Args) (string, error) {
	p, ok := args.(probeArgs)
	if !ok {
		return "", errors.New("asynqprobe: unexpected args type")
	}
	if p.Physical == "" {
		return "e2e_probe", nil
	}
	return p.Physical, nil
}

func main() {
	redisURL := flag.String("redis", "", "redis:// の URL（port-forward 先）")
	queue := flag.String("queue", "", "投入先の物理キュー名（例: ingest_sitea）")
	count := flag.Int("count", 1, "投入する件数")
	flag.Parse()
	if *redisURL == "" || *queue == "" || *count < 1 {
		log.Fatal("-redis, -queue and a positive -count are required")
	}

	registry := jobq.NewRegistry()
	jobq.Register[probeArgs](registry, probeWorker{})
	adapter, closeClient, err := asynqadapter.NewFromRedisURL(*redisURL, registry, queueFor, asynqadapter.Config{})
	if err != nil {
		log.Fatalf("building the asynq adapter: %v", err)
	}
	defer func() { _ = closeClient() }()

	ctx := context.Background()
	for i := 1; i <= *count; i++ {
		if _, err := adapter.Enqueue(ctx, probeArgs{Physical: *queue, Seq: i}); err != nil {
			log.Fatalf("enqueue %d/%d into %s: %v", i, *count, *queue, err)
		}
	}
	fmt.Printf("enqueued %d e2e_probe task(s) into %s\n", *count, *queue)
}
