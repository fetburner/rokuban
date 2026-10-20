-- encode の desired−observed 定期 reconcile（internal/worker/encode_reconcile.go）と
-- api の待機列（internal/api/encode_queue.go）が読むクエリ。record_sweep（watcher の
-- 定期全量突き合わせ）とは対象集合が違うので、クエリを共有せず新規に切る ---
-- record_sweep が見るのは mirakc のエッジに残っている record（DB の外の観測）で、
-- こちらは「原本をコミット済みなのに desired なプロファイルの encoded が揃っていない
-- 録画」（DB だけで閉じる）。
--
-- # 共有する述語
--
-- 述語はスキーマ側の名前に置き、ここには直書きしない:
--
--   * 不足: view `unencoded_desired_profiles`（凍結済みの desired のうち active な
--     encoded が無い (recording_id, profile)。設定では絞らない）
--   * 録画単位: view `encodable_recordings`（ごみ箱でない・原本が active）
--   * プロファイル単位: 関数 `encodable_profile(rec_id, profile, known_profiles,
--     cut_profiles)`（空でない・現在の設定にある・cut なら確認済み）
--   * 試行上限: 関数 `encode_attempts_exhausted(rec_id, profile, attempt_limit)`
--     （failed かつ domain attempt 数が上限以上）
--   * 作り直し: view `unsatisfied_encode_rebuilds`（要求行のうち、押した時点の keep と
--     一致する凍結区間の active な encoded がまだ無いもの。ごみ箱・原本が active でない
--     録画を含まない）。cut の確認済み条件は掛けない
--
-- 不足の補完（ListMissingEncodeProfiles）と待機列（ListEncodeQueue）は同じ述語を
-- 持たなければならない。片方にだけ条件を書き足すと、待機列に出るのに投入されない
-- （またはその逆の）録画ができる。条件を足すときは上の名前の定義を変える。両経路が
-- 同じ集合に合意することは、設定から引数を組み立てる api の配線も含めて
-- internal/api の TestEncodeReconcileAndQueueAgreeOnPendingSet が固定する。
--
-- # until_encoded_deletable_originals との関係
--
-- 削除側の view `until_encoded_deletable_originals` も、「desired が揃っている」を
-- unencoded_desired_profiles に行が無いことで判定する。設定での絞り（known_profiles）
-- は encodable_profile にだけ置き、unencoded_desired_profiles には足さない。足すと、
-- 設定からプロファイルを消す編集だけで、それを凍結した until_encoded 録画の原本が
-- 不可逆に消える（docs/storage/retention.md §保持ポリシー。internal/worker の
-- TestUnknownDesiredProfile_PredicateAsymmetry が両側から固定する）。

