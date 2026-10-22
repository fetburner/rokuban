package db

// 予約状態のラベル。Phase 1 以降、`reservations.state` という列は存在しない。
// active/detached は (rule_id, base) から導出する。この定数はテスト・ログでの
// 表記を揃えるためだけに残す。
const (
	ReservationStateActive   = "active"
	ReservationStateDetached = "detached"
)
