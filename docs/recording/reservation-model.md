## 4. 予約モデル: base / overrides 分離

### 4.1 設計根拠（EPGStation v2.10.0 の問題）

EPGStation v2.10.0 の運用で「ルール予約を除外設定したはずなのに適用されない」「いつの間にか除外設定が外れる」という現象を確認した。原因は構造的に 2 つ:

1. **除外がルール予約単位**のため、複数ルールがマッチしていると別ルールの予約が生きて録画される（EPGStation#538）
2. **除外フラグが導出状態（予約行）に保存されている**ため、EPG 更新でルーラーが予約を再生成するとフラグごと消える

「ユーザーの意図を、コントローラが再生成する行に書く」ことが根本原因。以下の設計は両方を構造的に潰す。

### 4.2 base / overrides の分離

reservations の行を 2 層に分ける:

- **base**: ruler が「ルール x EPG」から計算するフィールド群（priority / エンコードプロファイル / 保持ポリシー / ファイル名等）。`reservations.base` に載り、**ruler だけが書く**
- **overrides**: ユーザーが上書きしたフィールドのみを持つ疎な jsonb。**`program_intents` 表に載り、api（ユーザー操作）だけが書く**
- **effective = base + overrides**。reconciler が mirakc に同期し ingest/encode が参照するのは常に effective

**意図（overrides）は導出行（reservations）とは別の表に置く。** ruler が base だけを書く規律でも上書きは守れるが、1 行が「ユーザー意図の永続記録」と「ruler の導出結果」を兼ねると、昇格・取消の分岐・削除の例外という 3 つの複雑さが派生する（issue #18 の案 A）。表を分けると:

| 1 表だったときに必要だったもの | 意図を分けた後 |
|---|---|
| 昇格（manual 行にルールがマッチしたときの effective 保存と `skip:false` の焼き付け） | **不要**。意図は据え置きで `rule_id` と base が変わるだけ |
| 取消の分岐（再生成者がいるか） | **不要**。無条件に `intent{skip}` を書いて導出行を落とす |
| 削除の例外（「overrides があれば消さず detached」） | **不要**。意図は別表なので導出行を消しても失われない |

`skip` は overrides のキーではなく **`program_intents.action`（`record` / `skip`）** という列にする。列なので base 側の skip に対する優先順位が明示的に決まり（`effective.skip = (action = 'skip') OR (意図がなく base.skip)`）、jsonb マージに細工を仕込まなくてよい。M2-6 の重複排除が base に skip を立てても、ユーザーの `record` 意図が勝つ。

> **この式は M2-6 の実装時まで満たされていなかった。** `db.EffectiveOptions` は `action = 'skip'` のときだけ `skip = true` を書いており、`action = 'record'` のときに base 側の skip を打ち消していなかった。base に skip を載せる経路が M2-6 まで存在しなかったので顕在化していなかっただけで、重複排除を入れた瞬間に「重複と判定された番組をユーザーが録れと指定しても録られない」として現れる。**意図があれば `action` が skip を決め切る**（`record` なら false で上書きする）形に直した。`(action = 'skip') OR (意図がなく base.skip)` を素直に読めばこうなる — 導出の式を docs に書いても、実装が片側の分岐しか持たないことはある。

意図と上書きの寿命は放送の寿命に揃える（番組終了後に GC）。

ruler は EPG 更新のたびに base を丸ごと再計算してよい --- **overrides は別表（`program_overrides`）にあるので構造的に触れない**。3-way merge は不要。ruler は `reservations` を、api は `program_intents` / `program_overrides` を書くので競合もない（ruler のパスはサイト単位で排他。[データ層](../data.md) §2）。api が `reservations` を書くのはルール削除 API の同期削除 1 本だけで、そこも WHERE の NOT EXISTS を適用の瞬間に再評価するため、#29 が問題にした窓（並行して着地する手動予約を踏み潰す）は生じない（ruler はルール一覧と desired を tx の外で読み tx 内で書くため、ルール削除と同時走行したパスが `rule_id` の FK 制約で失敗し再試行になる形自体は残る。§4.4）。**ルール側の変更は上書きしていないフィールドにだけ自動伝播する**（ユーザーの直感と一致）。

UI: 上書き中のフィールドにマーカー表示 + フィールド単位/予約単位の「ルールに戻す」（override を消すだけ）。

