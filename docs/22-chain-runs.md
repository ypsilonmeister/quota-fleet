# 22 チェーン実行（Chain Driver, C1）

Issue #26 の実装メモ。正本は設計ドラフト v2（Issue #26 コメント issuecomment-7180863188）。本書はそのうち **C1**（v2 §2〜§9、§11、§13）として実装した内容と、実装で決めた細部・v2 との差分を書く。

- 実装: `internal/chain`（定義・テンプレート展開・分岐関数・verdict 検証）、`internal/admission`（Selector→Gate の判定の切り出し）、`internal/store/chain.go`・`held_release.go`（永続化と 1 トランザクションの遷移）、`internal/runtime/chain_driver.go`（定期スイープ）、`internal/runtime/shepherd_verdict.go`（verdict の取り込み）、`internal/api/chains.go`、`cmd/quota-fleet/chain.go`。
- §15 の未決事項はすべて決定済み（2026-11-26 ゆうや承認）: `stall_after` 既定 24h・停滞は報告のみ／§3.4 の既存ジョブへの接続は C1 に入れない／publish の `reviewed_commit` 確認は工程側の責務／`POST /chains/{id}/cancel` は作成者にも許可。
- C1 に入れていないもの: resume、Decision 1 の並走、イベント起床、§3.4 の接続（`start: review|fix`・`start_after_job_id`）、supersede の API、任意の工程グラフ。既存の `parent_id` による昇格・波及キャンセルはそのまま（併存）。

## 1. 形と終了性

```
implement ──done──▶ review ──ok_to_merge──▶ publish ──done──▶ completed
                      ▲  │
                      │  └──changes_needed──▶ fix ──done──▶ review
                      └────────────────────────────────────┘
```

- `round` は「何回目のレビューか」。最初の review が 1、fix を作るたびに +1（その後の review は同じ round）。
- `changes_needed` で `round + 1 > max_rounds` なら fix を作らず `stopped: max_rounds_exceeded`。
- 工程予算 `max_steps = 2 × max_rounds + 1` を run に保存し、seq がこれを超える生成は分岐関数とトランザクションの両方で拒否して `stopped: step_budget_exceeded`（固定形では到達しない二重の歯止め）。
- よって review ≤ `max_rounds`、fix ≤ `max_rounds − 1`、implement・publish は各 1 回。`max_rounds` = 1 / 3 / 10 で常に `changes_needed` を返すと、工程数は 2 / 6 / 20（上限 3 / 7 / 21）で止まる（`TestChainTerminationMaxRounds`）。

## 2. 登録（`POST /chains`、スコープ `register`）

```yaml
request_id: qf-26-chain-1        # run 用の冪等キー
title: "#26 チェーン自己駆動化"
scope: ypsilonmeister/quota-fleet#26   # 表示・検索用
max_rounds: 3                    # 省略時 3。chains.max_rounds_cap（既定 10）を超えると 400
start: implement                 # C1 は implement だけ（review / fix は 400）
workspace: {host: ..., workdir: ..., worktree_spec: ...}   # workdir は必須。全工程で同じ workdir を使う
steps:
  implement: {title: ..., agent: codex, model: ..., instructions: ..., acceptance_criteria: ...}
  review:    {title: ..., agent: claude, instructions: "... {{verdict.path}} ... {{verdict.nonce}} ...", ...}
  fix:       {...}
  publish:   {...}
```

