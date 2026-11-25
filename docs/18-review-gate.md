# docs/18 — レビュー判定ゲート（Decision 1）

Issue #16。レビュー結果の判定を shadow（並行記録のみ）から本採用に切り替えるためのエンドポイントと、その運用手順。

## 1. 目的と範囲

- 修正・publish ジョブが分岐する根拠を、レビュアーが `review.md` に書いた verdict から、Decision 1（`gemini-decision-1`）の判定に変えられるようにする。
- ゲートは既定で無効。有効化するまで従来の運用は変わらない。
- 範囲外: publish のサーバー側自動化（publish は従来どおりジョブが行う）、shadow 判定ジョブの廃止（チェーン登録側の変更）。

## 2. 設定

```yaml
evaluator:
  # Decision 1 の API キー。次のどちらかで渡す。両方あれば api_key が優先する。
  api_key: "<Gemini API キー>"     # 任意
  api_key_env: GEMINI_API_KEY      # 任意（既定 GEMINI_API_KEY）

review_gate:
  enabled: true                    # 既定 false
```

- Decision 1 のクライアントとキーは、受け入れ条件の評価（[docs/06](06-implementation-design.md) の Evaluator）と共用する。`api_key` は受け入れ条件の評価にも使われる。
- ゲートは `evaluator.mode` に依存しない。`always-pass` でもキーがあれば判定する。
- キーの値は API の応答にもログにも出さない。外から確認できるのは「設定済みかどうか」だけ。上流の応答にキーが含まれていた場合、ログに残す前に `[REDACTED]` に置き換える。

`GET /meta`（read スコープ）の応答:

```json
"review_gate": {"enabled": true, "model": "gemini-decision-1", "key_configured": true}
```

`key_configured` はキーが空でないことだけを表す。キーが Decision API に受け付けられるかは確認しない。

## 3. エンドポイント

`POST /decision/review-verdict`（operate スコープ）

認証: **API トークン（`api.token` または `api.tokens`）の設定が必須。** 他の JSON API は、トークンが一つもなく `api.require_auth` が false なら匿名で使えるが、このエンドポイントは `require_auth` に関係なく 401 を返す（`/events`・`/mcp` と同じ扱い）。

要求:

```json
{"text": "<review.md の全文>", "job_id": 123, "review_job_id": 122}
```

- `text`: 必須。100,000 文字まで。超える場合は切り詰めず 413 で拒否する（切り詰めると未解決の指摘が落ちるため）。
- `job_id` / `review_job_id`: 任意の正の整数。記録用の参照で、ジョブの実在は確認しない。

成功（200）:

```json
{"verdict": "ok_to_merge", "score": 0.91, "reason": "decision1 noul: verdict p=0.93 (>= 0.50); unresolved=none p=0.91 (>= 0.50); reviewer_reliability p=0.88 (informational)"}
```

失敗した場合は `{"error": "..."}` だけを返し、`verdict` は含めない。

| 状態 | 意味 |
|---|---|
| 400 | `text` が空、ID が正でない、JSON が不正、本文が 1 MiB を超える |
| 401 | トークンがない・一致しない、またはサーバーに API トークンが一つも設定されていない（ゲートの有効・無効より先に判定する） |
| 403 | operate スコープがない |
| 413 | `text` が長すぎる |
| 500 | 判定を `decision_log` に記録できなかった |
| 502 | Decision API の呼び出し失敗、または応答が不完全 |
| 503 | `review_gate.enabled` が false、またはキーが未設定 |
| 504 | Decision API がタイムアウト（60 秒） |

サーバー側の再試行はしない。502 / 504 の詳しい理由は `serve` のログ（`review verdict failed:`）に出る。

## 4. 判定の方法

Decision 1 は文章を生成せず、質問ごとの確率（noul）を返す。shadow 判定の固定書式の観点を、次の 3 問にしている。確率が高いほどマージ側になる向きで聞く。

| 固定書式の項目 | 質問の内容 | 判定への使い方 |
|---|---|---|
| `verdict` | レビュー結果の判定は ok_to_merge か | 判定に使う |
| `unresolved` | 未解決（Not Resolved）の項目は none か | 判定に使う |
| `reviewer_reliability` | 指摘に再現証跡があり、スコープ外の指摘が混ざっていないか | `reason` に出すだけ |
| `reason` | （質問しない） | サーバーが各確率から組み立てる |

