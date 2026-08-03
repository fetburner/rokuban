-- name: GetReservation :one
SELECT id, rule_id FROM reservations
WHERE id = $1;

-- base も返す（issue #104: PatchProgramOverrides が「既存 override + このパッチ +
-- ルールの base」をマージした実効値を検証するために必要）。既存の呼び出し元
-- （internal/watcher, internal/reconciler）は ID / RuleID しか見ていないので
-- 列追加の影響を受けない。
-- name: GetReservationBySiteAndProgramID :one
SELECT id, rule_id, base FROM reservations
WHERE site = $1 AND program_id = $2;

-- name: CreateManualReservation :one
-- テスト用の直接 INSERT ヘルパー。reservations の実運用上の唯一の書き手は
-- ruler の一括 INSERT（internal/ruler/sql.go）で、api はこの表に一切書かない
-- （M3-1、issue #29「導出器が作るキーを宛先にしない」の帰結）。
--
-- 番組の事実のスナップショット（title / 開始時刻 / 尺 / チャンネル識別）は
-- program_snapshots に抽出された（#27）。FK (site, program_id) REFERENCES
-- program_snapshots があるので、呼び出し側（テストの fixture）はこの
-- INSERT より先に program_snapshots の行を upsert しておくこと。
--
-- reservations.source は持たない（issue #26 で削除）。この予約が「手動」で
-- あることは、program_intents.action='record' の行がそのまま表す。
INSERT INTO reservations (site, program_id)
VALUES ($1, $2)
RETURNING *;

-- 予約とユーザー意図・上書き・番組スナップショットを 1 行に合わせて返す。
-- action は program_intents、overrides は program_overrides（M2-4 / 00010 で分離）
-- にあり、予約が存在しても意図・上書きのどちらかしかない（あるいはどちらも
-- 無い）ことがあるので両方 LEFT JOIN する。番組スナップショットは FK が
-- あるので必ず存在する（INNER JOIN）。
--
-- never_recorded は issue #98 で orphaned_at の代わりに導出する列（読むたびに
-- 評価。CLAUDE.md 不変条件 9）。「この予約に紐づく status='failed' の
-- recordings 行が存在するか」を都度 EXISTS で問う --- reconciler が番組終了時に
-- 作る never-scheduled 行（recording.never-scheduled）も、mirakc 由来の
-- 途中失敗（handleRecordingFailed が作る行）も区別せず「録れなかった」の
-- 表示に使ってよい（api.reservationState のコメント参照）。
-- name: GetReservationFull :one
SELECT sqlc.embed(r), sqlc.embed(s), i.action AS intent_action, o.overrides AS overrides,
       EXISTS (
           SELECT 1 FROM recordings rec
           WHERE rec.reservation_id = r.id AND rec.status = 'failed'
       ) AS never_recorded
FROM reservations r
JOIN program_snapshots s ON s.site = r.site AND s.program_id = r.program_id
LEFT JOIN program_intents i ON i.site = r.site AND i.program_id = r.program_id
LEFT JOIN program_overrides o ON o.site = r.site AND o.program_id = r.program_id
WHERE r.id = $1;

-- never_recorded は GetReservationFull と同じ導出（コメント参照）。
-- name: ListReservationsBySite :many
SELECT sqlc.embed(r), sqlc.embed(s), i.action AS intent_action, o.overrides AS overrides,
       EXISTS (
           SELECT 1 FROM recordings rec
           WHERE rec.reservation_id = r.id AND rec.status = 'failed'
       ) AS never_recorded
FROM reservations r
JOIN program_snapshots s ON s.site = r.site AND s.program_id = r.program_id
LEFT JOIN program_intents i ON i.site = r.site AND i.program_id = r.program_id
LEFT JOIN program_overrides o ON o.site = r.site AND o.program_id = r.program_id
WHERE r.site = $1
ORDER BY s.start_at;

