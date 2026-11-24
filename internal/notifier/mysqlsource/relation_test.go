package mysqlsource

import (
	"testing"

	"github.com/fetburner/rokuban/internal/store/mysqlstore"
)

// TestRetentionOutlastsLookback は、保持期間 > 遡る時間 > 間隔 の関係を検査する。
// 関係が崩れると、掃除が読み直しに必要な行を先に消すか、読み直しが間隔に追いつかなくなる。
// 壊すと（定数を入れ替える）このテストが落ちる。
func TestRetentionOutlastsLookback(t *testing.T) {
	if pollInterval >= lookback {
		t.Errorf("pollInterval = %s, lookback = %s; want pollInterval < lookback", pollInterval, lookback)
	}
	if lookback >= mysqlstore.NotificationRetention {
		t.Errorf("lookback = %s, NotificationRetention = %s; want lookback < NotificationRetention", lookback, mysqlstore.NotificationRetention)
	}
}
