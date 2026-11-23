# 07 UI と MCP の設計（ドラフト）

quota-fleet に人間向け UI と AI 向け MCP の口を追加する設計。接続口の正本は引き続き HTTP API（06 §4）とし、UI と MCP はどちらも API の薄い層とする。二重のビジネスロジックを持たない。

背景: 実案件トライアル（2026-11-23、3d-prime Issue #23/#26 のチェーン）で、登録・監視・結果回収はコーディネーターの手作業（JSON 組み立て＋SSH 越しの curl、ポーリング、成果物ファイルの読み取りと転送）になっていることが実地で確認された。エンジン側（キュー、ゲート、起動、評価、納品記録）は自動化済みで、不足しているのは入口と出口である。

## 0. 大原則: 簡単に導入できて、簡単にみんなが使えること（2026-11-23 ゆうや決定）

UI と MCP はこの原則の下に置く。具体化は次のとおり。

- **単一バイナリで完結する**: 配布は Go の単一バイナリ（CGO_ENABLED=0）1つ。UI のテンプレートと静的資産は `go:embed` でバイナリに埋め込み、実行時に外部ファイルや Node 等のランタイムを要求しない。データストアは埋め込み SQLite のまま。
- **init コマンドで始まる**: `quota-fleet init` が最小の fleet.yaml（ローカルホスト＋claude／codex／agy の既定エージェント定義、API トークンの生成を含む）を書き出す。利用者はエージェントの実行パス程度を直すだけで `quota-fleet serve` で起動できる。
- **最小設定の目安**: 初回起動に必要な設定は10行程度に収める。ゲートや Evaluator の詳細設定は、書かなければ既定で動き、起動時のバナーに有効な設定と無効化されている機能を明示する（黙って無効化しない）。
- **UI は開けば分かる**: インストール直後の状態でブラウザから一覧と登録ができる。追加のビルド・デプロイ工程は無い。
- **MCP は貼れば繋がる**: `quota-fleet mcp config` が主要 AI クライアント（Claude Code 等）向けの設定スニペットを出力し、コピー＆ペーストで接続できるようにする。
- **README のクイックスタート**: 「バイナリ取得 → init → serve → UI で最初のジョブ登録」までを3手順で書く。

## 1. UI（serve 組み込み）

### 1-1. 方式
- serve プロセスが `/ui/` 以下にサーバーサイド描画の HTML を返す。Go の `html/template` のみで組み、JS フレームワークとビルド工程は入れない。画面遷移は通常のリンクとフォームで、状態変更は既存の JSON API を fetch で叩く。
- 認証は既存の Bearer トークンを流用する。初回アクセスでトークン入力を求め、`sessionStorage` に保持して以降の API 呼び出しに付ける。Cookie 認証は採らない（CSRF 面を持ち込まない）。
- 公開範囲は tailnet 内のみを前提とする。shadow/本番の serve は `127.0.0.1` バインドのままとし、`tailscale serve` で tailnet に出す。インターネット公開はしない。

### 1-2. 画面
1. **ダッシュボード**（`/ui/`）: 状態別件数、実行中ジョブ、保持中ジョブ、直近の納品。
2. **ジョブ一覧**（`/ui/jobs`）: 状態・agent・host のフィルタとタイトル検索。行は ID、タイトル、状態、agent/host、作成時刻、親へのリンク。
3. **ジョブ詳細**（`/ui/jobs/{id}`）: 指示文、受け入れ条件、result_text、last_error、親子関係、そのジョブの gate 判定と納品記録。操作はキャンセル、requeue（状態が許す場合のみ表示）。
4. **登録フォーム**（`/ui/jobs/new`）: title、agent、host、workdir、model、pool、alt_pools、parent_id（既存ジョブから選択）、instructions、acceptance_criteria。request_id は自動生成して表示する（再送時の冪等確認用）。Gate で保持された場合は 202 の応答（held_id と理由）をそのまま画面に出す。
5. **保持中ジョブ**（`/ui/gate/held`）: 理由と判定スナップショットつき一覧。手動解放とキャンセル。
6. **ゲート判定履歴**（`/ui/gate/decisions`）: 直近の判定（would_pass／would_hold、選択プール、測定時刻）。
7. **納品**（`/ui/deliveries`）: 未納品と納品済みの一覧。

### 1-3. API 側の追加
- 手動解放の HTTP 口が無い（現状は CLI のみ）ため、`POST /gate/held/{id}/release` を追加する。実装は既存の GateChecker の手動解放をそのまま使う。
- それ以外の画面は既存 API（`GET /jobs`、`GET /jobs/{id}`、`POST /jobs`、`POST /jobs/{id}/cancel`、`POST /jobs/{id}/requeue`、`GET /gate/held`、`DELETE /gate/held/{id}`、`GET /deliveries`、`GET /status`）で賄う。gate 判定履歴の取得口が API に無い場合は `GET /gate/decisions` を追加する。

## 2. MCP（AI 向けの口）

### 2-1. 方式
- 同一バイナリのサブコマンド `quota-fleet mcp` として実装し、stdio トランスポートで話す。
- MCP 層は HTTP API のクライアントとして実装する（接続先とトークンは環境変数 `QUOTA_FLEET_API_URL`／`QUOTA_FLEET_TOKEN`）。サーバーと同一ホストで動かす場合も API 経由に統一し、store への直結経路は作らない。これにより、コーディネーターの実行環境（別 VM）からそのまま使える。
- プロトコル実装は公式 Go SDK（modelcontextprotocol/go-sdk）を第一候補とする。依存追加が問題になる場合は、tools/list と tools/call のみの最小 JSON-RPC 実装に切り替える（判断は実装時に記録する）。

### 2-2. ツール
| ツール | 対応 API | 備考 |
|---|---|---|
| `create_job` | POST /jobs | 202（保持）も正常結果として held_id と理由を返す |
| `list_jobs` | GET /jobs | 状態・agent・host フィルタ |
| `get_job` | GET /jobs/{id} | 指示文・評価結果を含む |
| `cancel_job` | POST /jobs/{id}/cancel | |
| `requeue_job` | POST /jobs/{id}/requeue | |
| `list_held` | GET /gate/held | |
| `release_held` | POST /gate/held/{id}/release | 本設計で追加する口を使う |
| `cancel_held` | DELETE /gate/held/{id} | |
| `gate_decisions` | GET /gate/decisions | 本設計で追加する口を使う |
| `list_deliveries` | GET /deliveries | 未納品のみのフィルタ可 |
| `fleet_status` | GET /status | |

claim／complete／fail 等のランタイム内部操作は MCP に出さない（ワーカーとランタイムの専用であり、AI の誤操作面を広げない）。

## 3. 実施順序
1. **Phase UI-1**: API 追加（held release、gate decisions）＋ UI（ダッシュボード、一覧、詳細、登録、保持、判定履歴、納品）。shadow に載せ、tailnet で開いて実地確認する。
2. **Phase UI-2**: MCP サブコマンドと上記ツール。コーディネーター環境から登録→完了確認までを通し、手作業の curl を置き換える。
3. どちらも完了条件は「ゆうやが UI から登録でき、コーディネーターが MCP で登録・回収できる」ことの実地確認。

## 4. 非目標
- マルチユーザー、アカウント管理、権限分離はしない（トークン1本の運用のまま）。
- インターネット公開はしない。
- 作成後の指示文・タイトルの編集は作らない（title 不変の原則は UI でも維持）。
- 通知のプッシュ化（Webhook をチャットへ繋ぐ）は本設計の範囲外とし、別途扱う。
