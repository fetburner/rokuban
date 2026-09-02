-- ruler（M2-3）が使う集合演算クエリ。
-- 1 パスで全ルール x 全射影番組を評価し、差分だけを書く（docs/recording.md §3.1）。
-- 予約 1 件ずつのループにしないため、書き込みは Postgres の集合演算 1 文にまとめる。

-- name: ListEnabledRules :many
SELECT * FROM rules WHERE enabled = true ORDER BY priority DESC, id ASC;

-- ruler は skip 意図の除外にだけこのクエリを使う。record 側（「この番組に
-- ユーザーの投資があるか」の一部）は program_investments view に一本化した
-- ため ListProgramInvestmentProgramIDsBySite（下記）から引く（#162）。
-- name: ListProgramIntentActionsBySite :many
SELECT program_id, action FROM program_intents WHERE site = $1;

-- 「この番組にユーザーの投資があるか」（program_intents の action='record' 行 ∪
-- program_overrides の行）は program_investments view（#162）に一本化した。
-- ruler は record 意図の中身も overrides の中身も一切読まない（不透明な
-- ペイロード）ため programId だけを引く（docs/recording.md §4.2「ruler から
-- 見た load-bearing な行」: desired = (マッチ − skip) ∪ record ∪
-- {program_overrides に行がある番組}）。
-- name: ListProgramInvestmentProgramIDsBySite :many
SELECT program_id FROM program_investments WHERE site = $1;

-- name: ListReservationProgramIDsBySite :many
SELECT program_id FROM reservations WHERE site = $1;

-- 射影から program_snapshots への追従更新（#27）は
-- internal/db/queries/program_snapshots.sql の UpsertProgramSnapshotsFromProjection
-- 1 本にまとまった。ruler はそれを desiredIDs に対して呼ぶだけで、「射影にある間は
-- 更新、消えたら凍結」を自分で判定しない（射影に無い programId は
-- UpsertProgramSnapshotsFromProjection の JOIN にそもそも出てこないので、
-- 何もせず既存の program_snapshots 行がそのまま凍結される）。
-- 旧 ListProgramSnapshotsBySiteAndProgramIDs（epg_programs ⋈ epg_services を
-- 直接引いて reservations 側の CASE で凍結を判定していたもの）は撤去した。

-- name: ListProgramSnapshotProgramIDsBySiteAndProgramIDs :many
-- 新規に reservations 行を作れるかどうかの判定に使う。program_snapshots への
-- FK があるため、desired な programId のうち program_snapshots の行を持たない
-- ものは予約行を作れない（射影にもなく、既存の意図・上書き・予約からも
-- スナップショットが作られたことがない = 材料がどこにもない）。
SELECT program_id FROM program_snapshots
WHERE site = $1 AND program_id = ANY(sqlc.arg(program_ids)::bigint[]);

-- UpsertReservationsFromRulerPass は jsonb_to_recordset を使う集合演算 1 文で、
-- sqlc の組み込みアナライザ（実 DB 接続なしのカタログ解析）がこの動的レコード型を
-- 解決できないため（`column "program_id" does not exist` で generate が失敗する）、
-- rulequery パッケージの流儀に倣って internal/ruler/sql.go に生 SQL として置き、
-- pgxpool 経由で直接実行する。

