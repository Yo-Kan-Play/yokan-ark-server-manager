## 0. スコープ

本仕様書は、`yokan-ark` リポジトリに含まれる **Discord Bot コンテナ**の要件仕様を定義する。  
Bot は **rootless Podman** を前提に動作する。  
Bot は **Podman socket** 経由で ARK マップコンテナを制御する。

### 0-1. 目的
- Discord のスラッシュコマンドで ARK マップを起動・停止・監視する。
- 無人時の自動停止を行う。
- ローカルバックアップとクラウドバックアップを行う。
- 起動中サーバーへ一斉メッセージ送信（broadcast）を行う。
- 指定時刻に自動メッセージ送信（announcements）を行う。
- 可能ならシャットダウン時に安全停止を試みる（best effort）。

### 0-2. 非目的（やらない）
- compose の生成・利用は行わない。
- ファイアウォール設定やポート開放は行わない。
- ARK サーバー本体のビルドや Mod 管理の自動化は本仕様に含めない。
- Discord Bot のソースコード実装方式（ライブラリ選定など）の強制はしない。

---

## 1. 用語
- **マップ**: ARK(ASA) の 1 サーバーインスタンス。`map_id` で識別する。
- **マップコンテナ**: 1マップ=1コンテナ。停止状態で用意され、Bot が起動する。
- **Podman socket**: Podman API を叩く Unix socket。例: `/run/user/<uid>/podman/podman.sock`
- **RCON**: サーバー管理用のリモートコンソール。人数取得、save、broadcast に使う。
- **コマンド受付チャンネル**: Bot がコマンドを受け付ける Discord チャンネル。
- **通知チャンネル**: 起動・停止・バックアップなどの通知を送る Discord チャンネル。

---

## 2. 全体アーキテクチャ（Bot 観点）
- Bot は Quadlet 等で常駐する。
- マップコンテナは「停止状態のコンテナ」として存在する前提とする。
- Bot は config.yaml を正として、必要ならコンテナを create する（停止状態）。
- Bot は Podman socket 経由で `start/stop/inspect/stats` を行う。
- Bot は RCON で人数取得、saveworld、broadcast を行う。

---

## 3. Bot 表示言語と UI 方針

### 3-1. 言語
- コマンドは英語でよい。
- レスポンス、ステータス表示、通知、エラーはすべて日本語とする。

### 3-2. プレゼンス表示（簡易サーバー情報）
Bot のプレゼンス（Discord のオンライン表示）は短い情報だけを表示する。  
詳細は `/ark status` に集約する。

#### 3-2-1. 表示内容（推奨）
- 起動中マップ数 / 上限（例: `2/2`）
- 起動中マップの合計人数（例: `Players 5`）
- 無人監視の次回スキャンまでの残り（例: `Scan 7m`）または最終スキャン時刻
- ローカルバックアップの次回予定までの残り（例: `Backup 34m`）または次回予定時刻

#### 3-2-2. 表示フォーマット（例）
- `ARK 2/2 | Players 5 | Scan 7m | Backup 34m`
- 起動中マップが 0 の場合: `ARK 0/2 | Idle`

#### 3-2-3. 更新頻度
- 60 秒〜180 秒ごとに更新する。
- Discord のレート制限に配慮する。

---

## 4. 設定方式（config.yaml）

### 4-1. 設定ファイル
- 既定パス: `yokan-ark/bot/config.yaml`
- `config.example.yml` / `config.example.yaml` はサンプルとして提供する。
- 実運用は `config.yaml` に統一する。

### 4-2. 必須設定ブロック
- `discord`
- `podman`
- `runtime`
- `maps`