#### overrides API の形（M2-4 → M3-1 で宛先を `(site, programId)` に変更）

- `PATCH /api/sites/{site}/programs/{programId}/overrides` --- 値を書いたフィールドは override を設定、`reset` 配列に名前を挙げたフィールドは override を削除、どちらにも現れないフィールドは変更しない
- `DELETE /api/sites/{site}/programs/{programId}/overrides` --- 番組単位の「ルールに戻す」（`action` は触らない）

**`null` で消す形にはしない。** Go の `*T`（oapi-codegen が optional に生成する形）は「キーが無い」と「`null`」を区別できないため、null 方式では「消す」が「変更しない」に化けて黙って壊れる。明示的な `reset` 配列なら曖昧さがない。同じフィールドを値と `reset` の両方に書いたら 400（意図が不明なので推測しない）、`reset` に未知のフィールド名があったら 400（タイポを黙って無視しない）。

`skip` は PATCH では扱わない（`action` 列が担う）。取消は `PUT /api/sites/{site}/programs/{programId}/intent {action: skip}`（§4.4「取消」参照）。

マージは **Go 側で `db.ReservationOptions` の型付きフィールドとして行う**。SQL で `overrides || $1::jsonb` / `overrides - $1::text[]` とやらないのは下記「jsonb を許す条件」のため。同時 PATCH の心配は要らない（Rokuban は構造的に単一世帯用アプリで認証機構を持たない。[overview.md](../overview.md) §認証）ので、`program_snapshots` 行（PATCH の前段で必ず upsert する。FK の前提）を UPSERT の行ロックで直列化する。宛先が `reservations` ではなく `(site, programId)` になった（issue #29）ため、もはや `reservations` 行の存在に依存しない。

#### overrides は `program_intents` とは別の表に置く（M2-4）

意図の表は「録る / 録るな」だけを持ち、パラメータの上書きは `program_overrides` に分ける。

```
program_intents                  program_overrides
  site, program_id      (PK)       site, program_id      (PK)
  action  NOT NULL                 overrides  jsonb NOT NULL
    ('record' | 'skip')
```

番組の事実のスナップショット（`program_start_at` 等）はどちらの表にも無い。両者とも `(site, program_id)` の FK（`ON DELETE CASCADE`）で `program_snapshots` を参照する（Phase 1。[スキーマ](../schema.md) §3.7）。

**ユーザーが番組について主張しうることは 2 つあり、独立している**: ①録る / 録るな ②パラメータの上書き。1 つの行に同居させると、`action` が NOT NULL であるために **「パラメータだけ上書きした。録る録らないについては意見なし」を表現できない**。priority を 1 つ変えるだけで `action='record'`（= 録れ）を主張させられる。

その結果、行が空になったとき「この行はもともと何を主張していたのか」が行自身から読めず、`reservations.rule_id`（別表の、しかも直近 ruler パスのスナップショット）に問い合わせる必要が出る。それは次の場合に誤答する:

> 手動予約（`intent{record, {priority:7}}`）に後からルールがマッチして `rule_id` が埋まった状態で「ルールに戻す」を押す → `rule_id IS NOT NULL` なので意図の行が消える → 手動予約だったものが純粋なルール由来になり、その後ルールを編集して外れると**ユーザーの手動予約が消える**。

表を分けると:

| | 1 表だったとき | 分離後 |
|---|---|---|
| 「ルールに戻す」 | 掃除規則（`rule_id` で分岐） | **`program_overrides` の行を DELETE するだけ**。`program_intents` に触らないので手動予約が巻き込まれる経路が構造的に存在しない |
| 意味を持たない行 | CHECK で禁止する必要がある | **表現不可能**（空 = 行が無い） |
| 何も指定しない `PATCH {}` | 掃除規則が発火して意図を消しうる | 消すものが無いので何も起きない |
| M2-6 の dedup skip | priority を触っただけで無効化される | 意図の行が無いので `base.skip` が効く。手動予約（`intent{record}` あり）は正しく勝つ |

**「型階層ではないものに STI を使わない」という判断である。** 区別したいのは①②の presence の直積（4 通りのうち 1 つが禁止）であって、判別子ひとつで決まるサブタイプではない。判別子を無理に立てると、Single Table Inheritance の定番の欠点 --- サブタイプごとの NOT NULL が書けず、整合性がスキーマから CHECK とアプリコードへ逃げる --- がそのまま出る。

