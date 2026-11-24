# 11 UI のモデル選択と Pool 自動判定の設計

登録フォームの agent / model / pool の扱いを、利用者の実感に合わせるための設計（2026-11-24 ゆうや指摘: 「Agent と Pool は一緒じゃない？」「model も pick list で選びたい」「model list は1日一度自動更新させたい」）。

## 1. 前提の整理
- Pool は独立した選択肢ではなく、**agent と model から決まる**（fleet.yaml の pools.applies_to が agent × model_family の対応を既に持っている）。フォームで両方を選ばせるのは冗長で、組み合わせミスの元である。
- agent の候補はホスト側の設定にも存在する。/meta の agents がトップレベル `agents:` のキーだけを返す現行実装は、オプション設定の無い agent（claude）を落とす不具合がある（2026-11-24 実地確認。応急処置として本番 fleet.yaml に claude を追加済みだが、コード側で直す）。

## 2. /meta の是正
- agents は「全ホストの agents の和集合 ∪ トップレベル agents のキー」とする。
- models を追加する: agent（と host）ごとに選択可能な model の一覧を返す。

## 3. モデルカタログと日次プローブ
- カタログは host × agent 単位で保持する（同じ agy でもホストで見えるモデルが違う。例: Carbon は ubuntu-vm のみ）。
- 取得方法は agent ごとに異なる:
  - agy: `agy models` の出力をパースする（id と表示名）。
  - codex: ホストの `~/.codex/models_cache.json` を読む（CLI に非対話の一覧コマンドが無いため）。読めなければ設定のカタログにフォールバック。
  - claude: 一覧コマンドが無いため、fleet.yaml の設定カタログ（top-level agents 配下に models を書けるようにする）を正とする。
- プローブは serve のバックグラウンドで **24時間間隔**（版数プローブとは別設定、0で無効）。結果は DB（agent_models）に保存し、/meta は最新のスナップショットを返す。プローブ失敗時は前回のカタログを維持し、黙って空にしない。
- model の family は ID から決める: `gemini-*` → gemini、`claude-*` → claude、`gpt-*` → gpt、その他は unknown（Pool 判定では未測定扱いに倒す）。

## 4. Pool の自動判定
- POST /jobs で pool が空なら、pools.applies_to から導出する: agent が claude → claude pool、codex → codex pool。agy は model の family で分け、gemini → agy-gemini、claude / gpt → agy-3p。
- 明示指定があれば従来どおりそれを優先する（後方互換）。
- 導出できない組み合わせは、従来の「pool なし」として扱い、エラーにしない。

## 5. UI フォーム
- agent を選ぶ → host はその agent を持つホストだけに絞る → model はカタログからの pick list（host × agent で絞る）。
- pool の選択欄は廃止し、導出結果を表示だけする（例: 「pool: agy-3p（自動）」）。
- カタログが無い agent / host の組み合わせでは、model は従来どおり手入力も可能にする（フォールバックを消さない）。

## 6. 非目標
- Pool の手動上書き UI は v1 では作らない（API の明示指定は残す）。
- モデルの自動選択（Selector、docs/10）とは別物とする。本設計は「選べる一覧の提供と Pool の導出」まで。