### 4-3. 設定スキーマ（概要）
```yaml
discord:
  command_channel_ids: [123456789012345678]
  notify_channel_id: 234567890123456789
  command_guild_id: "" # 開発時のみ guild 指定。空なら global 登録

permissions:
  default:
    allow_role_ids: [111111111111111111]
  commands:
    start:     { allow_user_ids: [123456789012345678] }
    stop:      { allow_user_ids: [123456789012345678] }
    save:      { allow_user_ids: [123456789012345678] }
    backup:    { allow_user_ids: [123456789012345678] }
    scan:      { allow_user_ids: [123456789012345678] }
    broadcast: { allow_user_ids: [123456789012345678] }
    status:    { allow_user_ids: [] }
    players:   { allow_user_ids: [] }
    maps:      { allow_user_ids: [] }

runtime:
  max_running_maps: 2
  scan_interval_minutes: 15
  idle_stop_minutes: 30
  save_wait_seconds: 10
  rcon_timeout_seconds: 5
  podman_timeout_seconds: 20

podman:
  socket_path: /run/user/1000/podman/podman.sock
  server_image: docker.io/yourname/yokan-asa-multimap:latest
  persist_host_path: /srv/yokan-ark/persist
  persist_container_path: /srv/yokan-ark/persist
  container_name_prefix: yokan-ark-
  # create を Bot が行う場合に使用
  create_defaults:
    timezone_mount: true
    publish_query_port: true

server_defaults:
  max_players: 10
  cluster_id: yokan-ark
  memory_limit_gb: 20
  rcon_password_env: ARK_RCON_PASSWORD
  rcon_host: host.containers.internal
  rcon_port_offset: 19243  # rcon_port = port + offset
  query_port_offset: 1     # query_port = port + 1

backup:
  local:
    enabled: true
    interval_minutes: 120
    keep_generations: 3
    run_on_stop: true
    out_dir: /srv/yokan-ark/backups/local
  cloud:
    enabled: true
    schedule_hhmm: "01:00"
    keep_days: 2
    provider: cloudflare_r2
    bucket: yokan-ark-backups
    prefix: yokan-ark
    # 認証は Bot コンテナの env で与える想定
    # R2_ACCESS_KEY_ID / R2_SECRET_ACCESS_KEY / R2_ENDPOINT など

announcements:
  enabled: true
  items:
    - name: pre_shutdown_warning_10m
      time: "01:50"
      weekdays: ["mon","tue","wed","thu","fri","sat","sun"]
      message: "サーバーは10分後に停止します。作業中の人は安全な場所へ移動してください。"
      include_maps: []
    - name: pre_shutdown_warning_1m
      time: "01:59"
      message: "サーバーはまもなく停止します。ログアウトしてください。"
      include_maps: []

pre_shutdown:
  enabled: true
  time: "01:55"
  action: graceful_stop_all

shutdown_guard:
  enabled: true
  best_effort: true

maps:
  - map_id: TheCenter_WP
    display_name: The Center
    session_name: Yokan Ark The Center
    port: 7777
  - map_id: ScorchedEarth_WP
    display_name: Scorched Earth
    session_name: Yokan Ark Scorched Earth
    port: 7787
  - map_id: Aberration_WP
    display_name: Aberration
    session_name: Yokan Ark Aberration
    port: 7797
```

### 4-4. ポート方針（運用ルール）

* マップの `port` は **10刻み**で割り当てる。例: `7777 -> 7787 -> 7797`
* 追加の導出ポートは Bot が計算する。

  * `rcon_port = port + rcon_port_offset`（既定 `+19243`）
  * `query_port = port + query_port_offset`（既定 `+1`）
* マップごとの `port` 以外は共通化することを優先する。

---

## 5. チャンネル制御（追加要件）

### 5-1. コマンド受付チャンネル

* `discord.command_channel_ids` に含まれるチャンネルでのみコマンドを受け付ける。
* それ以外のチャンネルで実行された場合は拒否する。

#### 拒否時レスポンス（日本語）

* `このチャンネルではコマンドを受け付けません`

### 5-2. 通知チャンネル

* `discord.notify_channel_id` に、次の通知を送る。

  * 起動成功 / 起動失敗
  * 停止成功 / 停止失敗
  * 無人停止の実行結果
  * ローカルバックアップ実行結果
  * クラウドアップロード実行結果
  * pre_shutdown 実行結果
  * 自動メッセージ実行結果

---

## 6. 権限制御（追加仕様）

### 6-1. 権限評価の優先順位

1. コマンドに `allow_user_ids` が明示されている場合、その ID のみ許可する。
2. 1 が無い場合、`permissions.default.allow_role_ids` があれば、該当ロール保有者を許可する。
3. どちらも無い場合の既定挙動は安全寄りとする。

### 6-2. 既定挙動（安全寄り）

* `status / maps / players` は許可してよい。
* `start / stop / save / backup / scan / broadcast` は拒否する。

