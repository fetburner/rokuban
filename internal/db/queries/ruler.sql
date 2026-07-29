-- ruler（M2-3）が使う集合演算クエリ。
-- 1 パスで全ルール x 全射影番組を評価し、差分だけを書く（docs/recording.md §3.1）。
-- 予約 1 件ずつのループにしないため、書き込みは Postgres の集合演算 1 文にまとめる。

-- name: ListEnabledRules :many
SELECT * FROM rules WHERE enabled = true ORDER BY priority DESC, id ASC;

-- name: ListProgramIntentActionsBySite :many
SELECT program_id, action FROM program_intents WHERE site = $1;

-- program_overrides に行があるだけで予約を存在させる（docs/recording.md §4.2
-- 「ruler から見た load-bearing な行」: desired = (マッチ − skip) ∪ record ∪
-- {program_overrides に行がある番組}）。ruler は overrides の中身を一切読まない
-- （不透明なペイロード）ため programId だけを引く。
-- name: ListProgramOverrideProgramIDsBySite :many
SELECT program_id FROM program_overrides WHERE site = $1;

-- name: ListReservationProgramIDsBySite :many
SELECT program_id FROM reservations WHERE site = $1;

-- name: ListProgramSnapshotsBySiteAndProgramIDs :many
-- 射影（epg_programs ⋈ epg_services）にまだある desired 番組のスナップショットを
-- 一括取得する。ここに出てこない programId は「射影から消えた」= 凍結対象。
SELECT p.program_id, p.name AS title, p.start_at, p.duration_ms,
       s.network_id, s.service_id, s.channel_type, s.channel
FROM epg_programs p
JOIN epg_services s
  ON s.site = p.site AND s.network_id = p.network_id AND s.service_id = p.service_id
WHERE p.site = $1 AND p.program_id = ANY(sqlc.arg(program_ids)::bigint[]);

-- UpsertReservationsFromRulerPass と InsertReservationRuleMatches は
-- jsonb_to_recordset / unnest を使う集合演算 1 文で、sqlc の組み込みアナライザ
-- （実 DB 接続なしのカタログ解析）がこれらの動的レコード型を解決できないため
-- （`column "program_id" does not exist` / `function unnest(unknown, unknown)
-- does not exist` で generate が失敗する）、rulequery パッケージの流儀に倣って
-- internal/ruler/sql.go に生 SQL として置き、pgxpool 経由で直接実行する。

-- name: DeleteReservationsBySiteAndProgramIDs :execrows
-- ルール・program_intents のどちらからも desired でなくなった予約を削除する
-- （導出削除。呼び出し側でサーキットブレーカーの閾値判定を先に行うこと）。
--
-- toDelete は runPassForSite の先頭（トランザクション外）で ListProgramIntentActionsBySite /
-- ListProgramOverrideProgramIDsBySite / ListReservationProgramIDsBySite を読んでから
-- 計算した集合で、この DELETE 文自体は別のトランザクション（tx）内で後から実行される。
-- その間に api の CreateReservation（program_intents{record} と reservations 行を
-- 1 tx でコミットする）が同じ program_id に対して割り込むと、toDelete は古い読み取りの
-- ままその番組を含んでしまい、作られたばかりの手動予約を削除してしまう
-- （読み順を入れ替えても「計算してから DELETE を実行するまでの窓」は必ず残るため、
-- 読み順の入れ替えでは直らない）。
--
-- そこでガードを読み取り側ではなく DELETE 文自体の WHERE 句に持たせ、削除の瞬間に
-- load-bearing な行の有無を再評価する（CLAUDE.md 不変条件 9「導出は読むたびに
-- 評価する」を列だけでなく DELETE の対象判定にも適用する）。
-- internal/db/queries/rules.sql の DeleteReservationsByRuleWithoutIntent と同じ形。
--
-- program_intents 側は action = 'record' に限定する。action を問わずに EXISTS すると
-- action='skip' の予約行（そもそも desired に入らない設計 = issue #18 の案 A）が
-- ここで保護されてしまい、「取消した予約が消えない」リグレッションになる。
-- program_overrides 側は中身を問わず行の存在だけで desired に残る設計
-- （ruler.go の union の最後）なので、action と無関係に EXISTS だけでよい。
DELETE FROM reservations r
WHERE r.site = $1 AND r.program_id = ANY(sqlc.arg(program_ids)::bigint[])
  AND NOT EXISTS (
      SELECT 1 FROM program_intents i
      WHERE i.site = r.site AND i.program_id = r.program_id AND i.action = 'record'
  )
  AND NOT EXISTS (
      SELECT 1 FROM program_overrides o
      WHERE o.site = r.site AND o.program_id = r.program_id
  );

-- name: ListReservationIDsBySiteAndProgramIDs :many
SELECT id, program_id FROM reservations
WHERE site = $1 AND program_id = ANY(sqlc.arg(program_ids)::bigint[]);

-- name: ListEpgProgramIDsBySiteAndProgramIDs :many
-- 削除候補（desired から外れた既存予約）のうち、EPG プロジェクションに
-- まだ番組がある = ルールが「マッチしなくなった」と確信を持って判定できるものだけを
-- 絞り込む。射影から番組ごと消えている場合はここに出てこず、呼び出し側は削除せず
-- 凍結する（docs/schema.md「射影にある間は更新、消えたら凍結」を削除判定にも適用）。
SELECT program_id FROM epg_programs
WHERE site = $1 AND program_id = ANY(sqlc.arg(program_ids)::bigint[]);

-- name: DeleteReservationRuleMatchesBySite :exec
-- reservation_rule_matches はサイト内の予約に紐づく行を毎パス全消しして入れ直す
-- （insertReservationRuleMatchesSQL のコメント参照。この表には SSE 用の行トリガーが
-- ないので差分書き込みは要求されない）。
--
-- 対象を「今回マッチした programId」に絞ると、ルールを削除ではなく無効化した
-- （ListEnabledRules から外れる）、あるいはルールの条件を変えてマッチしなくなったが
-- intent/overrides のおかげで予約行自体は生き残っている、という経路で古いマッチ行が
-- 掃除されずに残り続ける（導出表が毎パス作り直されない = CLAUDE.md 不変条件 9 違反。
-- ルール自体の削除は reservation_rule_matches.rule_id の FK CASCADE で救われるので
-- ここでは対象外でよい）。サイト単位の予約に紐づく行を無条件で全消しすることで、
-- 「今回マッチしなかった」を含めて正しく反映する。
DELETE FROM reservation_rule_matches
WHERE reservation_id IN (SELECT id FROM reservations WHERE site = $1);

-- サーキットブレーカー（M2-5）発動時に breaker.Sample へ詰める「何を消そうとしていたか」の
-- タイトルスナップショットを引く。programId だけでは手動確認する人間が判断できないため
-- （breaker.SampleProgram.Title のコメント参照）。呼び出し側が対象を
-- breaker.MaxSampleSize 程度に絞ってから呼ぶ想定なので、ここでは LIMIT を掛けない。
-- name: ListReservationTitlesBySiteAndProgramIDs :many
SELECT program_id, title FROM reservations
WHERE site = $1 AND program_id = ANY(sqlc.arg(program_ids)::bigint[]);
