# 21 ジョブ単位の effort 指定

Issue #27 の実装メモ。ジョブ登録時に推論の深さ（effort）を任意で指定し、エージェントの起動コマンドに渡す。
実装: `internal/store/effort.go`（検証）、`internal/adapter/driver.go` の `effortArgs`（起動コマンド）、`internal/api` の `POST /jobs`、MCP `create_job`。

## 1. 値
- 登録フィールド `effort`（任意）: `low` | `medium` | `high` | `xhigh`。
- 前後の空白は除いて扱う。大文字（`HIGH`）や上記以外（`max`、`minimal`、`none` など）は不正値として **400**。
- `max` は CLI 側（codex / claude）が受け付けるが、fleet では受理しない（運用で使わない段のため）。

## 2. 保存と応答
- ジョブの `effort` 列（TEXT、既定 `''`）に保存する。既存 DB は起動時に列を追加する（既存行は `''`）。
- `GET /jobs`、`GET /jobs/{id}`、`POST /jobs` の応答の `job.effort` に出す。未指定のジョブでは **キー自体を出さない**（`omitempty`。従来の応答形のまま）。
- Quota Gate が保持（202）したとき、保持 payload（`held.payload.effort`）に入る。解放時は payload からジョブを作るので effort はそのまま引き継がれる。

## 3. エージェント別の扱い

| agent | effort 指定時 | 起動コマンドへの反映 |
|---|---|---|
| codex | 受理 | `-c model_reasoning_effort=<値>` を起動テンプレートの後ろ（sandbox_flag の前）に付ける |
| claude | 受理 | `--effort <値>` を同じ位置に付ける。resume（`--resume`）でも付ける（model は従来どおり resume で外す） |
| agy | **400** | effort はモデル ID の一部（例: `gemini-4-neon-medium`）で、二重指定は曖昧になるため |
| 上記以外・agent 未定 | **400** | 渡す手段が無いため。黙って捨てない |

- 判定するエージェント名は、ジョブの `agent` そのもの（`claude` / `codex` / `agy`）。fleet.yaml で別名のエージェントを定義していても、名前が上の 2 つでなければ effort は 400。
- 起動テンプレート（fleet.yaml の `launch` / `resume`）を上書きしていても、effort の引数は自動で付く。テンプレートにプレースホルダは要らない。
- Windows（agent-run.cmd）でも同じ引数を付ける（`=` を含むため `"model_reasoning_effort=<値>"` と引用される）。

### 3.1 CLI の確認（2026-11-26、ubuntu 上のプローブ）
- codex-cli 0.169.1: `codex exec --help` に `-c, --config <key=value>` がある。`-c model_reasoning_effort=low` で実行すると起動ヘッダに `reasoning effort: low` と出て応答が返った。`-c model_reasoning_effort=bogus` ではヘッダに `bogus` と出たうえで API が `invalid_enum_value`（受理値: none, minimal, low, medium, high, xhigh, max）で拒否した。値が API まで届いていることはこれで確認できる。
- Claude Code 2.1.331: `claude --help` に `--effort <level>  Effort level for the current session (low, medium, high, xhigh, max)` がある。`claude -p --effort low` は正常終了した。ただし `--effort bogus` も **エラーにならず** に実行された（CLI は値を検証しない）。どの段で推論したかは CLI の出力からは確認できなかった。よって fleet 側の値検証（§1）が唯一の歯止め。
- fleet の各ホスト（ubuntu-vm、home-win など）の CLI バージョンが `--effort` / `-c` を受けるかは未確認。古い CLI は未知のフラグとしてエラー終了し、ジョブの失敗として表に出る想定（この挙動自体は未検証）。

## 4. 検証の順序
1. Selector（docs/19）が空の agent / model / pool を埋める。**Selector は effort を選ばない・変えない**（登録者の指定をそのまま使う）。
2. その後の **実際に実行される agent** で effort を検証する。agent 未指定でも Selector（record の既定や apply の選択）が codex / claude を埋めれば受理、agy を選べば 400。
3. 検証を通ったものだけが Gate 判定・保持・ジョブ作成に進む。ストア層（`CreateJob` / `HoldJob`）でも同じ検証をしている（CLI `job create` 経由や保持解放でも不正値が入らない）。

## 5. 未指定時の互換
- `effort` を送らない／空文字のときは、検証も起動コマンドも従来と完全に同じ（引数を一切足さない。エージェントの既定＝ホストの設定に従う）。
- ホスト側の `~/.codex/config.toml` などは変更しない（対話利用を巻き込まない）。

## 6. MCP
- `create_job` ツールの入力に `effort`（任意、文字列）を追加した。値はそのまま `POST /jobs` に渡り、検証は API 側で行う（400 はツールのエラーとして返る）。

## 7. 範囲外
- effort の自動選択（Selector の拡張）。
- UI での表示・入力。
- CLI `quota-fleet job create` のフラグ（今回は追加していない）。