-- ListMissingEncodeProfiles は「エンコードしてよい録画（encodable_recordings）の
-- 不足（unencoded_desired_profiles）のうち、エンコードしてよいプロファイル
-- （encodable_profile）で、満たされていない作り直し（unsatisfied_encode_rebuilds）が
-- 無く、試行上限に達していない」(recording_id, profile) を返す。作り直しは固定 keep
-- を持つ別経路（ListEncodedAssetRebuildRequests）で投入する。
-- recording_id が after_recording_id より大きい録画を recording_id 昇順で
-- row_limit 件選んでから profile を展開するので、row_limit は profile 行数ではなく
-- 録画件数の上限になる。候補の判定は LIMIT 前に行い、完全な録画を先に消費しない。
--
-- 条件の意味:
--   * recording_encode_policy に行がある = エンコードポリシーが凍結済み
--     （行が無い = 未凍結。不変条件 10。unencoded_desired_profiles に行が出ない）。
--     desired が空なら不足の行も無いので候補にならない
--   * encodable_recordings = ごみ箱でなく、原本 media_assets が active（ingest
--     コミット済みで、until_encoded で物理削除されていない）。対象録画を絞った
--     即時パスもこのクエリを使うため、全件パスとごみ箱の扱いがずれない
--   * encodable_profile = 現在の設定に存在する非空のプロファイルで、cut なら
--     recording_chapter_ownership の行がある（不変条件 10: 行の存在 = ユーザーが
--     確認済み）。設定から消えたプロファイルを候補に含めると、投入しても
--     EncodeWorker が `unknown encode profile` で弾く（encode.go）録画が窓を
--     恒久的に占有し続ける。cut を確認前に投入すると、誤検出のまま本編が削られ、
--     原本がごみ箱を経由せずに消えて取り返せなくなる。確認 UI は cut でない
--     プロファイルの encoded で再生するので、原本 TS をブラウザで再生できないという
--     循環もここで断つ
--   * recording_id > after_recording_id = 継続ジョブの引数に保存したページ位置。
--     LIMIT まで埋まったページは最後の id を次のジョブに渡し、`--once` が worker を
--     作り直しても続きから見る。短いページでは継続ジョブを投入せず、次の定期パスが
--     0（先頭）から始まる（EncodeReconcileWorker の doc コメント「候補窓の継続」）。
--   * recording_id が NULL でなければ、その録画だけに候補を絞る（ingest 完了後や
--     encode_profiles の事後追加で投入する即時パス）。対象パスは after_recording_id
--     を 0 にし、全件パスの継続カーソルとは独立させる
--
-- **known_profiles は non-NULL でなければならない**（encodable_profile の注意。
-- NULL を渡すと候補が全滅する）。しかも下の ListUnsatisfiableEncodeProfiles の
-- `NOT (... = ANY(...))` も同時に NULL になるため、**空振りしていることすら
-- 見えなくなる**（バックストップが無症状で死ぬ）。呼び出し側が渡すのは
-- config.EncodeConfig.ProfileNames() で、空設定でも non-nil を返す契約
-- （internal/config の TestEncodeConfig_ProfileNames_EmptyIsNonNil、
-- internal/worker の TestEncodeReconcileWorker_EmptyProfileConfigIsVisibleNotSilent
-- が両側から固定している）。
--
-- name: ListMissingEncodeProfiles :many
WITH missing AS (
  SELECT u.recording_id, u.profile, u.ordinality
  FROM unencoded_desired_profiles u
  JOIN encodable_recordings er ON er.recording_id = u.recording_id
  WHERE u.recording_id > sqlc.arg('after_recording_id')::bigint
    AND (sqlc.narg('recording_id')::bigint IS NULL OR u.recording_id = sqlc.narg('recording_id')::bigint)
    AND encodable_profile(u.recording_id, u.profile, sqlc.arg('known_profiles')::text[], sqlc.arg('cut_profiles')::text[])
    AND NOT EXISTS (
      SELECT 1 FROM unsatisfied_encode_rebuilds rebuild
      WHERE rebuild.recording_id = u.recording_id
        AND rebuild.profile = u.profile
    )
    AND NOT encode_attempts_exhausted(
      u.recording_id, u.profile, sqlc.arg('attempt_limit')::integer
    )
), candidate_recordings AS (
  SELECT DISTINCT recording_id
  FROM missing
  ORDER BY recording_id
  LIMIT sqlc.arg('row_limit')
)
SELECT m.recording_id, m.profile
FROM missing m
JOIN candidate_recordings c ON c.recording_id = m.recording_id
GROUP BY m.recording_id, m.profile
ORDER BY m.recording_id, min(m.ordinality);

-- ListEncodeQueue は encode 待機列を (recording_id, profile) 単位で返す。候補の述語は
-- ListMissingEncodeProfiles と同じ unencoded_desired_profiles / encodable_recordings /
-- encodable_profile / unsatisfied_encode_rebuilds / encode_attempts_exhausted を使う
-- （ファイル冒頭「共有する述語」）。満たされていない作り直しは、active な encoded が
-- 残っていても待機列に入る（active な asset の存在だけでは、押された keep で作られた
-- ことを意味しない）。満たされた要求は入らない。row_limit や after_recording_id は適用しない。
--
-- 作り直しの側には encodable_profile の cut の確認条件（recording_chapter_ownership）を
-- 掛けず、設定にある cut プロファイルかだけを見る。押した時点の keep が利用者の確認
-- そのものだからである（encodable_profile の定義の近くのコメントと同じ判断）。
--
-- 各行は「現在の desired で未観測」のプロファイルで、running は試行表に running 行が
-- ある印（行は queued と running の分割になる）。active な encoded が既にある、
-- ごみ箱、設定から消えた profile の running 行は pending_profiles か述語で落ちるので
-- 数えない。`encode_attempts_exhausted` は ListMissingEncodeProfiles と同じ判定を
-- 共有し、上限に達した failed 行だけを両方から落とす。旧 worker の attempt_count = 0
-- は 1 回と数える。再要求で failed 行が消えると両方の集合に戻る。
--
-- name: ListEncodeQueue :many
WITH pending_profiles AS (
  SELECT u.recording_id, u.profile
  FROM unencoded_desired_profiles u

  WHERE encodable_profile(u.recording_id, u.profile, sqlc.arg('known_profiles')::text[], sqlc.arg('cut_profiles')::text[])

  UNION

  SELECT q.recording_id, q.profile
  FROM unsatisfied_encode_rebuilds q
  WHERE q.profile = ANY(sqlc.arg('cut_profiles')::text[])
)
SELECT
  m.recording_id,
  m.profile,
  EXISTS (
    SELECT 1 FROM recording_encode_attempts a
    WHERE a.recording_id = m.recording_id
      AND a.profile = m.profile
      AND a.state = 'running'
  )::boolean AS running
FROM pending_profiles m
JOIN encodable_recordings er ON er.recording_id = m.recording_id
WHERE NOT encode_attempts_exhausted(
  m.recording_id, m.profile, sqlc.arg('attempt_limit')::integer
)
ORDER BY m.recording_id, m.profile;

