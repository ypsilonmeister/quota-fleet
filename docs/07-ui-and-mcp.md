# 07 UI と MCP の設計（v2）

quota-fleet に人間向け UI と AI 向け MCP の口を追加する設計。接続口の正本は引き続き HTTP API（06 §4）とし、UI と MCP はどちらも API の薄い層とする。二重のビジネスロジックを持たない。

背景: 実案件トライアル（2026-11-23、3d-prime Issue #23/#26 のチェーン）で、登録・監視・結果回収はコーディネーターの手作業（JSON 組み立て＋SSH 越しの curl、ポーリング、成果物ファイルの読み取りと転送）になっていることが実地で確認された。エンジン側（キュー、ゲート、起動、評価、納品記録）は自動化済みで、不足しているのは入口と出口である。

v2 は、かせ（agy、Gemini 系）による独立レビュー（2026-11-23、全体判定「修正すれば採用可」、必須修正6件・推奨5件）を受けて改訂した。指摘と対応の対応表は付録に置く。

## 0. 大原則: 簡単に導入できて、簡単にみんなが使えること（2026-11-23 ゆうや決定）

UI と MCP はこの原則の下に置く。具体化は次のとおり。

- **単一バイナリで完結する**: 配布は Go の単一バイナリ（CGO_ENABLED=0）1つ。UI の資産は `go:embed` でバイナリに埋め込み、実行時に外部ファイルや Node 等のランタイムを要求しない。データストアは埋め込み SQLite のまま。
- **init コマンドで始まる**: `quota-fleet init` が最小の fleet.yaml を書き出す（§3 Phase UI-0）。
- **最小設定の目安**: 初回起動に必要な設定は10行程度に収める。ゲートや Evaluator の詳細設定は、書かなければ既定で動き、起動時のバナーに有効な設定と無効化されている機能を明示する（黙って無効化しない）。
- **UI は開けば分かる**: インストール直後の状態でブラウザから一覧と登録ができる。追加のビルド・デプロイ工程は無い。登録フォームの agent／host／pool は手打ちでなく候補から選べる（§1-3 の `GET /meta`）。
- **MCP は貼れば繋がる**: `quota-fleet mcp config` が主要 AI クライアント向けの設定スニペットを出力し、コピー＆ペーストで接続できるようにする（§3 Phase UI-0）。
- **README のクイックスタート**: 「バイナリ取得 → init → serve → UI で最初のジョブ登録」までを3手順で書く（§3 Phase UI-0）。
- **サポート対象 OS の境界（v1）**: serve とローカルワーカー実行は Linux（Windows の WSL2 を含む）を対象とする。macOS／Windows ネイティブでは CLI と MCP クライアント（リモート serve への接続）のみを対象とし、README に明記する。macOS／Windows でのローカル serve 対応（LocalTransport のシェル分岐、darwin の許可、setsid 非依存のデタッチ）は将来課題として §4 に残す。

## 1. UI（serve 組み込み）

### 1-1. 方式（レビュー M1 を受けて CSR に統一）
- UI は **シェル配信＋クライアント描画**とする。serve は `/ui/` 以下に `go:embed` の HTML 骨格（データを含まない）を無認証で返し、画面のデータはすべてブラウザ側 JS が既存の JSON API を `fetch`（Bearer トークン付き）して描画する。これにより「API の薄い層」「Cookie／CSRF なし」が矛盾なく両立する。
- 認証は既存の Bearer トークンを流用する。初回アクセスでトークン入力を求め、`sessionStorage` に保持して以降の API 呼び出しに付ける。シェル HTML 自体にジョブデータは一切含めないため、無認証配信でもデータは漏れない。
- XSS 対策を必須とする: 描画は `textContent` を徹底し `innerHTML` を使わない。シェルには CSP ヘッダ（`default-src 'self'`）を付ける。
- 公開範囲は tailnet 内のみを前提とする。serve は `127.0.0.1` バインドのままとし、`tailscale serve` で tailnet に出す。インターネット公開はしない。

