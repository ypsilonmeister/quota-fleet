# docs/12 API トークンの権限分離 設計

状態: 設計（2026-11-24、ゆうや承認）。実装は本書を正とする。

## 1. 目的

現行の API 認証は単一の bearer トークン（`api.token`）で、保持者は全操作ができる。MCP 経由で他の AI にも fleet を使わせるにあたり、相手ごとに必要最小限の権限だけを渡せるようにする。skill（使い方の作法）は正しい利用を促す層であり、安全の担保は本書のサーバー側の権限分離が担う。

## 2. 設計

### 2-1. 設定形式

```yaml
api:
  token: "<従来の単一トークン>"   # 後方互換。全スコープを持つ name=legacy として扱う
  tokens:
    - name: coordinator           # 監査用の識別名。ジョブや操作の記録に残る
      token: "<値>"
      scopes: [read, register, operate, worker, agents]
    - name: watch
      token: "<値>"
      scopes: [read]
    - name: mcp-register
      token: "<値>"
      scopes: [read, register]
```

- `api.tokens` が空なら従来どおり `api.token` のみで動く（既存の本番・自動化は無変更で継続）。
- トークン値の比較は従来どおり constant-time で行う。値そのものはログ・応答に出さない。
- 名前の重複、スコープ名の typo は起動時にエラーにする（黙って読み飛ばさない）。

### 2-2. スコープとエンドポイントの対応

| スコープ | 対象 |
|---|---|
| read | すべての GET（/jobs、/jobs/{id}、/status、/meta、/deliveries、/gate/held、/gate/decisions、/agents、/agents/events） |
| register | POST /jobs |
| operate | POST /jobs/{id}/cancel、/requeue、/reset、/deliver、POST /gate/held/{id}/release、DELETE /gate/held/{id}、DELETE /gate/cooldowns/{pool}、POST /agents/check、POST /decision/review-verdict（[docs/18](18-review-gate.md)） |
| worker | POST /jobs/{id}/claim、/complete、/fail、/cancel-claimed、/reject-invalid（実行系の内部用。MCP には公開しない） |
| agents | 将来のエージェント管理用に予約（当面は operate に含まれる /agents/check のみ） |

- `POST /decision/review-verdict` は匿名では使えない。トークン未設定の場合は `require_auth: false` でも 401（[docs/18](18-review-gate.md)）。
- 認証なし（トークン不一致・欠落）は従来どおり 401。認証は通ったがスコープ不足は 403 とし、不足したスコープ名だけを返す（トークンの情報は返さない）。
- UI（/ui/*）は従来どおり認証対象外の静的配信とし、UI からの API 呼び出しは利用者が持つトークンで行う。/meta の応答に、そのトークンのスコープ一覧を含め、UI ができない操作を無効化できるようにする。

### 2-3. 監査

- POST /jobs で作成されたジョブに `created_by`（トークンの name）を記録し、GET の応答に含める。既存ジョブは空でよい（移行時の埋め戻しはしない）。
- Gate の解放・キャンセル等、操作の記録に actor 欄がある箇所では同じ name を残す。欄が無い箇所は本対応の範囲外とする。

## 3. 移行手順（本番）

1. 実装とテストをマージし、デプロイする（この時点では `api.tokens` 未設定で従来動作）。
2. 本番 fleet.yaml に名前付きトークンを追加する: coordinator（全スコープ、コーディネーター用）、watch（read、見守り cron 用）、mcp-register（read＋register、他の AI の MCP 用）。値はサーバー上で生成し、設定ファイル以外に残さない。
3. 見守り cron とコーディネーターの利用を名前付きトークンへ切り替え、動作を確認する。
4. 従来の単一トークン（legacy）は移行完了まで残し、切り替え確認後にゆうやの判断で廃止する。

## 4. テスト要件

- スコープ×エンドポイントの表テスト（許可・403 の両方）。
- `api.tokens` 未設定時に従来の単一トークンで全操作できる後方互換テスト。
- `created_by` がジョブに記録され、GET で返ること。
- 不正なスコープ名・名前重複で起動時エラーになること。
- /meta にスコープ一覧が含まれること。

## 5. MCP との関係

MCP サーバー側の変更は不要。接続先ごとに `QUOTA_FLEET_TOKEN` へ渡すトークンを変えるだけでよい。推奨の渡し方:

- 状態を見るだけの AI: read
- ジョブ登録まで任せる AI: read＋register
- 運用操作（キャンセル・解放・納品）まで任せる相手: operate を追加
- worker スコープは外部の AI には渡さない