- 工程の雛形に書けるのは `title`・`agent`（必須）・`model`・`pool`・`alt_pools`・`instructions`・`acceptance_criteria`・`target_ref` だけ。それ以外のキー（`request_id`・`parent_id`・`draft`・`workdir` など）は **400**（未知キーを拒否するデコード）。
- `workspace.workdir` は必須（`worktree_spec` だけの定義は 400）。workdir が空のジョブは Adapter が `WorkdirRoot/job-<id>` を工程ごとに割り当てるため、`worktree_spec` だけでは工程ごとに別の worktree になり、implement の変更を review が見られない。`worktree_spec` を併記した場合、最初の工程が workdir に worktree を作り、後続の工程は既存の worktree（`<workdir>/.git`）をそのまま使う。
- 変数は `{{round}}`・`{{prev.job_id}}`・`{{run.id}}`・`{{run.scope}}`、review だけ `{{verdict.path}}`・`{{verdict.nonce}}`、fix と publish だけ `{{verdict.reviewed_commit}}`。未知・その工程で使えない変数は登録時に 400（生成時も同じ `chain.Expand` を通る）。
- **review の instructions には `{{verdict.path}}` と `{{verdict.nonce}}` が必須**（v2 に無い追加の検証。書けないと必ず `verdict_unavailable` で止まるため）。
- 上限: 本文 64 KiB、`instructions` 1 本 32 KiB。
- 正規化した JSON と SHA-256（`definition_hash`）、`definition_version: 1` を保存し、以後変えない。`request_id` が既存で hash と owner（登録トークン名）が同じなら既存を 200、どちらかが違えば 409。
- run・seq=1 の `chain_steps`・最初の工程の `jobs` 行（または `held_jobs` 行）・`gate_decisions` を **1 トランザクション**で書く（`Store.CreateChainRun`）。admission が要求を拒否したら 400。

## 3. 工程キーと予約接頭辞

- 工程のジョブ・保持の `request_id` は `chain:<run_id>:<seq>`。分岐名や round を含めないので、同じ seq に 2 つの工程はできない（`chain_steps` の主キー `(run_id, seq)` と `request_id` の UNIQUE）。
- `chain:` は予約。`POST /jobs` は 400、Store の `CreateJob`・`HoldJob`（CLI・MCP もここを通る）も `ErrInvalidInput`。起動時の移行で、チェーン外の `jobs` / `held_jobs` に `chain:` で始まるキーがあれば **起動を拒否**する。
- 工程を作るトランザクションの中で、同じキーが `jobs` にも `held_jobs` にも無いことを確かめてから挿入する（F12 の UNIQUE が別々の問題）。

## 4. 原子的な工程確定（`Store.AdvanceChain`）

Driver は 1 run について:

1. トランザクションの外で `LoadChainSnapshot`（run・現在工程・そのジョブ/保持・取り込み済み verdict）を読み、`chain.Decide` で分岐を仮決めする。次工程を作るなら雛形を展開し、`admission.Decide`（Selector→Gate、POST /jobs と同じ関数）を評価する。review の nonce（128 ビット）はここで作る。
2. `BEGIN IMMEDIATE` の中でスナップショットを読み直し、**指紋**（run の status・revision・current_seq・round、工程の job_id/held_id、ジョブの状態、保持の状態、verdict の `raw_sha256`）が手順 1 と一致し、同じ分岐関数が同じ結論を返すことを確かめる。一致しなければ何も書かない（`Skipped`。次のスイープで読み直す）。
3. 結果に応じて、次工程（`chain_steps(S+1)` + `jobs` 行〔queued、`parent_id` = 前工程、Outbox〕または `held_jobs` 行 + `gate_decisions`）とポインタ更新（`current_seq`・`round`・`revision+1`）、完了（`completed`）、停止（`stopped` + 通知行）、R10 の紐付けを書く。どれも `WHERE revision = R` 付き。

| 反例（v2 §4.3） | 固定したテスト |
|---|---|
| 2 つの実行が同じ完了を読み、片方は保持、片方は実ジョブを作る | `TestChainDoubleDriveCounterexamples/one execution holds, the other creates a job`（両方の順序。負けた側の行は無く、Launcher も claim できない。再起動後も工程は増えない） |
| 2 つの実行が異なる verdict を読む | `.../the verdict changes between read and commit`（照合で負け、次のスイープは DB の値で分岐）。`.../the verdict bytes change with the same verdict value` は分岐が同じで `raw_sha256` だけが変わる場合で、指紋の照合だけを外すと落ちる |
| ポインタだけ進んで工程が欠落する | `.../a failure inside the transaction leaves neither pointer nor step`（同じキーの保持行を割り込ませると、ポインタも工程もジョブも残らない。行を消すと収束） |
| cancel の後に古い実行が進める | `.../cancel between read and commit` |
| 初回作成で run だけ・ジョブだけが残る | `.../initial registration is one transaction and idempotent` |
| 書き込みの途中（ジョブ/保持・Outbox・Gate 記録・工程履歴の後、ポインタ更新の後、停止と通知行の後、解放のリンク後、初回作成の後）で失敗する | `TestChainFaultAfterWritesRollsBackEverything`（`Store.SetChainFaultHookForTest` で各点にエラーを注入し、全テーブルの件数と run の行が注入前と一致すること、フックを外すと収束することを確認） |
| 2 つのプロセス（別々の書き込み接続）が同じ run を駆動する | `TestChainDoubleDriveTwoConnections`（同じ DB ファイルに 2 つの Store を開き、片方の読み取りとコミットの間に他方を割り込ませる決定的な順序と、15 回の同時実行。seq 2 の工程・ジョブ・Outbox がそれぞれ 1 行） |

