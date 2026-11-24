# docs/13 — MCP の Streamable HTTP 提供（他 AI からの利用）

- 状態: 設計（2026-11-25 ゆうや承認、後者案＝HTTP 提供で進める）
- 前提: docs/07 §2（MCP、stdio 版は Phase UI-2 で実装・通し確認済み）、docs/12（API トークン権限分離）

## 1. 目的

現行の MCP は `quota-fleet mcp` サブコマンド（stdio）として手元で起動する形のため、利用できるのはバイナリとトークンを配布した環境に限られる。他の AI（各ホストの agy／Claude Code／Codex）が、バイナリ配布なしに quota-fleet を操作できるよう、fleet サーバー自身が Streamable HTTP の MCP エンドポイントを提供する。

非目的: インターネットへの公開（tailnet 内利用に限る）、実行系ツール（claim／complete／fail 等）の公開、stdio 版の廃止。

## 2. 方式

- エンドポイント: 既存 serve と同一ポートに `POST /mcp`（および同ハンドラが要求する GET／DELETE）を追加する。トランスポートは MCP Streamable HTTP（公式 Go SDK `github.com/modelcontextprotocol/go-sdk` の `NewStreamableHTTPHandler` を使う。stdio 版と SDK・ツール実装を共有する）。
- ハンドラはリクエストごとにサーバーを構築する形（SDK の getServer 関数形式）とし、**リクエストの Bearer トークンで API クライアントを束ねる**。ツール実行は従来どおり HTTP API 経由とし、スコープ強制（docs/12）と `created_by` の記録は API 側の既存経路をそのまま通す。トークンを束ね損ねて全員が同一 identity になる構成（単一トークンの固定束ね）は禁止する。
- 認証なしリクエストは 401。スコープ不足は従来の API の 403 をツールエラーとして返す（stdio 版と同じ見え方）。
- セッションはステートレスを基本とする（SDK がステートレスを許す範囲）。セッションを使う場合も、トークンの束ねはリクエスト単位で検証し直すこと。

## 3. セキュリティ

- 公開範囲は serve の待ち受け範囲そのまま（本番は tailnet IP）。funnel 等での外部公開はしない。
- MCP 仕様の DNS リバインディング対策として、Origin ヘッダの検証を SDK の既定／推奨設定で行う（localhost と tailnet ホスト名を許可する範囲に留める）。SDK v1.8.0 の既定はループバック着信時の Host 検査のみのため、`/mcp` 側で Origin を検査する。Origin なしは通し、Origin 付きはホストがループバックか `api.mcp_allowed_origin_hosts` にある場合だけ通す（それ以外は 403）。
- `/mcp` は常に Bearer トークン必須。トークン未設定の場合は `require_auth: false` でも 401（JSON API の従来動作は変えない）。
- ログにトークン値を出さない。呼び出し元 identity はトークンの `name` を監査に残す（API 側の既存記録で足りる場合は追加実装しない）。

## 4. 設定

- `fleet.yaml` に `api.mcp_http`（bool、既定 false）を追加し、有効化は明示設定のみ。無効時は `/mcp` を登録しない（404）。
- `api.mcp_allowed_origin_hosts`（文字列の配列、既定は空）に、Origin を許可する tailnet ホスト名を書く（スキーム・ポート・ワイルドカードは不可）。
- `quota-fleet mcp config` に HTTP 形式の接続スニペット出力を追加する（URL と環境変数参照のトークン。トークン値そのものは出力しない）。

## 5. ツール面

- stdio 版と同一の12ツール（create_job／list_jobs／get_job／cancel_job／requeue_job／mark_delivered／list_held／release_held／cancel_held／gate_decisions／list_deliveries／fleet_status）。ツール定義の正本は現行 `internal/mcpserver` とし、HTTP 側に複製を作らない。

## 6. テストと受け入れ

- ハンドラのテスト: initialize／tools/list が通ること、無認証が 401、read 専用トークン（watch 相当）で create_job がスコープ不足として失敗すること、register 可能なトークンでは作成したジョブの `created_by` がそのトークンの name になること。
- 既存テスト（stdio 版・API）がすべて従来どおり通ること。
- 実地確認（マージ後のデプロイ時にコーディネーターが行う）: 本番で `api.mcp_http: true` を有効化し、別ホストのクライアントから initialize と list_jobs が通ること。

## 7. 段階

1. 本設計の実装（エンドポイント、設定、config スニペット、テスト）。
2. マージ後にデプロイし、本番で有効化して実地確認する（デプロイは従来の deploy-local ジョブ）。
3. 各ホストのエージェントへの接続設定は、有効化の確認後にホストごとに進める（本設計の範囲外）。