-- ListEncodedAssetRebuildRequests は reconcile が投入し直す、満たされていない作り直しを
-- 返す。ごみ箱の録画・原本が active でない録画は view（unsatisfied_encode_rebuilds）が
-- 落とす。投入してもごみ箱の録画を作る / EncodeWorker が作れない。要求行は消さず、
-- 復元されたら再び拾う。満たされた要求は返さない。
-- keep は int8multirange のまま返し、Go 側は pgstore.KeepRangesFromMultirange で戻す。
-- name: ListEncodedAssetRebuildRequests :many
SELECT q.recording_id,
       q.profile,
       q.requested_keep
FROM unsatisfied_encode_rebuilds q
WHERE sqlc.narg('recording_id')::bigint IS NULL
   OR q.recording_id = sqlc.narg('recording_id')::bigint
ORDER BY q.recording_id, q.profile;

-- GetEncodedAssetRebuildRequestForShare は要求行を FOR SHARE で読む。行が無ければ
-- pgx.ErrNoRows。公開 tx では、読んでから公開するまで api の upsert（押し直し）を
-- 待たせ、ジョブの keep と requested_keep が等しいときだけ公開する。エンコード前の
-- 確認（EncodeWorker.loadRequestedEncodeCut）は tx の外から呼ぶので、ロックは文の
-- 終わりで外れ、読み取りとしてだけ働く。
-- name: GetEncodedAssetRebuildRequestForShare :one
SELECT q.requested_keep
FROM encoded_asset_rebuild_requests q
WHERE q.recording_id = sqlc.arg('recording_id')
  AND q.profile = sqlc.arg('profile')
FOR SHARE;

-- ListCutAwaitingReview は「cut プロファイルを凍結しているのに、まだチャプターを
-- 確認していない」録画数をプロファイル名別に返す（メトリクス
-- rokuban_cut_awaiting_review）。
--
-- **これは失敗ではない。** ユーザーが確認すれば次のパスが拾う正常な待ち状態で、
-- 数が減らないこと自体は異常ではない。出している理由は、cut プロファイルを
-- 選んだのに確認の導線に気付かず「エンコードされない録画」が静かに溜まる形を
-- 見えるようにするためである（ListUnsatisfiableEncodeProfiles が拾うのは設定から
-- 消えたプロファイルで、こちらは別の集合）。
--
-- エンコードしてよい録画（encodable_recordings。ListMissingEncodeProfiles と同じ
-- view）だけ数える。原本が無ければ確認しても投入できないので、削除済み・未取り込みの
-- 録画を「待ち」に混ぜない。
--
-- name: ListCutAwaitingReview :many
SELECT want.profile::text AS profile, count(*)::bigint AS recordings
FROM recording_encode_policy p
JOIN encodable_recordings er ON er.recording_id = p.recording_id
CROSS JOIN LATERAL unnest(p.encode_profiles) AS want(profile)
WHERE want.profile = ANY(sqlc.arg('cut_profiles')::text[])
  AND NOT EXISTS (
      SELECT 1 FROM recording_chapter_ownership o WHERE o.recording_id = p.recording_id
  )
GROUP BY want.profile
ORDER BY want.profile;

-- ListUnsatisfiableEncodeProfiles は ListMissingEncodeProfiles が**落とした**側 --- 凍結済みの
-- desired が known_profiles に無いために永久に満たせない (プロファイル名, 録画数)
-- を返す。
--
-- 落とす判断（ListMissingEncodeProfiles）と落としたことを見せる責務を分けている。
-- 録画単位は同じ encodable_recordings を引く。プロファイルを
-- 改名 / 削除するとその名前で凍結済みの過去録画が一斉にここへ落ちるので、
-- 数を出さないと「エンコードされない録画」が静かに増える（過去に塞いだ
-- 症状そのものを、別の原因で再現してしまう）。
--
-- この値はプロファイル名別の該当録画数で、keep_original を問わず数える
-- （`always` の録画も含むので「回収されない原本の件数」そのものではない。
-- 1 録画が複数の消えたプロファイルを凍結していれば複数のラベルにまたがって
-- 数えられるため、ラベル間で単純合計すると録画数を超える）。この中の
-- until_encoded 録画は until_encoded_deletable_originals にとっても
-- 永久に「揃っていない」ため、原本が回収されない
-- （docs/storage/retention.md §保持ポリシー）。
--
-- cut の確認条件は持たない。cut_profiles は known_profiles と同じ設定
-- （config.EncodeConfig.CutProfileNames は ProfileNames と同じ Profiles から cut: true を
-- 選ぶ）から作る部分集合なので、known_profiles に無いプロファイルは cut でもない。
--
-- name: ListUnsatisfiableEncodeProfiles :many
SELECT u.profile, count(*)::bigint AS recordings
FROM unencoded_desired_profiles u
JOIN encodable_recordings er ON er.recording_id = u.recording_id
WHERE NOT (u.profile = ANY(sqlc.arg('known_profiles')::text[]))
GROUP BY u.profile
ORDER BY u.profile;