- `score` は `verdict` と `unresolved` の確率の小さい方。`score` がしきい値以上なら `ok_to_merge`、未満なら `changes_needed`。
- しきい値は `evaluator.decision_threshold`（既定 0.5）。ただし **0.5 より下げてもこのゲートには 0.5 が使われる**。受け入れ条件の評価を緩めても、ゲートは緩まない。
- 3 問のどれかに有効な確率が返らなければ、判定せず 502 にする。
- 応答の JSON で同じオブジェクトに同じキーが 2 回以上ある場合（大文字小文字の違いだけのものを含む）も、判定せず 502 にする。この検査は受け入れ条件の評価にも適用される。
- プロンプトは固定（`internal/evaluator/review.go`）。レビュー本文中の判定者向けの指示には従わないよう指示している。

### 記録

成功した判定は `decision_log` テーブルに残る。列は `created_at`、`job_id`、`review_job_id`、`verdict`、`score`、`model`。レビュー本文と `reason` は保存しない。失敗した要求は記録しない。参照用の API はないので SQLite で読む:

```sh
sqlite3 <db パス> 'SELECT created_at, job_id, review_job_id, verdict, score, model FROM decision_log ORDER BY id DESC LIMIT 20'
```

テーブルは `serve`（または DB を開くコマンド）の起動時に自動で作られる。

## 5. 限界

- **Decision 1 の判定精度は未測定。** 昇格の根拠になった shadow 運用（有効 27 件・一致 27 件・危険方向 0 件、Issue #16）の件数は、このエンドポイントの実績ではない。shadow 判定ジョブは `review.md` を読んで固定書式を書くエージェントジョブで、本実装は同じ観点を Decision 1 に確率で聞く別の方式である。チェーンの分岐を切り替えるのは、このエンドポイントでの照合（6-2）が条件を満たしてから。
- `reason` は確率の要約で、判定の根拠となる指摘の説明ではない。未解決項目の一覧も返さない。修正ジョブは指摘の内容を引き続き `review.md` から読む。
- レビュー本文は判定対象であると同時に入力なので、本文の書き方で確率が動く可能性がある（プロンプトインジェクションを含む）。
- `review.md` 冒頭の verdict との突き合わせはしない。冒頭の verdict の行は本文の一部として Decision 1 に渡るので、6-2 の照合で分かるのは「全文を渡したときに採用した判定と同じ結論が返るか」で、レビュー自体の正しさではない。
- レビュー本文は Google の Decisions API に送信される。送ってはいけない内容を `review.md` に書かない。

## 6. 運用手順

### 6-1. 有効化

1. キーを設定する。
   - `fleet.yaml` の `evaluator.api_key` に書く場合: ファイルを自分だけが読める権限にし（`chmod 600 fleet.yaml`）、リポジトリにコミットしない。同じホストで同じユーザーとして動くエージェントは、このファイルを読める。
   - 環境変数で渡す場合: `serve` を起動する環境（systemd なら `EnvironmentFile=` など）に `GEMINI_API_KEY` を入れる。`api_key` は空にする。
2. API トークンがなければ設定する（`api.token`、または operate スコープを持つ `api.tokens`。[docs/12](12-api-tokens.md)）。トークンを一つでも設定すると、他の JSON API も匿名では使えなくなるので、既存の利用者にもトークンを配る。
3. `fleet.yaml` に `review_gate.enabled: true` を追加する。
4. `serve` を再起動する（設定も環境変数も起動時のものが使われる）。起動バナーが `review_gate: enabled` で、キーに関する `WARNING` がないことを確認する。
5. `GET /meta` で `enabled` と `key_configured` がともに true であることを確認する。
6. サンプルの `review.md` で 6-3 の呼び出しを 1 回実行し、200 が返ることを確認する（キーが実際に有効かはここで初めて分かる。**判定が正しいかは確認できない**）。

ここまででは、修正・publish ジョブの指示は変えない。

### 6-2. 照合（切り替えの条件）

