> [runbook.md](../runbook.md) の一部。索引から辿る。

## EPGStation との並走

同じ mirakc を共有してよい。**チューナーの調停は mirakc が行う**ので、Rokuban が
EPGStation の録画を奪うことはない。ただし物理的な制約は残る。

視聴・寿命・移行の出口基準と切替手順は [cutover.md](cutover.md) にある。

### 同一チャンネルなら競合しない

mirakc は同じ物理チャンネルのストリームを複数の購読者で共有する。EPGStation が
GR/27 をライブ視聴していて Rokuban が GR/27 を録画する場合、チューナーは 1 本で足りる。

### 別チャンネルはチューナー数で競合する

チューナーが 1 本しかない環境では、EPGStation の録画と Rokuban の録画が別チャンネルに
なった時点でどちらかが録れない。負けた側は `recording.failed` の
`need-rescheduling` になる（`rokuban_recordings_failed_total{reason="need-rescheduling"}`）。

調停は `priority` で行う。Rokuban の既定は 10。EPGStation 側の優先度と揃えるか、
**並走中はチャンネルが重ならない番組で試す**のが安全。

### EPG 収集もチューナーを使う

mirakc の `update-schedules` ジョブ（既定 08:21 / 20:21、timeout 10 分）は
物理チャンネルごとにチューニングして EPG を集める。この時間帯に録画を入れると
競合しやすい。

逆にこのジョブが特定チャンネルで失敗すると、そのチャンネルの番組が
`/api/programs` から返らなくなる。Rokuban は**番組を返さなかったチャンネルの
プロジェクションを消さない**ようにしてあるが、
`rokuban_epg_channels_without_programs` が 0 以外で続くなら mirakc 側の
収集失敗を疑う。

### 二重録画に注意

Rokuban と EPGStation の両方に同じ番組の予約が入っていると、**同じ番組を 2 回録る**
（tag が違うので互いに相手の schedule を消さない）。reconciler は
rokuban tag（`program:{programId}`）が無い schedule を触らない。ディスクとチューナーを二重に消費するので、シャドー
運用中は片方だけに予約を入れる。