#### jsonb を許す条件

`overrides` に jsonb を許すのは、**それが `program_overrides` 自身のロジックでは一切使われない不透明なペイロードだから**である。予約のパラメータを上書きするためだけに存在し、内容でクエリも制約もしない。

したがって `CHECK (jsonb_strip_nulls(overrides) <> '{}')` のような**内容を検査する制約は置かない**。技術的には可能（`jsonb_strip_nulls` は IMMUTABLE なので CHECK に書けて `{"priority":null}` も弾ける）だが、「クエリはしないが制約はする」という中途半端な状態が一番悪い。**不透明なペイロードなら不透明に扱う。** 同じ理由でマージも SQL ではなく Go 側で型付きに行う。

この規則は他のテーブルの表現も説明する --- **内容でクエリするなら型付き列、Go に渡すだけなら jsonb**:

| 場所 | 表現 | 内容でクエリするか |
|---|---|---|
| `rules` | 型付き列 + 子テーブル（#3 の決定） | **する**（ユーザーが編集し UI が一覧・フィルタ） |
| `reservations.base` | jsonb | しない |
| `program_overrides.overrides` | jsonb | しない |
| `recordings.keep_original` / `encode_profiles` | 型付き列 | **する**（プロファイル別の録画一覧） |
| `schedule_sync.options` | jsonb | しない（mirakc 固有。不変条件 7） |

「オプションの組」を独立したテーブルに正規化はしない。`base` と `overrides` を判別子付きの 1 表に寄せることは上記の STI をやり直すことに等しい（base は完全 / overrides は疎、書き手が ruler / api、寿命も違う）。繰り返し現れる実体はテーブルではなく `db.ReservationOptions` という **Go の型**で、マージ点も `Effective()` の 1 箇所に既に正規化されている。

#### ruler から見た load-bearing な行

```
desired = (ルールにマッチした番組 − intent.action='skip')
        ∪ {intent.action='record' の番組}
        ∪ {program_overrides に行がある番組}
```

**上書きの行の存在も予約を存在させる。** §4.3「overrides あり → 削除せず detached で保持。意図は番組単位のユーザーの投資」の要求そのもの。`action` が答えているのは「録画するか」、行の存在が答えているのは「この番組にユーザーの投資があるか」で、別の問いなので両立する。

### 4.3 ライフサイクル: detached・再アタッチ・GC

EPG の変化・ルール編集でルールがマッチしなくなったとき:

| 状態 | 挙動 |
|---|---|
| `record` 意図も上書きもなし（`intent{skip}` のみ、または意図・上書きが一切ない） | 削除（通常の宣言的動作） |
| **`record` 意図または上書きがある（= `program_investments` view に行がある。#162）** | **削除せず detached 状態で保持**。`intent{skip}` があれば録画しない detached、それ以外は実質 manual として録画する |

**`detached` は mirakc への同期対象から外してはならない**（M2-4 で修正）。「実質 manual として録画する」は、reconciler が schedule を作るという意味である。`reconciler.listDesired` が `state = 'active'` で絞っていたため detached の予約は schedule が作られておらず、次の経路で**ユーザーの手動予約が黙って録画されなくなっていた**:

> 手動予約 → たまたまルールがマッチ（`state='active'`, `rule_id` が埋まる）→ そのルールを編集して外す → `state='detached'` → 同期対象から外れる

同期の可否を決めるのは state ではなく **`effective.skip`** である（`listDesired` は既にこれで絞っている）。state で除外してよいのは `orphaned` だけ --- 番組が終了しているので schedule を作る意味がない。

この混乱の原因は `state` が 2 種類の情報を混ぜていることにある:

| 値 | 正体 |
|---|---|
| `orphaned` | 番組終了後に schedule が観測されなかったという**独立した観測事実** |
| `active` / `detached` | `(rule_id, base)` から**導出できる値**（`detached ⟺ rule_id IS NULL AND base IS NOT NULL`） |

導出値を「同期対象か」のフィルタとして使ったのが誤りだった。`active` / `detached` は UI 表示（マーカー）のための派生値として扱う。