### 1-2. 画面
1. **ダッシュボード**（`/ui/`）: 状態別件数、実行中ジョブ、保持中ジョブ、直近の納品。
2. **ジョブ一覧**（`/ui/jobs`）: 状態タブ（状態別カウントつき）＋ agent・host フィルタとタイトル検索。行は ID、タイトル、状態、agent/host、作成時刻、親へのリンク。「全状態を1回で取得」はしない（06 §1.1 の原則どおり status フィルタ必須を維持し、一覧はタブ切替で賄う）。表示はチェーン（親子の木。既定の状態は「すべて」）とフラット（既定の状態は queued）の2種類で、既定はチェーン。切替ボタンで選んだ表示はブラウザの localStorage に保持し、URL の `view=chain` / `view=flat` があればそれを優先する（保持値は変えない）。
3. **ジョブ詳細**（`/ui/jobs/{id}`）: 指示文、受け入れ条件、result_text、last_error、親子関係（子ジョブ一覧）、そのジョブの gate 判定と納品記録。操作はキャンセルと requeue（状態が許す場合のみ表示）。**実行中（claimed）のキャンセルは UI に出さない**（§2-3 の非目標）。
4. **登録フォーム**（`/ui/jobs/new`）: title、agent、host、workdir、model、pool、alt_pools、parent_id（既存ジョブから選択）、target_ref、worktree_spec（repo_path／base_ref／branch、折りたたみ詳細欄）、instructions、acceptance_criteria。request_id は自動生成して表示する（再送時の冪等確認用）。Gate で保持された場合は 202 の応答（held_id と理由）をそのまま画面に出す。
5. **保持中ジョブ**（`/ui/gate/held`）: 理由と判定スナップショットつき一覧。解放試行（§1-3）とキャンセル。
6. **ゲート判定履歴**（`/ui/gate/decisions`）: 直近の判定（would_pass／would_hold、選択プール、測定時刻）。
7. **納品**（`/ui/deliveries`）: 未納品／納品済みの切替つき一覧。

### 1-3. API 側の追加・拡張（レビュー M3・M5）
- `POST /gate/held/{id}/release` を追加する。意味は **「即時再評価による解放試行」**であり、強制解放ではない。再評価で健全ならジョブを作成して解放し、まだ枯渇中なら解放しない。応答契約: 解放成功は `200 {released: true, job_id}`、未回復で見送りは `409 {released: false, reason}`、既に解放／キャンセル済みは `409`、存在しない場合は `404`。強制解放オプションは v1 では設けない。serve は GateChecker（またはそのインターフェース）を api.Server へ注入する構成に変える。
- `GET /jobs` に `agent`、`host`、`q`（タイトル部分一致）、`parent_id` のフィルタを追加する（`status` 必須は維持）。
- `GET /deliveries` に `status=undelivered|delivered|all` と `limit` を追加する。
- `GET /gate/decisions` に `job_id`、`held_id`、`limit` のフィルタを追加する。
- `GET /meta` を追加する: 登録フォームの候補（hosts 名、agents 名、pools 名、gate_mode、evaluator_mode、プール残量サマリ）を返す。秘密情報（SSH 鍵パス、トークン、API キー）は含めない。

## 2. MCP（AI 向けの口）

### 2-1. 方式
- 同一バイナリのサブコマンド `quota-fleet mcp` として実装し、stdio トランスポートで話す。
- MCP 層は HTTP API のクライアントとして実装する（接続先とトークンは環境変数 `QUOTA_FLEET_API_URL`／`QUOTA_FLEET_TOKEN`）。サーバーと同一ホストで動かす場合も API 経由に統一し、store への直結経路は作らない。これにより、コーディネーターの実行環境（別 VM）からそのまま使える。
- プロトコル実装は公式 Go SDK（modelcontextprotocol/go-sdk）を第一候補とする。依存追加が問題になる場合は、tools/list と tools/call のみの最小 JSON-RPC 実装に切り替える（判断は実装時に記録する）。

### 2-2. ツール
| ツール | 対応 API | 備考 |
|---|---|---|
| `create_job` | POST /jobs | 202（保持）も正常結果として held_id と理由を返す |
| `list_jobs` | GET /jobs | 状態・agent・host・タイトル・親フィルタ |
| `get_job` | GET /jobs/{id} | 指示文・評価結果を含む |
| `cancel_job` | POST /jobs/{id}/cancel | draft／queued／waiting のみ |
| `requeue_job` | POST /jobs/{id}/requeue | |
| `mark_delivered` | POST /jobs/{id}/deliver | 回収完了の印（レビュー M4 で追加） |
| `list_held` | GET /gate/held | |
| `release_held` | POST /gate/held/{id}/release | 即時再評価による解放試行 |
| `cancel_held` | DELETE /gate/held/{id} | |
| `gate_decisions` | GET /gate/decisions | job_id 等のフィルタ可 |
| `list_deliveries` | GET /deliveries | status フィルタ可 |
| `fleet_status` | GET /status | |