shadow の実績はこの方式の実績ではない（5 節）。ジョブの指示を変える前に、Issue #16 の昇格条件をこのエンドポイント（Decision 1）の判定で満たすことを確認する。サーバーはこの条件を検査しないので、運用で守る。

1. 判定が確定している過去の `review.md` を選ぶ。`changes_needed` の事例を含める。結果を見てから対象を除かない。
2. 1 件ずつ 6-3 の呼び出し例で判定させる。
3. 各件について、その `review.md` で実際に採用した判定（基準）と、エンドポイントの `verdict` を比べる。shadow の記録がある件は併記する。
4. 次の内容を、後から参照できる場所（Issue #16 のコメントなど）に記録する: 実施日、モデル、しきい値、件ごとの対象・基準・`verdict`・`score`、集計。`decision_log` には基準の判定が残らないので、これだけでは記録にならない。
5. 次をすべて満たせば切り替える。満たさなければ指示は変えない。

| 条件 | 数え方 |
|---|---|
| 15 件以上 | 判定が返った、異なる `review.md` の数。エラーで判定が返らなかった件は含めず、件数と理由を記録する |
| 一致率 9 割以上 | 基準と `verdict` が一致した件数 / 上の件数 |
| 危険方向 0 件 | 基準が `changes_needed` なのに `verdict` が `ok_to_merge` の件数 |

モデル、`evaluator.decision_threshold`、固定プロンプトのどれかを変えたら、照合をやり直す。

条件を満たして記録を残したら、修正・publish ジョブの指示を 6-3 の形に変える。

### 6-3. チェーン側の使い方

修正・publish ジョブは、エンドポイントの `verdict` で分岐する。**エンドポイントが失敗したら進めない。** `review.md` の verdict への切り替えや、`ok_to_merge` の推定はしない。

ジョブに渡すトークンには operate スコープが必要になる。operate はジョブの取り消しや再投入もできる権限なので、渡す範囲に注意する（[docs/12](12-api-tokens.md)）。

呼び出しの例（`QF_URL`、`QF_TOKEN` を設定して、`review.md` のあるディレクトリで実行する。`jq` と curl 7.76 以降が必要）:

```sh
#!/bin/sh
set -eu

if ! resp=$(jq -n --rawfile text review.md '{text: $text}' |
  curl -sS --fail-with-body --max-time 90 \
    -H "Authorization: Bearer $QF_TOKEN" -H 'Content-Type: application/json' \
    --data-binary @- "$QF_URL/decision/review-verdict"); then
  echo "review gate failed; not proceeding: $resp" >&2
  exit 1
fi

case "$(printf '%s' "$resp" | jq -r '.verdict')" in
  ok_to_merge)    echo "ok_to_merge" ;;      # publish ジョブ: Ready 化する / 修正ジョブ: 何もしない
  changes_needed) echo "changes_needed" ;;   # publish ジョブ: 何もしない / 修正ジョブ: 指摘を直す
  *) echo "unexpected response; not proceeding: $resp" >&2; exit 1 ;;
esac
printf '%s\n' "$resp"
```

ジョブ ID を記録に残す場合は、`jq` に `--argjson job_id 123 --argjson review_job_id 122` を足し、本体を `'{text: $text, job_id: $job_id, review_job_id: $review_job_id}'` にする。

ジョブの指示には次を含める。

- 分岐の根拠はエンドポイントの `verdict` であること。
- 非 0 終了なら、Ready 化も修正もせず、生のエラーを `summary.md` に書いて終了すること。
- 応答（`verdict` / `score` / `reason`）を `summary.md` に残すこと。

### 6-4. 無効化（従来運用に戻す）

1. 修正・publish ジョブの指示を、`review.md` 冒頭の verdict で分岐する従来の形に戻す。
2. `review_gate.enabled: false` にする（または `review_gate` を削除する）。
3. `serve` を再起動する。

**1 を忘れて 2・3 だけを行うと、エンドポイントを呼ぶジョブは 503 を受けて進まなくなる。** 自動で従来の判定に切り替わることはない。

`api_key` は受け入れ条件の評価にも使われるので、ゲートを無効にしても消す必要はない。`decision_log` の記録は残る。
