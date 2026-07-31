package main

import (
	"testing"
)

// resolveRiverClientKind はロール指定が実際に実行する仕事を制約するための唯一の
// 分岐点。ここが誤ると、--roles watcher 単独のプロセスが worker.NewWorkers の
// フルのワーカー群（EncodeWorker/ThumbnailWorker を含む）を登録し、ffmpeg/ffprobe
// を検査しないまま encode/thumbnail ジョブを実行しうる（不変条件 4 違反、issue #113）。
func TestResolveRiverClientKind(t *testing.T) {
	tests := []struct {
		name  string
		roles []string
		want  riverClientKind
	}{
		{"watcher alone must not get the full worker set", []string{"watcher"}, riverClientInsertOnly},
		{"worker alone", []string{"worker"}, riverClientFull},
		{"worker and watcher together (monolith) keep the shared full client", []string{"worker", "watcher"}, riverClientFull},
		{"watcher and worker together regardless of order", []string{"watcher", "worker"}, riverClientFull},
		{"api alone needs no river client here (uses its own insert-only client)", []string{"api"}, riverClientNone},
		{"streamer alone", []string{"streamer"}, riverClientNone},
		{"notifier alone", []string{"notifier"}, riverClientNone},
		{"api and watcher: watcher still must not get the full worker set", []string{"api", "watcher"}, riverClientInsertOnly},
		{"all roles together", allRoles, riverClientFull},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveRiverClientKind(tt.roles); got != tt.want {
				t.Errorf("resolveRiverClientKind(%v) = %v, want %v", tt.roles, got, tt.want)
			}
		})
	}
}