### 6-3. 拒否時レスポンス（日本語）

* `このコマンドを実行する権限がありません`

---

## 7. ステータス表示要件

### 7-1. /ark status に含める情報（日本語）

* 起動中のマップ名
* 各マップの人数（RCON）
* 各マップのメモリ使用量（Podman stats）
* 現行マップのファイルサイズ（保存ディレクトリの合計）
* 最新ローカルバックアップのファイルサイズ
* 最新クラウドバックアップ（R2）のサイズ
* 次回ローカルバックアップ予定時刻（起動時刻＋interval）
* 同時起動数 / 上限

### 7-2. ファイルサイズ計算

* `save_dir` を config に明示しない場合、次の既定パスを使用する。

  * `${podman.persist_host_path}/maps/${map_id}/server-files/ShooterGame/Saved`
* 例外が必要な場合のみ `maps[].save_dir_override` を許可する。

---

## 8. コマンド仕様

### 8-1. /ark start <map>

* 上限チェック（`max_running_maps`）
* マップコンテナ start
* 起動完了確認（任意: RCON 応答待ち）
* 成功時に通知チャンネルへ通知する

#### 上限超過時

* 起動拒否する。自動入れ替えはしない。

#### 成功レスポンス（日本語）

* `マップを起動しました: <display_name>`

#### 失敗レスポンス（日本語）

* `マップの起動に失敗しました: <display_name>`

### 8-2. /ark stop <map>

* RCON `saveworld`
* `save_wait_seconds` 待機
* コンテナ stop
* `backup.local.run_on_stop: true` の場合はローカルバックアップを実行

#### 成功レスポンス（日本語）

* `マップを停止しました: <display_name>`

### 8-3. /ark status

* 7章の要件に沿って表示する

### 8-4. /ark save

* 起動中の全マップへ `saveworld`
* 成否をマップごとにまとめて返す

### 8-5. /ark backup

* 起動中の全マップへ `saveworld`
* 待機
* ローカルバックアップを即時作成
* 成否をマップごとにまとめて返す

### 8-6. /ark scan

* 無人監視スキャンを即時実行する
* 無人停止判定を更新する
* 停止が発生した場合は通知チャンネルへ通知する

### 8-7. /ark players [map]

* map 指定があればそのマップのみ表示する
* map 指定が無ければ起動中マップを列挙する
* RCON で人数とプレイヤー一覧を取得する

### 8-8. /ark maps

* config.yaml に登録されたマップ一覧と状態を表示する

### 8-9. /ark broadcast <msg>

#### 目的

* 起動中の全マップに対して、サーバー内チャットへメッセージを送信する。
* 送信は RCON 経由で行う。

#### 対象マップ

* 既定は「起動中の全マップ」。
* 起動中マップが 0 の場合は送信せず、日本語で理由を返す。

#### 権限

* `permissions.commands.broadcast` を使用する。

#### 成功時レスポンス（日本語）

* `起動中の全マップにメッセージを送信しました（対象: Nマップ）`

#### 失敗時レスポンス（日本語）

* `メッセージ送信に失敗しました（失敗: <map_id>）`
* 失敗マップがある場合はマップ単位で列挙する。

---

## 9. 自動停止（15分スキャン）

### 9-1. スキャン間隔

* `scan_interval_minutes`（既定 15分）で起動中マップの人数をチェックする。

### 9-2. 無人停止条件

* `idle_stop_minutes`（既定 30分）を超える無人状態が続いたら停止する。
* 実装は次でもよい。

  * `scan_interval_minutes` 間隔で 0人が連続 N 回（例: 2回）なら停止

### 9-3. 停止手順

* `saveworld`
* 待機（`save_wait_seconds`）
* `stop`

---

## 11. バックアップ運用

### 11-1. ローカルバックアップ（間隔のみ）

* 固定時刻は使わない。
* `local.interval_minutes` を使用する。
* マップ起動時刻を基準に interval ごとにバックアップする。
* `run_on_stop: true` の場合、停止時にもバックアップする。
* ローカル保持は各マップ `keep_generations` 世代を残し、古いものは削除する。

### 11-2. クラウドバックアップ（固定時刻のみ）