指紋の照合とトランザクション内の再判定は二重の歯止めで、片方だけを外しても上の多くの反例は他方で止まる。両方を外すと保持/実ジョブの二重駆動と verdict の反例が落ちることを確認した。

## 5. 保持の解放と取消（`Store.ReleaseHeld` ほか）

- GateChecker の定期解放と `POST /gate/held/{id}/release` は、判定（トランザクション外）の後に `ReleaseHeld` を呼ぶ。中で held が `held` であること、チェーンなら run が running で現在工程がこの保持であることを照合し、ジョブ挿入（または同じ `request_id` の既存ジョブの採用）・`released`・`chain_steps.job_id`・`revision+1`・`gate_decisions` を 1 トランザクションで書く。**チェーン外の保持もこの経路になった**（`CreateJob` と `MarkHeldReleased` が別トランザクションだった F7' を解消）。
- run が終端なら保持を `cancelled`（`cancel_reason = 'chain <status>'`）にしてジョブは作らない。
- 親が無い・終端・入力不正などの**恒久的な**エラーは held の `last_error` に記録し、チェーンなら同じトランザクションで `stopped: release_failed` と通知行を書く。枠待ち（not releasable）は `ReleaseHeld` に来ないので区別される。一時的な DB エラーはロールバックし、チェーン外の保持では従来どおり `last_error` に記録、チェーンの保持では記録しない（`last_error` は R9 で「恒久」と解釈するため）。
- `CheckOnce` はキーセットで全ページを巡回する（先頭 200 件が枯渇で残っても後ろを判定する）。
- `POST /chains/{id}/cancel`: run を `cancelled`。現在工程が held なら `cancelled`（`chain cancelled`）、draft/queued/waiting のジョブなら既存の `Cancel` と同じ書き込み（波及を含む）。解放済みで `chain_steps.job_id` がまだ NULL（R10 の状態）なら、解放先のジョブを同じ扱いにし、同じトランザクションでそのジョブを工程に紐付ける（`chain_steps.job_id` と `jobs.chain_run_id`・`chain_seq`、`CancelChainRunResult.LinkedJobID`）。取消時に claimed だった未所属ジョブも、これで後の `Reset` が終端 run の工程として claimed → cancelled にし、queued に戻らない。対象のジョブと保持は §6 の所属検証を通ったものだけで、通らなければ触らない（`CancelChainRunResult.Inconsistency`）。別 run・別 seq の保持（`held_jobs.chain_run_id`・`chain_seq` の不一致）は held でも cancelled にしない。**claimed のジョブは止めず**、応答の `running_job_id` と `warning` で知らせる。冪等（既に cancelled なら 200 と `already_cancelled: true`）。operate、または run を登録したトークンに許可。
- `DELETE /gate/held/{id}`: `cancel_reason` は本文の理由（#31 と同じく前後の空白を除き上限で切る。なければ空）。チェーンの現在工程なら同じトランザクションで `stopped: step_cancelled`（`stop_detail` は `held <id> cancelled via gate API[: <理由>]`）と通知行。
- `POST /jobs/{id}/requeue` はチェーンのジョブに **409**（`chain-managed job; register a new chain run`）。worker の complete/fail などは従来どおり。
- `Store.Reset`（Reconciler の自動 reset と `POST /jobs/{id}/reset`）は、run が running ならチェーンのジョブも従来どおり queued に戻す。run が終端（cancelled / stopped / completed）なら、**同じトランザクションで claimed → cancelled** にし（`last_error = chain run is <status>; not reset to queued`）、再び claim できる状態にしない。Reconciler はこの場合 `stale_claimed_cancelled` を報告する（`TestChainResetAfterCancelDoesNotRequeue`）。

