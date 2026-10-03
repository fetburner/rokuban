package mirakc

import (
	"errors"
	"net/http"
	"time"
)

// MaxConsecutiveRetries は record の追従（worker の ingest 転送と streamer の追っかけ入力）が、
// 一過性の失敗を連続して再試行する上限。これを超えた失敗は呼び出し側がエラーとして返す。
const MaxConsecutiveRetries = 5

// retryBaseDelay / retryMaxDelay は RetryDelay の指数バックオフの下限・上限。mirakc が
// 即座に refuse する状況（再起動直後など）で再試行を一瞬で使い切らないようにする。
const (
	retryBaseDelay = 200 * time.Millisecond
	retryMaxDelay  = 5 * time.Second
)

// IsRetryable は mirakc への要求の失敗が一過性かを返す。5xx と HTTP 応答の無い失敗
// （通信断・本文の途中切れ）は再試行する。それ以外の HTTP エラー（4xx や、Range を無視した
// 200 を表す APIError）は再試行しても変わらない。
func IsRetryable(err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode >= http.StatusInternalServerError
	}
	return true
}

// RetryDelay は attempt 回目（0 始まり）の再試行の前に待つ時間を返す。200ms から倍々に
// 伸び、5 秒で頭打ちになる。MaxConsecutiveRetries 回までなら最長は 3.2 秒である。
func RetryDelay(attempt int) time.Duration {
	delay := retryBaseDelay << attempt
	if delay > retryMaxDelay || delay <= 0 {
		return retryMaxDelay
	}
	return delay
}
