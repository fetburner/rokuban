package riveradapter

import (
	"slices"
	"testing"

	"github.com/fetburner/rokuban/internal/jobs"
)

// jobs.AllQueueNames() が allQueues() と同じキュー集合を主張していること。
//
// **キュー集合の権威は本来 1 つであるべきだが、jobs.AllQueueNames() は
// internal/jobs 側の手書きリテラルで、allQueues() は riveradapter 側の
// river.QueueConfig map。jobs から riveradapter を import すると循環するため、
// 権威を 1 つに統合できない。** この二重管理を許す唯一の代償として、
// 両者が一致することをここで固定する。
//
// このテストが検出すべき変異: allQueues() だけにキューを足す（jobs 側は
// unknown のまま worker.queues が弾く）、または jobs.AllQueueNames() だけに
// 足す（allQueues() に無いので worker が購読できず、k8s の
// TestScaledJobsCoverEveryQueue の網にも入らない）。
func TestAllQueueNames_MatchesAllQueues(t *testing.T) {
	got := sortedQueueNames(allQueues(0, 0, 0))
	want := jobs.AllQueueNames()
	if !slices.Equal(got, want) {
		t.Errorf("sortedQueueNames(allQueues(0,0,0)) = %v, want jobs.AllQueueNames() = %v", got, want)
	}
}