`TestChainReleaseHeldVersusCancel` が「取消が先」「解放が先」「同時実行 25 回（不変条件: cancelled の run に生きたジョブが残らない）」「claim 済みを止めない」「DELETE /gate/held」「停止済み run の保持の解放」「恒久エラーと枠待ちの区別」を、`TestGateCheckerCheckOnceWalksAllPages` が 200 件超の巡回を固定する。

## 6. 定期スイープ（`ChainDriver.SweepOnce`）

起動直後に 1 回、その後 `chains.sweep_interval`（既定は `runtime.shepherd_interval`）ごと。`status = 'running'` の run を `(created_at, id)` のキーセットで 200 件ずつ全ページ巡回し、`next_check_at` が未来の run は飛ばす。1 run のエラーは記録して `next_check_at` を指数的に延ばし（上限 30 分）、他の run は止めない。

| # | 現在工程の実体 | 動作 |
|---|---|---|
| R1 | ジョブ queued / claimed / waiting / draft | 待つ。工程の作成から `chains.stall_after`（既定 24h）を超えたら `chain_stalled` を revision ごとに 1 回報告（停止しない） |
| R2 | done、implement / fix | review を作る |
| R3 | done、review、verdict captured | `ok_to_merge` → publish、`changes_needed` → fix または `max_rounds_exceeded` |
| R4 | done、review、verdict が missing / invalid / unavailable / 行なし / 別工程のもの | `verdict_unavailable` |
| R5 | done、publish | `completed`（`chain_completed` を通知） |
| R6 | failed | `step_failed`（自動リトライなし） |
| R7 | cancelled | `step_cancelled` |
| R8 | 保持 held、`last_error` なし | 待つ（R1 と同じ停滞報告） |
| R9 | 保持 held、`last_error` あり | `release_failed` |
| R10 | 保持 released で `chain_steps.job_id` が NULL | 解放先のジョブが存在し、保持と同じ `request_id` で、所属が未設定またはこの run・seq なら紐付ける。欠損・所属不一致は `internal_inconsistency` |
| R11 | 保持 cancelled | `step_cancelled` |
| R12 | 工程なし／job_id と held_id が両方 NULL／参照先なし／所属不一致 | `internal_inconsistency` |
| T1 | 終端の run の保持が `held` のまま | `cancelled`（`chain <status>`） |
| T2 | 未送信の通知 | 送る（§8） |

スイープの最後に `meta.chain_driver_last_sweep_at` を書く。`GET /status` の `chains` に run の件数・最終スイープ時刻・`driver_stale`（`3 × sweep_interval` 超）・未送信通知の件数と最古を出す。Reconciler は running の run があるのに最終スイープが `3 × sweep_interval` より古ければ `chain_driver_stale` を報告する（最終スイープ値ごとに 1 回）。

所属検証（`ChainSnapshot.OwnershipProblem`）は分岐関数・R10 の書き込み・取消が共有する。工程の保持自体が `chain_run_id`・`chain_seq` でこの run・seq に属すること（`HeldOwnershipProblem`、保持の状態によらず最初に確認する。別 run の未解放保持を正常な待機として扱わず、別 run の解放済み保持を経由した R10 の紐付けもしない）。工程のジョブは `chain_run_id`・`chain_seq` がこの run・seq であること。job_id と held_id を両方持つ工程（通常の解放後）は、保持が存在し `released` で `released_job_id` が工程のジョブと一致すること。R10 は上の表のとおり。

