# 21 ジョブ単位の effort 指定

Issue #27 の実装メモ。ジョブ登録時に推論の深さ（effort）を任意で指定し、エージェントの起動コマンドに渡す。
実装: `internal/store/effort.go`（検証）、`internal/adapter/driver.go` の `effortArgs`（起動コマンド）、`internal/api` の `POST /jobs`、MCP `create_job`。

## 1. 値
- 登録フィールド `effort`（任意）: `low` | `medium` | `high` | `xhigh`。
- 前後の空白は除いて扱う。大文字（`HIGH`）や上記以外（`max`、`minimal`、`none` など）は不正値として **400**。
- `max` は CLI 側（codex / claude）が受け付けるが、fleet では受理しない（運用で使わない段のため）。

## 2. 保存と応答
- ジョブの `effort` 列（TEXT、既定 `''`）に保存する。既存 DB は起動時に列を追加する（既存行は `''`）。
- 応答での位置（未指定のジョブでは **キー自体を出さない**。`omitempty` で従来の応答形のまま）:
  - `POST /jobs`（201）: `job.effort`
  - `GET /jobs`: `jobs[].effort`
  - `GET /jobs/{id}`: トップレベルの `effort`
- Quota Gate が保持（202）したとき、保持 payload（`held.payload.effort`）に入る。解放時は payload からジョブを作るので effort はそのまま引き継がれる（`internal/runtime` の自動解放テストで、解放後のジョブの effort を確認している）。

## 3. エージェント別の扱い

| agent | effort 指定時 | 起動コマンドへの反映 |
|---|---|---|
| codex | 受理 | `-c model_reasoning_effort=<値>` を起動テンプレートの後ろ（sandbox_flag の前）に付ける。テンプレートに `--` があればその直前に入れる |
| claude | 受理 | `--effort <値>` を同じ位置に付ける。resume（`--resume`）でも付ける（model は従来どおり resume で外す） |
| agy | **400** | effort はモデル ID の一部（例: `gemini-4-neon-medium`）で、二重指定は曖昧になるため |
| 上記以外・agent 未定 | **400** | 渡す手段が無いため。黙って捨てない |

- 判定するエージェント名は、ジョブの `agent` そのもの（`claude` / `codex` / `agy`）。fleet.yaml で別名のエージェントを定義していても、名前が上の 2 つでなければ effort は 400。
- 起動テンプレート（fleet.yaml の `launch` / `resume`）を上書きしていても、effort の引数は自動で付く。テンプレートにプレースホルダは要らない。
- テンプレートにオプション終端 `--` があるとき（例: `codex exec --skip-git-repo-check --`）、effort の引数は **最初の `--` の直前** に入れる。`--` より後ろは位置引数として扱われ、codex-cli 0.169.1 では `codex exec ... -- -c model_reasoning_effort=low` が `unexpected argument 'model_reasoning_effort=low'` で失敗した（`-c ... --` の順なら `reasoning effort: low` で実行された）。
  - Linux（run.sh）ではテンプレートのトークンをそのままシェルに渡すので、`--` かどうかは **シェルが分ける語の単位で、引用除去後の値** で判定する。引用符やバックスラッシュでエスケープした空白は複数のトークンにまたがるため、引用状態をコマンド全体で追跡して語にまとめてから判定する。`codex exec --add-dir '/tmp/a -- b' "--"` では引用内の `--` は終端ではなく、effort は最後の `"--"` の直前に入る（`--add-dir` の値は `/tmp/a -- b` のまま）。`'--'`、`"--"`、`\-\-` も終端として扱い、その直前に入れる（`codex exec --skip-git-repo-check '--'` で `-c` が後ろに付くと codex-cli 0.169.1 / 0.171.0 は `unexpected argument` で失敗した）。`$` やバッククォート、グロブ文字を含むトークンは実行時まで値が決まらないので終端とみなさない。
  - Windows（agent-run.cmd）はテンプレートのトークンを値として引用し直すため、トークンが `--` そのものの場合だけが終端になる（`'--'` はリテラルの `'--'` として渡る）。
