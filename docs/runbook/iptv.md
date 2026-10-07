> [runbook.md](../runbook.md) の一部。索引から辿る。

# IPTV / XMLTV を外部プレイヤーで確認する

## 前提

実チューナーを使うライブ確認には `live.enabled: true` と 1 つ以上の
`live.profiles` が必要である。EPG が同期済みで、少なくとも 1 つの局が
`GET /api/sites/{site}/services` に現れることも確認する。

録画確認には完了済み録画と active な原本または encoded asset を用意する。
字幕確認には `subtitles: webvtt` を設定した encoded profile と字幕のある録画が要る。
リバースプロキシ認証も試す場合は、Basic 認証などプレイヤーが扱える方式を用意する。

```sh
URL=http://localhost:40773
ID=1
PROFILE=h264
LIVE_PROFILE=hd
```

## 出力を確認する

M3U の URL は相対パス参照なので、ファイルをダウンロードせずネットワーク URL を
プレイヤーへ渡す。

```sh
curl -fsS "$URL/api/iptv/playlist.m3u?include=all&liveProfile=$LIVE_PROFILE&recordingProfile=$PROFILE"
curl -fsS "$URL/api/iptv/xmltv.xml" -o /tmp/rokuban.xml
xmllint --noout /tmp/rokuban.xml
```

M3U には `#EXTM3U` と既存のライブ・録画 URL が含まれる。ライブ局のエントリだけが `tvg-id` を持つ。
XMLTV の `channel id` は局の `tvg-id` に一致する。録画のエントリに `tvg-id` は付かない。
`url-tvg` は出さないので、XMLTV の URL は IPTV クライアント側に手で設定する。
`programme/url` は `/api/sites/{site}/programs/{programId}` を指す。
`live.enabled: false` の場合、M3U の `include=live` は 404 になり、
`include=all` は録画だけを返す。

## VLC でライブ・録画・シークを確認する

1. VLC の「ネットワークストリームを開く」に次の M3U URL を入力する。
   VLC が局と録画を列挙することを確認する。

   ```text
   http://localhost:40773/api/iptv/playlist.m3u?include=all&liveProfile=hd&recordingProfile=h264
   ```

2. ライブ局を選び、実放送が再生されることを見る。
   チャンネルを切り替えて別局も再生する。
3. 録画を選び、冒頭と後半へシークする。
   再生が追いつき、映像と音声が続くことを確認する。
4. 元素材を指定した `PROFILE` の録画を再生し、以下で HTTP Range の応答も見る。

   ```sh
   curl -sS -D - -o /dev/null -H 'Range: bytes=0-1023' \
     "$URL/api/media/recordings/$ID/file?profile=$PROFILE"
   ```

   `206 Partial Content` と `Content-Range: bytes 0-1023/...` が返る。
   プレイヤーで前後へシークできることも確認する。

## WebVTT 字幕を確認する

字幕サイドカーは encoded file と同じ `profile` を指定する。
次の応答が `WEBVTT` で始まることを確認する。

```sh
curl -fsS "$URL/api/media/recordings/$ID/file?profile=$PROFILE&track=subtitles" | head
```

VLC では同じ動画 URL を開き、`--sub-url` に字幕 URL を指定して再生する。
字幕の切替と同期を確認する。

```sh
vlc --sub-url="$URL/api/media/recordings/$ID/file?profile=$PROFILE&track=subtitles" \
  "$URL/api/media/recordings/$ID/file?profile=$PROFILE"
```

字幕が無い録画では字幕 URL が 404 になる。これはサイドカーを作らない番組に対する
通常の応答であり、映像ファイルの再生失敗とは区別する。

## リバースプロキシ認証を確認する

M3U、XMLTV、ライブ HLS、録画ファイルのすべてを同じ認証境界の内側に置く。
まず認証無しの要求が 401 になることを確かめる。

```sh
curl -sS -o /dev/null -w '%{http_code}\n' "$URL/api/iptv/playlist.m3u"
curl -sS -o /dev/null -w '%{http_code}\n' "$URL/api/media/recordings/$ID/file"
```

次に認証付きで M3U URL を VLC に渡す。資格情報を URL に付けず、プレイヤーの HTTP
認証入力で与える。認証後にチャンネル切替・録画のシーク・字幕 URL も通ることを見る。
プロキシが Basic 認証以外を使う場合は、その方式をプレイヤーが扱えることも確認する。

## 削除・原本消去を確認する

使い捨ての録画で確認する。まず `recordingProfile` を省略した M3U に原本が載ることを
見る。原本アセットを削除すると、新しく取得した M3U からその録画が消える。
削除前に取得した URL も 404 になる。

encoded profile を指定した M3U は、その profile の active asset がある録画だけを載せる。
原本を消した後も encoded asset が active なら encoded profile の M3U には残り、再生できる。
ごみ箱へ移した録画と完全削除済みの録画は、新旧いずれの URL でも 404 になる。

## 実測記録

確認時にプレイヤー名・バージョン、接続経路、再生した局と録画、Range 応答、
字幕の有無、認証方式を記録する。実チューナーが無い環境ではライブ再生を確認済みにせず、
この手順を使って実機で追試する。