* `cloud.schedule_hhmm` の固定時刻で実行する。
* Cloudflare R2 にアップロードする。
* R2 側の保持は各マップ `keep_days` 日分とし、それより古いものを削除する。
* 毎日1回、各マップの「最新ローカルバックアップ1本だけ」をアップロードする。
* 対象は「起動中」ではなく「最新ローカルバックアップが存在するマップ」。

---

## 12. 自動メッセージ機能（announcements）

### 12-1. 目的

* `/ark broadcast` と同等の送信を、YAML で指定した時刻に自動実行する。
* Ubuntu Server が 02:00 に停止する前に警告メッセージを送れるようにする。

### 12-2. スケジュール形式

* 時刻指定は `HH:MM`（Asia/Tokyo）とする。
* 曜日指定は任意とする。
* 複数設定を許可する。

### 12-3. 対象マップ

* 既定は「起動中の全マップ」。
* `include_maps` を指定した場合は、その集合のうち起動中だけを対象にする。

### 12-4. 実行ログ

* 自動メッセージ実行の成否を通知チャンネルへ送る。
* 失敗はマップ単位で列挙する。

---

## 13. pre_shutdown（確実性の高い停止）

### 13-1. 目的

* OS 停止が確定している運用（例: 毎日 02:00）に対して、事前に安全停止を実行する。

### 13-2. 挙動

* `pre_shutdown.time` に `pre_shutdown.action` を実行する。
* `graceful_stop_all` の場合は次を行う。

  * 起動中の全マップへ `saveworld`
  * `save_wait_seconds` 待機
  * 起動中の全マップを stop
  * `backup.local.run_on_stop` に従いローカルバックアップ

### 13-3. 通知

* 実行開始と完了を通知チャンネルへ送る。
* 失敗時は失敗マップを列挙する。

---

## 14. シャットダウンブロック（任意・best effort）

### 14-1. 目的

* ホスト OS がシャットダウンしようとしたタイミングで、可能なら「保存→停止」を完了するまでシャットダウンを保留する。

### 14-2. 仕様上の扱い

* シャットダウン保留は「できる場合がある」。
* 権限・systemd 設定・シャットダウン方法に依存する。
* 本機能は `best_effort` とし、失敗しても通常の停止処理にフォールバックする。

### 14-3. 実装方針（推奨）

* Bot が SIGTERM を受けた際に、次を行う。

  * 起動中の全マップへ `saveworld`
  * `save_wait_seconds` 待機
  * 起動中の全マップを stop
  * `run_on_stop` が true ならローカルバックアップ
* 追加で可能なら systemd inhibitor（delay）を取得する。

  * `shutdown_guard.enabled: true` の場合のみ試行する。
  * inhibitor 取得に失敗した場合はログに残し、SIGTERM ハンドリングのみ行う。

---

## 15. Podman 操作要件

### 15-1. 必須操作

* `podman start <container>`
* `podman stop <container>`
* `podman inspect <container>`
* `podman stats --no-stream <container>`

### 15-2. create（任意）

* Bot がコンテナ create を担当する場合、次を満たす。

  * コンテナ名: `${container_name_prefix}${map_id}`
  * イメージ: `podman.server_image`
  * env: `MAP_ID / SESSION_NAME / PORT`
  * volume: `${persist_host_path}:/persist:rw`
  * port publish:

    * UDP `${port}:${port}`
    * UDP `${port+query_port_offset}:${port+query_port_offset}`（設定で無効化可）
    * TCP `${rcon_port}:${rcon_port}`

---

## 16. 競合と排他（重要）

* 同一マップに対する操作（start/stop/save/backup/broadcast）は同時実行しない。
* `backup` と `stop` は同一マップで排他する。
* `scan` は実行中でもコマンドで再実行できるが、内部でキューイングする。

---

## 17. エラー処理方針

* 部分失敗は許容する。
* 失敗はマップ単位で列挙する。
* タイムアウトは設定値に従う。
* Podman socket が死んでいる場合は通知チャンネルへ警告する。

---

## 18. ログ・通知方針

* 重要イベントは通知チャンネルへ送る。
* 詳細ログはコンテナ標準出力へ出す。
* ログには `map_id` と `action` と `result` と `elapsed_ms` を含める。

---

## 19. セキュリティ注意

* Podman socket のマウントは強い権限を与える。
* Bot コマンドは allowlist 運用を推奨する。
* 受付チャンネル制限と権限制御を必須とする。