- **登録時に実行先で渡せるかを確かめる**。`POST /jobs` は fleet.yaml からジョブの `host` と agent のドライバ（`launch` / `resume` テンプレートとホスト別 `command` 上書き）を解決し、実際に起動する実行ファイルがその agent 自身の CLI でなければ **400** にする。判定は実行ファイルのベース名（ディレクトリと Windows の拡張子 `.exe` / `.cmd` / `.bat` / `.ps1` を除く）が agent 名（`claude` / `codex`）と一致するか。`/home/u/.local/bin/claude` や `C:\...\npm\claude.cmd` は受理、`/opt/bin/run-agent.sh` のようなラッパーや別名のバイナリは、`--effort` / `-c` を受けるか分からないので拒否する。effort 未指定のジョブはこの判定をしない。
  - fleet.yaml を読み込んでいない API サーバー、または `host` が fleet.yaml に無いジョブではこの判定を省く（ホスト未定義は起動時に別途失敗する）。
  - 続けて、サーバーが実ホストに届く構成（`runtime.adapter: ssh`）では **ジョブの host 上でその CLI に `--help` を実行** し、ヘルプに effort の指定手段（claude は `--effort`、codex は `-c, --config`）があるかを確かめる。無ければ **400**（例: `/opt/legacy/bin/claude` のように名前は claude でも `--effort` を持たない古い CLI）。実行ファイルが host に無い場合も 400。
  - host に接続できない・プローブがタイムアウトした（15 秒）など、判定できなかったときは **503**（受理も「非対応」の断定もしない）。
  - 判定が付いた結果（対応・非対応）は (host, agent, 実行ファイル) ごとに 10 分間再利用する。接続失敗は記憶しない。
  - 実行ファイルが **相対パス**（`./claude`、`bin/claude`、`.\claude.exe` など、区切り文字を含むが `/`・`~/`・ドライブ文字・UNC で始まらないもの）のときは、プローブせずに **400** にする。起動時はジョブの workdir に移動してから実行するため、どのファイルが起動されるかは workdir（worktree モードでは起動直前に作られる）で決まり、登録時のプローブ（host のログインディレクトリで実行）とは別のファイルを判定しうるため。絶対パスかコマンド名だけ（PATH で解決）を指定する。effort 未指定のジョブはこれまでどおり受理する。
  - mock アダプタ（実ホストが無い）ではこのプローブを行わず、上の構成上の判定だけをする（相対パスの拒否は構成上の判定なので mock でも行う）。
- Windows（agent-run.cmd）でも同じ引数を付ける（`=` を含むため `"model_reasoning_effort=<値>"` と引用される）。

### 3.1 CLI の確認（2026-11-26、ubuntu 上のプローブ）
- codex-cli 0.169.1: `codex exec --help` に `-c, --config <key=value>` がある。`-c model_reasoning_effort=low` で実行すると起動ヘッダに `reasoning effort: low` と出て応答が返った。`-c model_reasoning_effort=bogus` ではヘッダに `bogus` と出たうえで API が `invalid_enum_value`（受理値: none, minimal, low, medium, high, xhigh, max）で拒否した。値が API まで届いていることはこれで確認できる。
- Claude Code 2.1.331: `claude --help` に `--effort <level>  Effort level for the current session (low, medium, high, xhigh, max)` がある。`claude -p --effort low` は正常終了した。ただし `--effort bogus` も **エラーにならず** に実行された（CLI は値を検証しない）。どの段で推論したかは CLI の出力からは確認できなかった。よって fleet 側の値検証（§1）が唯一の歯止め。
- 登録時のプローブ（§3）は上記のヘルプ行で判定する。codex-cli 0.169.1 の `codex --help` に対してプローブを実行し、対応と判定されることを確認した。claude の判定はテスト用の偽 CLI（`--effort <level>` 行の有無）で確認しており、実機の Claude Code に対しては未実行。

## 4. 検証の順序
1. Selector（docs/19）が空の agent / model / pool を埋める。**Selector は effort を選ばない・変えない**（登録者の指定をそのまま使う）。
2. その後の **実際に実行される agent** で effort を検証し、続けてジョブの host でその agent の CLI に渡せるか（§3）を確かめる。agent 未指定でも Selector（record の既定や apply の選択）が codex / claude を埋めれば受理、agy を選べば 400。
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