### 2-3. MCP に出さないもの
- claim／complete／fail 等のランタイム内部操作は出さない（ワーカーとランタイムの専用であり、AI の誤操作面を広げない）。
- 実行中（claimed）ジョブのキャンセルは v1 の非目標とする（claim_token が必要な cancel-claimed はコーディネーターの非常用として CLI／API 直叩きに残す）。UI でも実行中ジョブにキャンセルを出さない。

## 3. 実施順序
1. **Phase UI-0（導入基盤）**（レビュー M2）:
   - `quota-fleet init`: 既存 fleet.yaml の上書きガード、暗号学的に安全なランダムトークンと instance_id の生成、ファイルパーミッション 0600、ローカルホスト＋主要エージェント（claude／codex／agy）の最小テンプレート出力。テンプレートの adapter は実動する構成（ssh／local transport）にし、mock を既定にしない。
   - 既定値の見直し: limits（max_claimed／attempts_cap）に既定値を持たせる。Evaluator は API キー未設定のとき起動バナーで明確に警告し、自由文条件が評価エラーになることを初回体験で踏ませない。
   - 起動バナー: 有効な設定（adapter、gate_mode、evaluator mode、notifier の有無、API 認証の有無、UI の URL）を起動時に一覧表示する。
   - `quota-fleet mcp config`: Claude Code／汎用 JSON のスニペット出力（トークンは環境変数参照の形で出し、値そのものは出力しない）。
   - README: ステータス表記の更新とクイックスタート（3手順）の執筆。
2. **Phase UI-1**: API 追加・拡張（§1-3）＋ UI（§1-2）。shadow に載せ、tailnet で開いて実地確認する。
3. **Phase UI-2**: MCP サブコマンドと §2-2 のツール。コーディネーター環境から登録→完了確認→mark_delivered までを通し、手作業の curl を置き換える。
4. 完了条件: 「ゆうやが UI から登録でき、コーディネーターが MCP で登録・回収・納品印までできる」ことの実地確認。あわせて init から最初のジョブ実行までを新規環境（まっさらなディレクトリ）で通す。

## 4. 非目標
- マルチユーザー、アカウント管理、権限分離はしない（トークン1本の運用のまま）。
- インターネット公開はしない。
- 作成後の指示文・タイトルの編集は作らない（title 不変の原則は UI でも維持）。
- 実行中ジョブのキャンセルは UI／MCP の v1 では扱わない（§2-3）。
- macOS／Windows ネイティブでのローカル serve は v1 の対象外（§0）。対応する場合は LocalTransport のシェル分岐、os: darwin の許可、setsid 非依存のデタッチ方式が別途必要。
- 通知のプッシュ化（Webhook をチャットへ繋ぐ）は本設計の範囲外とし、別途扱う。

## 付録: レビュー指摘への対応
| 指摘 | 対応 |
|---|---|
| M1 SSR と sessionStorage 認証の矛盾 | 案 A を採用し CSR（シェル＋fetch）に統一。XSS 対策と CSP を明記（§1-1） |
| M2 §0 が §1〜§3 に欠落、既定値と矛盾 | Phase UI-0 を新設し、init／既定値／バナー／mcp config／README を作業項目化（§3） |
| M3 API 実態との不一致 | フィルタ追加、deliveries の status、decisions のフィルタ、GET /meta、フォームへの worktree_spec／target_ref 追加を明記（§1-2、§1-3） |
| M4 mark_delivered 欠落 | ツールに追加。claimed キャンセルの扱いも明記（§2-2、§2-3） |
| M5 release の仕様未定義 | 「即時再評価による解放試行」と定義し応答契約を明記。GateChecker の注入も記載（§1-3） |
| M6 他 OS の実動作ギャップ | v1 のサポート境界を §0 と §4 に明記（serve は Linux／WSL2、他 OS はクライアントのみ） |
| R1〜R5（推奨） | 実装時に個別判断し、採否を実装記録に残す |
