# 19 Selector（エージェント／モデル自動選択）の実装

docs/10 の設計のうち **S1（記録のみ）** と **S2（未指定時の自動適用）** の実装メモ。S3（チェーン自動編成）は未実装。実装: `internal/selector`、配線: `internal/api` の `POST /jobs`。

## 1. 位置づけ
- Selector は Gate の**前段**。ジョブ登録時に agent / model / pool の未指定項目を決め、その結果を Gate が従来どおり判定する。Gate の判定ロジックは変更していない。
- **明示指定は絶対に上書きしない**。補完されるのは空の項目だけ。

## 2. 設定と mode の切替
```yaml
selector:
  mode: record   # off | record | apply（既定 off）
```
`GET /meta` の `selector_mode` で確認できる。変更は fleet.yaml を書き換えて `serve` を再起動する。

| mode | 未指定の扱い | 実行に使う値 | 記録 |
|---|---|---|---|
| off | 従来どおり（Selector は動かない。role / weight は受理するが無視） | 指定どおり | なし |
| record (S1) | 選択を計算 | **既定フォールバック** `codex / gpt-6.2-sol / pool codex`（空の項目のみ。agent が codex 以外の明示なら model / pool は pools.applies_to で解決できる範囲） | `selection_mode=recorded` |
| apply (S2) | 選択を計算 | **選択結果**（空の項目のみ） | `selection_mode=applied` |

agent と model の両方が明示されているジョブは Selector を呼ばず、`selection_mode=explicit` で記録する（record / apply のとき）。

推奨手順: off → record で一致率を見る（selected_* と実際の agent/model を比較）→ 問題なければ apply。昇格はゆうやが判断する（docs/10 §6）。

## 3. 登録 API
`POST /jobs` で `agent` / `model` / `pool` が任意。追加のヒント:
- `role`: `implement | review | judge | fix | publish | misc`（未指定は implement 扱い）。不正値は 400。
- `weight`: `light | standard | heavy`（未指定は standard 扱い）。不正値は 400。
title や指示文からの推定はしない（docs/10 §2）。

## 4. 対応表（コード: `internal/selector/selector.go` の `table`）
先頭から順に評価し、**最初に健全な候補**を採る。

| role | light | standard | heavy |
|---|---|---|---|
| implement | agy Neon Medium / Codex sol | Codex sol / agy Neon Medium / claude sonnet 5.5 | claude opus 5.5 / codex astra / agy Neon xhigh |
| review | agy Neon Medium | Codex sol / claude sonnet 5.5 | heavy implement と同じ並び（※） |
| fix / publish / misc | implement と同じ行 | 同左 | 同左 |
| judge | agy Neon Medium | agy Neon Medium | agy Neon xhigh |

※ 「実装の系統と排他」（docs/10 §4）は親ジョブのモデルを見る S3 の領分で、本実装では未対応。
agy のモデル ID（`gemini-4-neon-medium` 等）は既存テストの表記に合わせた仮置きで、実機の `agy models` との突き合わせは未了。

## 5. 選択ロジック
1. 明示値と矛盾する候補を除く。
2. 各候補の pool を Gate と同じ `quota.EvaluatePool`（同じ usage スナップショット、同じ stale 判定）で評価する。
3. 健全な最初の候補を採用。**枯渇**プールは候補から外し、理由に記録する。**未測定／unknown／stale** のプールも候補に入れない（docs/10 §1）。
4. 健全な候補が無く、実測が stale／欠落のとき: 決定論の既定順（表の先頭の非枯渇候補）にフォールバックし、理由に `FALLBACK ... (usage stale or missing)` と書く。
5. 全候補が枯渇のとき: 表の先頭を残し、Gate の保持に委ねる（理由に記録）。
6. 明示値が表のどの行とも合わないときは、明示値のまま、足りない項目は空のままにし、その旨を理由に記録する。

同格候補の評価式（残量×リセットまでの近さ）は docs/10 §7 の未決のままで、現状は表の順。

## 6. 記録の読み方
ジョブ（`GET /jobs/{id}`、UI のジョブ詳細）に以下が出る。
- `selected_agent` / `selected_model` / `selected_pool`: Selector の選択（record では実行値と異なり得る）。
- `selection_mode`: `explicit` | `recorded` | `applied`（off では空）。
- `selection_reason`: 役割・重さ、各プールの状態、除外理由、フォールバックの有無、record のときは実行した既定値。
store には jobs テーブルの列追加マイグレーションで保持する（既存 DB は起動時に追加）。Gate 保持（enforce）されたジョブも、保持時のペイロードに含まれて解放後のジョブへ引き継がれる。

## 7. 未実装・留意
- S3（チェーン自動編成）、host 制約、親モデルによる別系統レビュー、残量スナップショット全体の保存（理由文字列に要約のみ）。
- Gate が `alt_pools` で別プールに倒した場合、ジョブの `pool` は Gate の結果、`selected_pool` は Selector の選択のまま残る。

## レビュー修正で確定した挙動（PR #22）

- **Gate の入力**: enforce / dry-run とも、Selector 適用後に実際に実行されるプール（apply は選択結果、record は既定 codex）を評価する。apply で全候補が枯渇、record の既定プールが枯渇している場合は Gate が保持（HTTP 202）する。
- **部分指定の補完**: role/weight の行に適合候補が無い場合は全既知候補（固定順）へ広げて、指定された agent / model / pool と両立するものを探す。agent+model が表の既知ペアなら pool も補完（apply のみ）。表に無い組み合わせは指定値のまま保持し、足りない項目は空のまま理由に記録する。
- **off**: role / weight は受理するが検証も使用もしない（従来クライアント互換を優先）。Issue #21 の「agent 未指定はバリデーションエラー」は採用していない（要合意）。record / apply では不正な role / weight は 400。
- **fix**: apply で `parent_id` があり agent/model 未指定なら親ジョブの agent/model に追従する。親が取得できない場合は implement 行。**judge**: docs/10 の Gemini Decision（評価器）はジョブではないため S1/S2 の対象外で、暫定的に agy Neon で実行する。
- **未測定プール**: `unmeasured` のプールは自動選択から除外する（全候補が未測定／枯渇のときのみ表の先頭を保持し Gate に委ねる）。理由には usage 欠落（missing）／stale／unmeasured／exhausted を区別して記録する。