固定したテスト: `TestDecideRepairTable`（分岐関数の全行と所属検証）、`TestChainOwnershipOfReleasedJobs`（実 DB で R10 の他 run のジョブ・欠損ジョブ・保持と工程の食い違い・R10 状態での取消）、`TestChainCancelR10ClaimedThenReset`（R10 の未所属ジョブを claim → run 取消 → reset で cancelled になり再 claim されない）、`TestChainHeldOwnership`（別 run の保持を指す工程で Driver が停止、取消が他 run の保持・解放先ジョブを更新しない）、`TestChainSweepRepairTable`（実 DB で R1〜R12・T1・T2）、`TestChainSweepIsolatesBrokenRun`、`TestReconcilerReportsStaleChainDriver`。

## 7. verdict ファイル V1

1. review 工程を作るとき nonce を `chain_steps.verdict_nonce` に保存し、`{{verdict.path}}` = `.qf-chain/<run_id>/<seq>/verdict.json`（workdir からの相対パス、工程ごとに別）と `{{verdict.nonce}}` を instructions に展開する。
2. レビュアーは `{"schema":"qf.review-verdict/1","run_id":"…","seq":N,"nonce":"…","verdict":"ok_to_merge|changes_needed","reviewed_commit":"<40桁の小文字16進>"}` を書く。
3. **Shepherd** が受け入れ条件の評価に合格した後、同じ FileAccess で読む（`chain_run_id` を持つ review 工程だけ）。パスはサーバーが決めた相対パスで、途中のディレクトリを含めてシンボリックリンク（Windows は reparse point）を通らない通常ファイル・4 KiB 以下であること。検査と読み取りは `adapter.NoFollowReader.ReadFileNoFollow` の 1 操作で、リンク検査 → ファイルを開く（Unix は `O_NOFOLLOW|O_NONBLOCK`）→ 開いたまま経路を再検査 → 開いたファイルとパスが同じファイル（`os.SameFile`）であることを確認 → 開いたファイルから最大 4 KiB + 1 バイトだけ読む。途中で変わった場合は `invalid`（`changed while it was being read`）。リモートでも open 自体に制約をかける。**検査の範囲**: どの実装でも workdir 自体は信頼済みの起点とし、検査するのは workdir より下の各要素（途中のディレクトリと最終ファイル）。workdir のパスはオペレーターが設定する値（ホスト設定・チェーン定義の `workspace.workdir`）で、エージェントが書くのはその下だけ。workdir 自体やその祖先がリンク（Windows は symbolic link・junction）なら、ローカル（`lstatChain` は workdir の下から検査）・Linux（`cd` で解決）・Windows（`FILE_FLAG_OPEN_REPARSE_POINT` なしで開く）のいずれもそのまま辿る。Linux はシェル 1 コマンドで、途中のディレクトリへ `[ -L ]` 確認の後 `cd -P` で入り、`env pwd -P` が期待する物理パスであることを確かめて読み取り元のディレクトリを cwd として固定する。最終要素は python3（無ければ perl）の 1 プロセスが 1 つの FD で処理する。名前を lstat して通常ファイルであることを確かめ、`O_RDONLY|O_NOFOLLOW|O_NONBLOCK` で開く（検査後にリンクへ差し替えられても open が拒否し、FIFO へ差し替えられても open で待たない）。**読む前に**その FD を fstat し、通常ファイルで device・inode が lstat と同じことを確かめる。同じ FD から最大 4 KiB + 1 バイトをメモリへ読み、読み取り後に FD の fstat（mode・links・size・mtime・ctime。改名して戻すと ctime が変わる）が変わっていないこと、名前の lstat が同じ device・inode の通常ファイルであること、cwd が期待する物理パスのままであることを確かめる。ファイルの内容は全検査を通った後にだけ出力し、どこかで失敗すれば `CHANGED` だけを返す（検査に通らないファイルのバイトは出力しない）。python3 も perl もないホストでは、検査なしで読むことはせず exit 5 でエラーにする。Windows は C#（Add-Type）から `CreateFileW` を直接呼ぶ。workdir と途中のディレクトリは `FILE_SHARE_DELETE`・`FILE_SHARE_WRITE` なしで開いたまま保持する（改名・削除・差し替えを拒否）。workdir より下の各要素（途中のディレクトリとファイル）は `FILE_FLAG_OPEN_REPARSE_POINT` で開いて reparse point でないことを確かめる。workdir 自体は上記のとおり起点として扱い、この検査の対象外。ファイルも `FILE_FLAG_OPEN_REPARSE_POINT`・`FILE_SHARE_READ` だけで開き、ディレクトリ・reparse point でなく `GetFileType` がディスクファイルであることを確かめてからハンドルで上限付きで読む。読み取り後にパスを開き直し、ボリュームシリアル番号とファイル ID がハンドルと一致することを確かめる。JSON は値 1 つ・末尾データなし・キーの重複なし・未知キーなし・6 キーすべて、`run_id`・`seq`・`nonce` が工程と一致、`verdict` は 2 値、`reviewed_commit` は 40 桁。
4. 結果（`captured` / `missing` / `invalid` / `unavailable`）を `chain_artifacts` に `Complete` と**同じトランザクション**で書く（`CompleteWithArtifact`）。不合格で failed になるときは取り込まない（R6）。ホストに届かない等の一時エラーは評価エラーと同じ回数（`evaluator.max_eval_retries`）だけ再試行し、尽きたら `unavailable`。Adapter が FileAccess を持たない（mock）なら `unavailable`。
5. Driver は `chain_artifacts` だけを読み、ワーカーのファイルは読まない。