> **この式は当初、実装では満たされていなかった**（[#30](https://github.com/fetburner/rokuban/issues/30)。§4.2 の `EffectiveOptions` と同じ現象）。`internal/ruler/sql.go` は式を評価せず、**前パスの `rule_id` を見た遷移**を `state` 列に書いていた。そのためルールを**編集**して外れた場合は `detached` になるが、ルールを**削除**した場合は FK の `ON DELETE SET NULL` が先に `rule_id` を落とすので `active` のまま固定される —— 同じ「マッチしなくなった」状態が原因によって違う `state` になる不具合だった。あわせて `MarkReservationOrphaned` に `AND state = 'active'` が残っており、**detached 予約が永久に `orphaned` にならない**不具合もあった（M2-4 では `listDesired` 側だけを直した）。**Phase 1（#28 / #30）でこの `state` 列自体を撤去し、`active` / `detached` を API が読むたびに `(rule_id, base)` から計算する形にした**（[スキーマ](../schema.md) §3「active / detached / orphaned は API が都度導出する」）ことで、前パスの遷移を保存する列そのものが無くなり、この 2 件は構造的に再発しなくなった。導出は読むたびに評価するもので、やむなく列に焼くなら両方向のテストが要る、という教訓が残る。

重要: **skip の意図そのものは削除しない**。予約行は desired に入らないので消えるが、除外は `program_intents` に残る（この行の FK は `program_snapshots` を指すので予約行の削除では落ちない）。そのため「EPG の一時不整合で番組消失 → 予約行が消える → EPG 回復 → ruler が新規生成」という経路を通っても除外は生き残り、EPGStation の症状 2（除外が外れる）は EPG フリッカー経由では再発しない。

- **再アタッチ**: ルールが再マッチしたら base を再計算して再アタッチ（overrides はそのまま）。EPG がちらついても除外は生き残る
- **GC**: detached 行の削除は「番組の終了時刻を過ぎた後」のみ。ユーザー意図の寿命を放送の寿命に揃える
- programId 一意性は detached 行にも適用され、再マッチ時の重複予約は構造的に生まれない
- ルール自体の削除も同じ規則（**`record` 意図または上書きなし → 削除 / `record` 意図または上書きあり → 残す**。`program_investments` view の定義そのもの。`intent{skip}` のみの予約は投資に数えず削除する）。**ユーザーが個別に編集した予約は、ルールを消しても手動予約と同等に生き残る**。意図は番組単位のユーザーの投資であり、ルール削除とは別の意図。録画ドメインでは録り逃しが不可逆で余計な録画は消せば済むため、迷ったら録る側に倒す
  > 揃える前は `program_intents` の存在だけで判定していたため（action を限定しない EXISTS）、`intent{skip}` のみの予約が detached として残ると数えられていたが、直後の ruler パス（DeleteRule が同一 tx でヒントを投入するので数秒後）で導出削除され、内訳表示が数秒で消える行を「detached になった」と数える不整合になっていた（#162）
- **ルール削除の UX は可視化で解決する**: 削除 API は内訳（予約 N 件を削除、M 件は編集済みのため detached 化）を返し、UI は確認ダイアログとトーストに出す。detached 行は予約一覧にマーカー付きで現れ、個別に削除できる（§4.4 の取消分岐）。残る行は定義上「ユーザーが触ったものだけ」なので件数は常に少なく、1 件ずつ説明可能

除外が外れるのは、ユーザーが自分で「ルールに戻す」を押したときだけになる。

### 4.4 manual 予約との統一

manual 予約は「base を持たず、`program_intents` に `action = 'record'` だけがある」縮退形。ルール由来予約と同一コードパスで扱う。複数ルールマッチ時（最高 priority ルールが base を供給）も、勝者が入れ替われば base が変わるだけで意図は生存する。

state は「今、誰が base を供給しているか」の答えに過ぎない: base = NULL なら誰もいない / `active` はルールが毎パス再計算 / `detached` はかつてのルール（凍結された base）。§4.3 のとおりこれは `(rule_id, base)` からの導出値であり、同期の可否を決めるフィルタに使ってはならない（列としては Phase 1 で撤去済み。API が都度計算する）。

**`reservations.source` はかつて「今」ではなく「どう作られたか」を答えようとしていて、両方に失敗していた。** `internal/ruler/sql.go` は手動予約にルールがマッチすると `source` を `manual` → `rule` に書き換え、ルールが外れても戻さなかった。下の「昇格は要らない」と矛盾するうえ、`watcher` が `recordings.source` にコピーするため**手動予約した番組の録画履歴が恒久的に「ルール由来」と記録される**（永続資産なので不可逆）不具合があった（issue #26）。列を削除した現在は 2 つの事実が別々に読める --- 「ユーザーが録れと言った」は `program_intents.action='record'`、「いまルールが base を供給している」は `rule_id IS NOT NULL` --- ので `source` は API が都度この 2 つから導出して返す。

#### manual 行にルールがマッチしても昇格は要らない

意図が別表にあるので、手動予約済みの番組にルールがマッチしたとき ruler がやることは **`rule_id` と base を埋めるだけ**。`program_intents` には触らないので、ユーザーの上書きは定義上失われない。effective を保存するための細工（`skip:false` の焼き付け等）は不要になる。

ルールがマッチしなくなったら base は凍結され（ruler は上書きしない）、`rule_id` が外れて実質 manual として動く。意図は無関係に生き続ける。

逆方向（ルール予約済みの番組への手動予約）は行が既にあるので「予約済み」を返すだけ。

manual 予約を「その番組 1 つにマッチする自動生成ルール」として表現する統一はしない。rules がワンショットの行で埋まり、ユーザーが書いた永続資産と導出される短命な行の寿命が混ざる。

#### 取消は `PUT .../intent {action: skip}`。api は `reservations` に触れない（M3-1）

`PUT /api/sites/{site}/programs/{programId}/intent {action: skip}` は `program_intents` を書くだけで、`reservations` の行は同一トランザクションで削除しない（issue #29 の決定: 導出行 `reservations` の書き手は ruler だけにする）。行の削除は ruler が次の全量パスで「意図に基づいて desired から除外された」ことを検出して行う（非同期。`insertRulerPassHint` で ruler_pass を即座に投入するので実質秒オーダー。フロントエンドは楽観更新で一覧の見た目を即時反映する）。**行の状態による分岐はない。**

この「ruler だけ」の原則には例外が 1 つある。`DELETE /api/rules/{id}`（§4.3「ルール削除の UX」）はルール削除と同一 tx で `reservations` を直接 DELETE する（`internal/api/rules.go` の `DeleteRule` → `DeleteReservationsByRuleWithoutIntent`）。これは実装の手抜きではなく、§4.3「削除 API は内訳（削除 N 件・detached M 件）を返す」という要求の帰結である。ただし内訳のうち detached 側は削除前の別 COUNT（`CountReservationsByRuleWithIntent`）から得ており、DELETE 自体のロウカウントが要るのは deleted 側だけである（issue #153 で「削除前 COUNT だけ返して実削除は ruler の次パスに委ねる」案 B を検討したが、非同期化すると「削除 API は成功を返したのに一覧にまだ残っている」窓が生まれ、その削除が大量削除サーキットブレーカーの導出削除カウントに合流してしまうため、同期のほうが正確で単純と判断し却下した）。明示操作は同期・ブレーカー対象外という既存の線（[reconciler](reconciler.md) §3.2「大量削除サーキットブレーカー」が明示操作を対象にしない理由と同じ側）に乗るための例外であり、1 つの表に書き手が 2 人いる形（CLAUDE.md 不変条件 12 の兆候）だが、`DeleteReservationsByRuleWithoutIntent` の WHERE 句は `program_investments`（intent / overrides）を DELETE 実行の瞬間に再評価するため、issue #29 が問題にした「適用の瞬間の窓」（並行して着地する手動予約を踏み潰す）はここでは生じない。詳細は `internal/db/queries/rules.sql` の同クエリのコメント参照。

api が行を直接消さない理由は ruler 側の GC ロジックと同じ: 行を消すだけにしてはならない。**消された行と最初から無かった行は ruler から区別できない**（DELETE は「録画するな」という負の意図ごと情報を破壊する）ため、次の全量パスが復活させてしまう。意図が別表に残るので、勝者ルールが入れ替わっても・全ルールがマッチしなくなっても・再アタッチされても、除外は一貫して守られる。

**旧設計との違い**: M3-1 以前は `DELETE /api/reservations/{id}` が `intent{skip}` の書き込みと `reservations` 行の削除を同一トランザクションで行っていた（即時反映）。これは宛先が `reservations.id`（導出物）だったための構造的な制約（#29 症状 1: 導出行が「無い」ときしか `intent{record}` を書けない）を解消する過程で、書き込みの宛先を `(site, programId)` に変えたことの帰結として無くなった。

意図そのものを捨てたい（「この番組についての指定をなかったことにする」）場合は `DELETE /api/sites/{site}/programs/{programId}/intent` で `program_intents` の行を消す。ルールがマッチしていればその後の全量パスで普通のルール予約として作り直される。これは §4.2「空になった意図の掃除」で「ルールに戻す」が `rule_id IS NOT NULL` のときに行を消すのと同じ操作である。

### 4.5 録画開始後の編集

| フィールド | いつ効くか |
|---|---|
| `priority` | reconciler が DELETE + POST で schedule を再作成して反映（§3.2）。**録画開始後の recorder には効かない可能性が高い** |
| `contentPath` / `filenameTemplate` | **既存の schedule には反映されない**（contentPath は churn を避けるため差分対象外で、初回生成値に固定される。§3.2）。まだ schedule が作られていない予約にだけ効く |
| `encodeProfiles` / `keepOriginal` | **ingest が原本 media_asset をコミットする tx の中で `recordings.keep_original` / `recordings.encode_profiles` に焼かれる瞬間まで効く**（M3-14、issue #103）。録画開始後の変更でも、放送終了・ingest 完了より前ならこの録画に反映される。**ingest 完了後の変更はこの録画には反映されない**（次にルールがマッチする別の録画には反映される） |

UI で「開始後に意味を持つフィールド」を区別表示する。この表は overrides API のフィールド説明（`openapi.yaml`）にも同じ内容を書く --- API だけを見ている利用者が「上書きしたのに反映されない」で詰まらないようにするため。

**凍結の例外としての事後追加（issue #133）**: 上記の凍結後は `encodeProfiles` の変更（overrides 経由）はこの録画には反映されないのが原則だが、ユーザー起点の `POST /api/recordings/{id}/encode-profiles` による追加専用の書き換えだけは例外として認める。overrides / rule_id を経由せず `recordings.encode_profiles` に直接 union + dedup で書くため、既存の指定を消す経路は無い。原本削除済み（`until_encoded` でエンコード完了後に削除済み等）の録画には 409 を返し、追加を拒否する。適用範囲・実装経路の詳細は [ストレージ](../storage.md) §6「凍結の例外: 事後追加」参照。

**なぜ ingest コミット時に凍結するか**: `recordings` は永続資産だが、導出元（`reservations` / `program_overrides` / `program_intents`）は放送終了 + 猶予後に GC される寿命の短い表（CLAUDE.md 不変条件 12「表は行の寿命で割る」）。`recordings.encode_profiles` を「参照」ではなく値のコピーとして持つ（凍結する）しかない理由はここにある --- 導出元に依存させると、番組が EPG から消えて GC された時点で desired が消え、エンコード未完了の録画で原本削除（[ストレージ](../storage.md) §6「原本 TS の保持ポリシー」）が止まる／再エンコードが投入できなくなる。凍結する以上どこかの瞬間で確定させる必要があり、`recordings` 行自体は録画開始時（watcher）に作られるが、この表の約束（録画開始後の変更でも効く）を満たせる最後の瞬間が ingest コミットである。詳細は `internal/worker/ingest.go` の `resolveAndSnapshotEncodePolicy` の doc コメントと [ストレージ](../storage.md) §6 を参照。

**既に ingest 済みの録画は今回の変更で backfill されない。** 凍結は ingest コミット時の 1 回だけなので、この変更のデプロイ前に ingest が完了した録画は `keep_original = 'always'` / `encode_profiles = '{}'`（列の既定値）のまま残る --- これは凍結設計の正しい帰結であり、バグではない。過去分にも encode を投入したい場合は、上記「凍結の例外としての事後追加」（`POST /api/recordings/{id}/encode-profiles`）で個別に追加できる。ただし追加専用（不足分をユーザーが都度指定する）であり、`keep_original` の変更や一括自動 backfill は提供しない。

### 4.6 スコープ外

「この番組シリーズは常に...」のような永続的例外は overrides（予約の寿命 = 短命）ではなくルール側の機能。必要になったら別途検討。

---

