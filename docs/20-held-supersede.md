# docs/20 — 保持ジョブの代替（supersede）

Issue #23。同じ役割の古い登録を、新しい登録が自動で取り消す仕組み。

## 1. 目的と範囲

- チェーンをラウンドごとに登録すると、枠切れで保持された古いラウンドのジョブ（例: publish）が残る。放置すると、解放の順序しだいで古い publish が最新のレビューを読んで動く。
- 登録側が同じ役割のジョブに同じ `supersede_key` を付けると、まだ開始していない古いジョブは取り消される。
- 範囲外: 実行中（claimed）ジョブの停止、チェーン登録そのものの自動化。

## 2. 指定方法

`supersede_key` は任意の文字列。前後の空白は無視し、大文字と小文字は区別する。空（または空白のみ）は未指定と同じで、未指定の登録は他の登録に影響せず、影響も受けない。

| 経路 | 指定 |
| --- | --- |
| HTTP API | `POST /jobs` の `supersede_key` |
| MCP | `create_job` の `supersede_key` |
| CLI | `quota-fleet job create --supersede-key <key>` |

Web UI の新規登録フォームには入力欄がない。

キーは DB 全体で照合し、agent・host・親ジョブをまたぐ。別の対象のジョブと取り消し合わないように、キーには対象を含める（7 章）。

取り消せる範囲は登録した主体の権限で決まる。

| 登録した主体 | 取り消せる登録 |
| --- | --- |
| `operate` スコープを持つトークン、認証なしの API、CLI | 同じキーのすべて |
| `operate` スコープを持たないトークン | 同じキーのうち、そのトークンが登録したもの |

権限外の古い登録は、状態を変えずに残る（エラーにはしない）。解放時の再確認も同じ範囲で判定する。権限は登録時のものを記録し、後からトークン設定を変えても過去の登録の範囲は変わらない。トークンは名前で識別する。

## 3. 登録時の代替

同じキーの新しい登録（ジョブ作成、または Quota Gate の保持）は、同じ 1 トランザクションで、それより前の登録のうち次を取り消す。

| 古い登録の状態 | 扱い |
| --- | --- |
| held | `cancelled`。`cancel_reason` に理由を記録する |
| queued / waiting | `cancelled`。`last_error` に理由を記録する |
| claimed | 触らない |
| done / failed / cancelled | 触らない |
| draft | 対象外（draft になる登録にはキーを指定できない） |

理由は次の形式。

```
superseded: newer registration request_id=<新しい request_id> has supersede_key=<key>
```

- 取り消すのは同じキーの登録だけである。取り消したジョブを待っている waiting の子のうち、同じキーを持たないもの（キー未指定・別キー）は変更しない。手動のキャンセルと違い、連鎖しない。
- 残った子は `waiting` のままで、実行されない。Reconciler は親が終了した waiting ジョブ（`orphan_waiting`）として一度通知する。不要なら `quota-fleet job cancel` で取り消す。親を `requeue` して完了させれば、子は通常どおり `queued` になる。子のキーで新しい登録があれば、その時点で取り消される。
- draft になる登録（`--draft` かつ親なし）に `supersede_key` を指定すると 400 になり、何も取り消さない。実行できるジョブがなくなるのを防ぐためである。
- 同じ `request_id` の再送は新しい登録ではないので、何も取り消さない。取り消された保持の `request_id` を再送すると、キーを省いても 409 になる。取り消されていない保持の `request_id` からキーを省いてジョブを作った場合は、そのジョブは保持のキーを引き継ぐ。
- `supersede_key` を指定した登録は、親が完了できない場合に 400 になり、何も取り消さない。親、または親が（間接的にも）待っているジョブが `failed` / `cancelled`、あるいはこの登録で取り消される場合である。実行できないジョブのために古い登録を取り消さないためである。保持になる場合は、親が存在しないときも 400 になる。キー未指定の登録にこの追加の確認はない。

## 4. 解放時の再確認