`TestParseVerdict` と `TestChainVerdictCapture` が「前ラウンドのファイルだけがある（missing）」「前ラウンドのファイルを今回のパスに複製（seq・nonce 不一致で invalid）」「seq だけ書き換えた複製（nonce 不一致）」「書きかけ」「キー重複」「未知キー・未知の値」「末尾データ」「4 KiB 超」「シンボリックリンク（ファイル・親ディレクトリ）」「ディレクトリ」（`TestReadFileNoFollowLocal` が「検査と open の間にリンクへ差し替え」「親をリンクにして open 後に戻す」「open 後に別ファイルへ置換」「FIFO」「読み取り上限」を、`TestLinuxNoFollowReadCommand` が Linux 用のシェルコマンドをローカルの sh で実行して通常・上限超過・欠損・リンクを固定し、`TestLinuxNoFollowReadRaces` が同じコマンドの検査と open の間・読み取り後に差し替えを注入して「FIFO へ差し替え（待たない）」「FIFO へ差し替えて元へ改名で戻す」「リンクへ差し替え」「別ファイルへ改名で差し替えて戻す」「読み取り中に改名して戻す」「親をリンクにして戻す（固定したディレクトリを読む）」「親をリンクのまま残す」「改行・NUL を含むデータ」を固定する。`TestWindowsNoFollowReadScript` は PowerShell の組み立て（二重引用符なし・長さ・フラグ・ID 比較）を確認する）「ホスト不通」「FileAccess なし」「手動 complete（R4）」「done の後の書き換えと再起動で判断が変わらない」を固定する。

`reviewed_commit` は publish・fix に `{{verdict.reviewed_commit}}` として渡すだけで、head との一致確認は工程側（指示と受け入れ条件）の責務（§15-3 決定どおり）。

## 8. 停止と通知

- 停止・完了は、その状態を書く同じトランザクションで `chain_notifications(run_id, revision, kind)` を挿入する。停滞報告も同じ表（`chain_stalled`、revision ごとに 1 回）。
- スイープ（T2）が `Notifier` の `Report` に `event_id = chain:<run_id>:<revision>:<kind>`、`chain_run_id`、`payload` を付けて送り、**送信が成功してから** `sent_at` をコミットする。送信後・コミット前に止まれば次のスイープで再送する（at-least-once、重複は `event_id` で除く）。
- 送信の成否を返すため `CheckedReporter`（`ReportChecked`）を追加した。`WebhookNotifier.Report` は従来どおり失敗を飲み込むが、`ReportChecked` はリトライ後の最終エラーを返す。失敗は `attempts`・`last_error`・`next_attempt_at`（指数、上限 1 時間）に記録する。
- `RecordReconcilerDedup` は使わない。
- 停止理由: `verdict_unavailable`、`step_failed`、`step_cancelled`、`max_rounds_exceeded`、`step_budget_exceeded`、`release_failed`、`admission_rejected`、`internal_inconsistency`。取消は `cancelled` で区別する。