-- name: DeleteReservationsBySiteAndProgramIDs :execrows
-- ルール・program_intents のどちらからも desired でなくなった予約のうち、
-- 「ルール x EPG」由来と区別できないものを削除する（導出削除。呼び出し側で
-- サーキットブレーカーの閾値判定を先に行うこと）。ユーザーが投資を手放す書き込みを
-- しない限り起きない削除は先に DeleteReleasedReservationsBySiteAndProgramIDs が
-- 処理済みで、ブレーカーの数にも入らない。
--
-- toDelete は runPassForSite の先頭（トランザクション外）で ListProgramIntentActionsBySite /
-- ListProgramInvestmentProgramIDsBySite / ListReservationProgramIDsBySite を読んでから
-- 計算した集合で、この DELETE 文自体は別のトランザクション（tx）内で後から実行される。
-- その間に api の PutProgramIntent（program_intents.action='record' をコミットする
-- だけで、reservations には一切触れない）が同じ program_id に意図を立てると、
-- toDelete は古い読み取りのままその番組を含んでしまい、意図が反映される直前だった
-- 既存の予約行を削除してしまう（読み順を入れ替えても「計算してから DELETE を実行する
-- までの窓」は必ず残るため、読み順の入れ替えでは直らない）。
--
-- そこでガードを読み取り側ではなく DELETE 文自体の WHERE 句に持たせ、削除の瞬間に
-- load-bearing な行の有無を再評価する（CLAUDE.md 不変条件 9「導出は読むたびに
-- 評価する」を列だけでなく DELETE の対象判定にも適用する）。
-- internal/db/queries/rules.sql の DeleteReservationsByRuleWithoutIntent と同じ形。
--
-- 「この番組にユーザーの投資があるか」という述語は program_investments view
-- （program_intents の action='record' 行 ∪ program_overrides の行）に一本化した
-- （#162）。view は実行時にインライン展開されるため、ガードが「削除の瞬間に
-- 再評価する」性質は変わらない。
DELETE FROM reservations r
WHERE r.site = $1 AND r.program_id = ANY(sqlc.arg(program_ids)::bigint[])
  AND NOT EXISTS (
      SELECT 1 FROM program_investments v
      WHERE v.site = r.site AND v.program_id = r.program_id
  );

-- name: DeleteReleasedReservationsBySiteAndProgramIDs :many
-- desired から外れた予約のうち、**ユーザー（運用者）が投資を手放す書き込みを
-- しない限り起きない**ものを削除し、実際に消した programId を返す。呼び出し側
-- （runPassForSite）はこれを大量削除サーキットブレーカーの対象にしない ---
-- ブレーカーが防ぐのは EPG の一時欠損による一斉削除（EPGStation#692。
-- docs/recording.md §3.2「大量削除サーキットブレーカー」）で、この集合は EPG が
-- 壊れただけでは増えないため。
--
-- 条件（上の DELETE と同じ NOT EXISTS program_investments に加えて、次のいずれか）:
--
--   r.rule_id IS NULL
--     いまルールが base を供給していない行。**この列は EPG の変化だけでも NULL に
--     なる**ので、これ単体では「ユーザー由来」の証明にならない: 投資（record 意図
--     ∪ overrides）を持つ行はルールが外れても desired に残るためそのパスで
--     upsert され、internal/ruler/sql.go の resolved CTE が凍結するのは base と
--     dedup 根拠 2 列だけで、rule_id = EXCLUDED.rule_id がそのまま NULL を書く
--     （TestRunPass_EpgUnmatchNullsRuleIDButInvestmentBlocksRelease が実測で固定）。
--     効いているのは NOT EXISTS program_investments のほうである ---
--     rule_id が NULL の行は、その NULL が書かれた時点で必ず投資を持っていた
--     （ruler の upsert が NULL を書くのは「勝者なしで desired」= 投資ありの行
--     だけ。rule_id を NULL にするもう 1 つの経路である rules の FK
--     ON DELETE SET NULL も、DeleteRule が同一 tx で投資なしの行を**先に**消す
--     ため、SET NULL を受けて生き残るのは投資ありの行だけ。internal/api/rules.go）。
--     したがってこの文の対象に落ちるには、投資を消す書き込みが別途必要になる。
--   program_intents.action = 'skip'
--     ユーザーが「録るな」と書いた。
--
-- 投資を消す（あるいは record を skip に倒す）書き込みの全数え上げ:
-- per-program の api 3 本（DELETE .../intent / DELETE .../overrides /
-- PUT .../intent{skip}。openapi.yaml のパスはどちらも {programId} を含み
-- バルクが無い）、運用者が明示的に走らせる catalog rescue の復元
-- （internal/catalog/rescue.go。program_intents を upsert するので record を
-- skip で上書きしうる。バルクだが EPG に駆動されない運用者の操作で、復元して
-- いるのは記録済みのユーザー意図そのもの）、program_snapshots の GC CASCADE
-- （このとき reservations 行も一緒に落ちるので toDelete に現れない）。
-- epg_sync / ruler / reconciler はこの 2 表に一切書かない。よって EPG が
-- 壊れてもこの文が消す件数は「人が書いた回数」で頭打ちになる。
--
-- 逆に rule_id が非 NULL で skip 意図も無い行は「ルールが base を供給しているのに
-- desired から外れた」= EPG 由来と区別できない削除であり、この文の対象外
-- （上の DeleteReservationsBySiteAndProgramIDs 側でブレーカーを通す）。
--
-- 境界: EPG 欠損中は投資を持つ行の rule_id が一斉に NULL に落ちるので、その最中に
-- ユーザーが投資を消すと、健全な EPG ならルール由来で残ったはずの予約がブレーカーの
-- 外で消える。EPG 復旧後の次パスでルールが作り直すので自己修復するが、
-- 「明示操作からしか説明できない」とまでは言えない（docs/recording.md §3.2 の
-- 境界 (c)）。
--
-- 分類そのものをこの WHERE に置いてあるのは #29 型の窓を作らないため。呼び出し側は
-- toDelete 全体をこの文に渡し、RETURNING で「実際にここで消えた集合」を受け取って、
-- その差集合をブレーカー対象の導出削除とする。分類がトランザクション外の古い
-- 読み取りで決まる余地がない（上の DELETE と同じ規律）。
DELETE FROM reservations r
WHERE r.site = $1 AND r.program_id = ANY(sqlc.arg(program_ids)::bigint[])
  AND NOT EXISTS (
      SELECT 1 FROM program_investments v
      WHERE v.site = r.site AND v.program_id = r.program_id
  )
  AND (
      r.rule_id IS NULL
      OR EXISTS (
          SELECT 1 FROM program_intents i
          WHERE i.site = r.site AND i.program_id = r.program_id AND i.action = 'skip'
      )
  )