`supersede_key` を持つ保持は、解放するとき（定期チェック、`POST /gate/held/{id}/release`、`quota-fleet gate release`）に、その保持を取り消せる同じキーの新しい登録（2 章の権限の範囲）がないかを確認する。新しい登録があれば、その状態（held・claimed・done・cancelled など）に関係なくジョブを作らず、保持を `cancelled`（superseded）にする。手動の解放は 409 と `{"released": false, "reason": "superseded: ..."}` を返す。

確認、ジョブ作成、保持の状態変更は 1 トランザクションなので、その間に別の登録は割り込まない。

再確認はプールが解放できる状態になったときに行う。登録時の代替が先に働くため、通常はここで取り消される保持はない。

新旧は**登録した順**で決まる。保持が後からジョブになっても順序は変わらない。

## 5. 参照

`POST /jobs`（201 / 200 の `job`、202 の `held`）、`GET /jobs/{id}`、`GET /gate/held` は、次を返す。該当がないフィールドは省略する。

| フィールド | 内容 |
| --- | --- |
| `supersede_key` | 登録時のキー |
| `superseded_job_ids` | この登録が取り消したジョブ |
| `superseded_held_ids` | この登録が取り消した保持（ジョブになっていないもの） |
| `superseded_by` | この登録を取り消した登録。`{"job_id": 12}` または `{"held_id": "held-..."}` |

`GET /jobs`（一覧）は `supersede_key` だけを返す。

例:

```json
{"job": {"id": 14, "status": "queued", "supersede_key": "pr42:publish",
         "superseded_job_ids": [11], "superseded_held_ids": ["held-0123456789abcdef"]},
 "created": true}
```

## 6. 保持一覧

`GET /gate/held` の各要素は、`created_at` に加えて次を返す。

- `age_seconds`: 保持の作成からの経過秒数（サーバー時刻）
- `parent_id`、`parent_status`: 親ジョブとその状態。親が存在しなければ `missing`
- `supersede_key`
- `cancel_reason`: 代替で取り消された場合の理由

Web UI の保持一覧（`/ui/gate/held` とダッシュボード）は、作成時刻と経過時間、親ジョブと状態、`supersede_key` を表示する。`/ui/gate/held` は取り消し理由も表示する。

## 7. チェーン登録側の使い方

**ラウンドをまたいで同じ役割を持つジョブには、同じ `supersede_key` を付ける。** 役割と対象の両方をキーに入れる。

```
<対象>:<役割>    例: ypsilonmeister/quota-fleet#42:publish
```

- 対象（PR や Issue）を入れないと、別の PR の publish どうしが取り消し合う。
- 役割（`review` / `fix` / `publish`）を入れないと、同じチェーン内の別の役割が取り消し合う。
- 1 ラウンドの中で並行して走らせるジョブ（複数のレビュアーなど）には、それぞれ別のキーを付ける。
- 次のラウンドは、親から順に新しいジョブを登録し直す。古いラウンドの子は、同じ役割の新しい登録が来るまで `waiting` のまま残る。キーを付けていない子は残り続けるので、チェーンのジョブにはすべてキーを付ける。
- `operate` を持たないトークンで登録する場合、全ラウンドを同じトークンで登録する。

```sh
quota-fleet job create --title "publish r2" --parent 31 \
  --supersede-key "ypsilonmeister/quota-fleet#42:publish" --request-id pr42-r2-publish
```

## 8. 制限

- claimed のジョブには触らないので、実行中の古いジョブは最後まで動く。
- 親が代替された waiting の子は自動では取り消されない（3 章）。
- claimed のジョブが `reset`（手動、または Reconciler の自動 reset）で queued に戻った場合や、取り消したジョブを `requeue` した場合は、新しい登録があってもそのまま queued になる。次に同じキーで登録されると取り消される。
- 保持を経ていない queued のジョブには、claim 時の再確認はない。
- `supersede_key` 導入前に登録されたジョブと保持はキーを持たないので、対象にならない。

## 9. データ

- `jobs.supersede_key`、`held_jobs.cancel_reason`（起動時に列を追加する）
- `supersede_registrations`: キー付きの登録 1 件につき 1 行。`seq` が登録順で、保持と解放後のジョブは同じ行を共有する。`owner`（トークン名）と `own_only` が取り消せる範囲を記録する