## 9. API・CLI・設定

| エンドポイント | スコープ | 内容 |
|---|---|---|
| `POST /chains` | register | §2。201（作成）/ 200（同一の再登録）/ 400 / 409 |
| `GET /chains` | read | `status`・`scope`・`limit`・`cursor`（`<created_at>|<id>`、新しい順） |
| `GET /chains/{id}` | read | run、全工程（job/held の状態、`decided_branch`、取り込んだ verdict）、通知の送信状態 |
| `POST /chains/{id}/cancel` | operate または登録トークン | §5 |

- `GET /jobs/{id}`・`GET /jobs` に `chain_run_id`・`chain_seq`、`GET /gate/held` に `chain_run_id`・`chain_seq`・`cancel_reason`（いずれも未設定なら出さない）。
- CLI（HTTP の薄い層。`$QUOTA_FLEET_TOKEN`、`$QUOTA_FLEET_API_URL` または `--url`）: `quota-fleet chain create -f def.yaml`、`chain list`、`chain show <id>`、`chain cancel <id> [--reason]`。YAML は JSON に変換して送る。
- `fleet.yaml`:

```yaml
chains:
  sweep_interval: 5s      # 既定: runtime.shepherd_interval
  stall_after: 24h        # 既定 24h。報告のみ
  max_rounds_cap: 10      # 既定 10（1〜100）
```

## 10. admission の切り出し（`internal/admission`）

`POST /jobs` の Selector（docs/19）と Gate（docs/03）の判定を、書き込みをしない `Admission` に移した。`Decide` = `Prepare`（role/weight 検証・プール解決・Selector・effort 検証）+ `Gate`（off: create、dry-run: create + would-* 記録、enforce: 保持 or 選ばれたプールで create）。Driver は `Decide` を、`POST /jobs` は間に effort のホスト確認（docs/21 §3）を挟むため `ValidateRequest`→`Select`→`Gate` を個別に呼ぶ。判定の関数は同じで、`POST /jobs` の既存テスト（Selector・Gate・effort）は変更なしで通る。

## 11. v2 からの差分・実装で決めたこと

- スイープのキーセットは `(created_at, id)`（v2 は `(updated_at, id)`）。巡回中に `updated_at` が変わっても取りこぼし・重複が起きないため。
- review の nonce は Driver が admission の前に作り、確定トランザクションで保存する（v2 は「トランザクションの中で」）。コミットされなかった実行の nonce はどこにも残らないので、意味は同じ。
- `chain_completed` の通知行を完了時にも書く（v2 は停止時のみ明記）。
- `chain_runs.check_failures` を追加（`next_check_at` の指数バックオフ用）。
- `held_jobs.cancel_reason` は Issue #30 の列を使う前提だったが、main に未導入のため本変更で追加した（同名・同型。#30 側とは列追加が冪等なので衝突しない）。
- 停滞の起点は工程の `created_at`。
- **未実装**: 通知の Web Push 送出（ファイル / Webhook の Notifier のみ）、MCP のチェーン用ツール、UI のチェーン表示。

## 12. 未検証事項

- Linux リモート用の `ReadFileNoFollow` のコマンドはローカルの sh（dash・bash）で、最終要素の読み取りを python3（3.14）と perl（5.40）のそれぞれに固定して、差し替えの注入を含めて実行した。SSH 越しの実ホストでは動かしていない。python3 も perl もないホスト（BusyBox だけの最小環境など）では verdict を読めず（exit 5）、工程は `verdict_unavailable` で停止する。perl に Time::HiRes がない場合（Debian 系で perl-base だけのとき）、読み取り後の mtime・ctime 比較は秒単位になる。Windows の C#・PowerShell は組み立てのみ確認し、実ホストでのコンパイル・実行は未確認。
- 実エージェントを使ったチェーンの実運用（v2 §14 の 4）は未実施。
- Webhook 失敗時の再送は `CheckedReporter` を返すテスト用 Notifier で確認した。実 Webhook 受け手での重複排除は未確認。