RETURNING r.program_id;

-- name: ListEpgProgramIDsBySiteAndProgramIDs :many
-- 削除候補（desired から外れた既存予約）のうち、EPG プロジェクションに
-- まだ番組がある = ルールが「マッチしなくなった」と確信を持って判定できるものだけを
-- 絞り込む。射影から番組ごと消えている場合はここに出てこず、呼び出し側は削除せず
-- 凍結する（docs/schema.md「射影にある間は更新、消えたら凍結」を削除判定にも適用）。
SELECT program_id FROM epg_programs
WHERE site = $1 AND program_id = ANY(sqlc.arg(program_ids)::bigint[]);

-- サーキットブレーカー（M2-5）発動時に breaker.Sample へ詰める「何を消そうとしていたか」の
-- タイトルスナップショットを引く。programId だけでは手動確認する人間が判断できないため
-- （breaker.SampleProgram.Title のコメント参照）。呼び出し側が対象を
-- breaker.MaxSampleSize 程度に絞ってから呼ぶ想定なので、ここでは LIMIT を掛けない。
-- title は program_snapshots に移設された（#27）。削除候補の programId は
-- 呼び出し時点でまだ reservations 行を持つので、FK により program_snapshots の
-- 行も必ず存在する（reservations との JOIN は不要）。
-- name: ListReservationTitlesBySiteAndProgramIDs :many
SELECT program_id, title FROM program_snapshots
WHERE site = $1 AND program_id = ANY(sqlc.arg(program_ids)::bigint[]);