-- 同期対象の「候補」を返すクエリ（issue #54）。
--
-- **除外条件は issue #98 で orphaned_at IS NULL から書き換えた。** 旧実装は
-- reconciler.markOrphaned が「番組終了後に schedule が観測されなかった」
-- という不可逆な観測を reservations.orphaned_at という列に直接書いており、
-- それを次パス以降の除外フィルタに使っていた。#98 の決定でこの観測は
-- recordings の試行行（status='failed' + quality_events に
-- recording.never-scheduled）に移設され、orphaned_at 列自体が無くなった
-- （00025）。
--
-- 除外条件を「その予約に既に never-scheduled の recordings 行がある」に
-- 置き換える。never-scheduled という特定の quality_events マーカーだけを
-- 見て、status='failed' の行全般では絞らないのが要点 ---
-- handleRecordingFailed（internal/watcher）が作る「録画開始後に mirakc が
-- 失敗を報告した」failed 行まで含めて除外すると、mirakc が schedule を
-- 消した後にレコンサイラが再作成を試みる既存の再試行経路（#98 とは無関係の
-- 挙動）を壊してしまう。never-scheduled は「番組終了かつ schedule 非観測」
-- でしか作られない一方向の事実なので、これだけを除外条件にすれば
-- 再試行経路には触れない。
--
-- 「番組が既に終了しているか」で直接絞らない（now() との比較にしない）
-- 理由: reconciler.programEnded が実際の POST/文言判定を毎パス評価し直す
-- ので、ここでの事前絞り込みは効率のためだけに存在してよい。だが
-- now() 比較を SQL に持ち込むと、固定時刻を使うテスト（過去に書かれた
-- fixture が「将来」のつもりで書いた日時が実行時点で過去になっている等。
-- cmd/rokuban/shadowdiff_test.go で実例あり）が経過時間に依存して壊れる。
-- recordings の存在という「reconciler 自身が過去に書いた事実」を条件にすれば
-- テストの実行時刻に依存しない（reconciler が実際に never-scheduled 行を
-- 作らない限り除外されない）。
--
-- 旧名 ListSyncableReservationsBySite は「もう絞ってある」と約束してしまって
-- いた。その約束を信じて shadow-diff（cmd/rokuban/shadowdiff.go）の書き手は
-- effective.skip の絞り込みを移植し忘れ、M2-6 の重複排除が base.skip=true を
-- 立てた予約を「EPGStation と一致（Both）」と誤報告する見逃しが M2 の出口
-- 基準の測定器に入り込んだ（issue #54）。「同期対象か」を最終的に決めるのは
-- effective.skip（base + overrides + program_intents.action の合成）であり、
-- この行だけでは絞り切れていない。絞り込みは呼び出し元が
-- db.EvaluateSyncCandidates（internal/db/sync.go）に通して行う。
--
-- 番組の開始時刻・尺（reconciler の開始遅延検出に使う）は program_snapshots に
-- 移設された（#27）ので JOIN する。FK があるので必ず存在する。
-- name: ListReservationsForSyncEvaluation :many
SELECT sqlc.embed(r), sqlc.embed(s), i.action AS intent_action, o.overrides AS overrides
FROM reservations r
JOIN program_snapshots s ON s.site = r.site AND s.program_id = r.program_id
LEFT JOIN program_intents i ON i.site = r.site AND i.program_id = r.program_id
LEFT JOIN program_overrides o ON o.site = r.site AND o.program_id = r.program_id
WHERE r.site = $1
  AND NOT EXISTS (
      SELECT 1 FROM recordings rec
      WHERE rec.reservation_id = r.id
        AND rec.status = 'failed'
        AND EXISTS (
            SELECT 1 FROM jsonb_array_elements(rec.quality_events) qe
            WHERE qe->>'event' = 'recording.never-scheduled'
        )
  )
ORDER BY s.start_at;

-- 番組終了後の GC は DeleteEndedProgramSnapshots（internal/db/queries/program_snapshots.sql）
-- 1 本に集約された（#27）。reservations は program_snapshots への FK が
-- ON DELETE CASCADE なので、program_snapshots 側の行が消えれば一緒に落ちる
-- （orphaned かどうかを問わない。recordings.reservation_id は ON DELETE SET NULL
-- なので、録画履歴（recordings/media_assets）はこの削除の影響を受けない）。
-- 個別の DeleteEndedReservations は撤去した。