-- name: ListRetractGraceProtectedProgramIDsBySiteAndProgramIDs :many
-- 猶予（ruler.retract_grace, issue #428）で削除を見送る programId を、
-- derivedDeletes（toDelete から released --- ユーザーの明示操作でしか説明できない
-- 削除、intent skip / intent クリア / 最後の investment だった overrides の削除
-- --- を引いた後の、ブレーカー対象の削除候補）の中から絞り込む。denpa の
-- 「開始 N 時間前以降はルールから外れても引っ込めない。ただしルールごと削除・停止
-- されたぶんは直前でも引っ込める」に倣う（docs/recording/ruler.md §3.1）。
--
-- **released を引いた後に適用する。** toDelete 全体（released を含む）に適用す
-- ると、rule_id が前パスから NOT NULL のままユーザーが intent{skip} を立てた行
-- まで猶予が守ってしまい、「これは録らない」という明示操作が直前の猶予に呑まれ
-- て一生解放されない（released の DELETE 文はこの猶予より先に、この猶予とは無
-- 関係に実行済み）。derivedDeletes まで絞ってから猶予を掛けることで、猶予が保護
-- する対象はブレーカー対象の集合そのものになる（呼び出し側 internal/ruler/ruler.go
-- のコメント参照）。
--
-- r.rule_id は呼び出し時点でまだ「前パス」の値である --- derivedDeletes の
-- programId はどれも今パスの desired に無いため、internal/ruler/sql.go の
-- upsertReservationsFromPass の入力行（rows）にも
-- DeleteReleasedReservationsBySiteAndProgramIDs の対象にも含まれず、この SELECT
-- が呼ばれる時点ではまだ何にも書き換えられていない。**この列を「今パスの評価結
-- 果」で読んではならない**（今パスで NULL に落ちた後に見ても、既に unmatch した
-- 事実を見ているだけで前パスの勝者が分からない。罠の一つ）。
--
-- 条件:
--   r.rule_id IS NOT NULL
--     前パスでルールが base を供給していた行だけが対象。ルールを一度も勝ち取って
--     いない行（manual 予約が record 意図だけで存在するケース等）はそもそも
--     「ルールから外れた」に該当しない。
--   p.start_at >= sqlc.arg(now) AND p.start_at < sqlc.arg(grace_until)
--     開始時刻を過ぎた予約は対象にしない --- 過ぎた行は reconciler の allowlist と
--     GC の領分（ruler.md「開始遅延検出器」の `detectStartDelays` と同じ理由）。
--     grace_until（呼び出し側で now + retract_grace を計算）より先の予約も対象外:
--     「開始直前」だけを守る猶予であり、遠い予約はサーキットブレーカーが受け持つ。
--     上限・下限のどちらも同じ表（epg_programs）の start_at を見る --- 片方だけ
--     古い表のままにすると、繰り上げで既に開始した番組が「開始前」と誤判定
--     されうる（reconciler の allowlist と GC の領分に踏み込む）。
--   EXISTS (rules ru WHERE ru.id = r.rule_id AND ru.enabled)
--     **ルールの無効化は猶予の対象外**。denpa と同じく「ルールごと削除・停止された
--     ぶんは直前でも引っ込める」（人が押した結果だから）。「ルールの編集で条件を
--     狭めた」は EPG 由来の unmatch と区別できない（breaker.md が同じ整理）ので、
--     こちらは猶予の対象のまま --- 録り過ぎ側に倒す非対称。
--
-- program_investments の除外はここでは再確認しない: 呼び出し側が渡す candidates
-- （derivedDeletes）は toDelete（既に stillProjectedSubset を通した削除候補）から
-- released を引いた集合で、investment を持つ programId は desired に含まれ
-- toDelete に入らないため、この関数の入力にそもそも現れない。
--
-- program_snapshots ではなく epg_programs（射影の最新値）を join する。
-- program_snapshots は desired（= ルールが今もマッチしている）番組にしか追従
-- しないため、猶予が効いてほしい unmatch のパスでは前回マッチ時点の値のまま
-- 凍結されている。stillProjectedSubset（internal/ruler/ruler.go）は tx を開く
-- 前に pool 上で走る 1 回の SELECT でしかなく、この SELECT との間に epg_sync
-- が該当行を消す窓はもとからある --- 当たれば「猶予の対象外」（削除）に倒れる。
-- 旧実装は program_snapshots への FK が行の存在を保証していたため、この窓では
-- （sub-millisecond）気づかれないまま保護できていた。COALESCE で両方見る形は
-- 取らない --- program_snapshots は定義上 stale になり得るので、生きた射影を
-- 1 本で見る方が説明が素直。
SELECT r.program_id
FROM reservations r
JOIN epg_programs p ON p.site = r.site AND p.program_id = r.program_id
WHERE r.site = $1
  AND r.program_id = ANY(sqlc.arg(program_ids)::bigint[])
  AND r.rule_id IS NOT NULL
  AND p.start_at >= sqlc.arg(now)::timestamptz
  AND p.start_at <  sqlc.arg(grace_until)::timestamptz
  AND EXISTS (
      SELECT 1 FROM rules ru WHERE ru.id = r.rule_id AND ru.enabled
  );
