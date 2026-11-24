# 10 自動モデル選択（Selector）の設計

ジョブ登録時に agent／model／pool が未指定の場合、Gate の手前で「役割×重さ」と利用枠の実測から自動で選ぶ仕組みの設計（2026-11-23 ゆうや要望、2026-11-24 作成）。

## 1. 原則
- **明示指定が常に優先**する。Selector は未指定のときだけ動く。
- **黙った差し替えはしない**: 選択した agent／model／pool と、その理由（役割・重さ・各プールの残量スナップショット）をジョブに記録し、詳細画面で見えるようにする。
- Selector は「健全なプールの中での最適」を担う。枯渇の判定と保持は従来どおり Gate の仕事で、Selector は枯渇プールを候補から外すだけにする。
- usage が未測定のプールは候補に入れない（登録時バイパスの原則と整合）。

## 2. 入力
- **role ヒント**（登録時に任意指定）: implementation / fix / review / research / judge。未指定なら title や指示文からの推定はせず、standard 扱いの既定表を使う。
- **weight ヒント**（任意）: light / standard / heavy。
- **チェーン制約**: レビューは実装と**別系統**にする。系統はエージェントではなく**モデル由来**で判定する（系統 = Gemini 系 / Claude 系 / GPT 系）。agy は3系統すべてを内包する（Gemini モデル、3p 経由の Claude、GPT-OSS）ため、エージェント単位の分類では同系統のすり合わせを防げない。親ジョブの model が分かれば自動で適用する（2026-11-24、Claude（かなめ）の指摘を受けて改訂）。
- **host 制約**: ジョブに host 指定があれば、その host で実行可能な agent／model の中から選ぶ。

## 3. モデル対応表（2026-11-24、ubuntu-vm の `agy models` で実地確認）
- agy / pool agy-gemini: Neon (low/medium/high/xhigh)、Carbon (low/medium/high)、Argon (low/medium/high)、Hydrogen (low/medium/high/xhigh)、Gemini 3.8 Flash (low/medium/high)、Orchestrator (Neon)=`gemini-pro-orchestrator-neon`、Orchestrator (Carbon)=`gemini-pro-orchestrator`、Orchestrator (Argon)=`gemini-pro-orchestrator-argon`
- agy / pool agy-3p: Claude Opus 5.5 (low/medium/high)、Claude Sonnet 5.5 (low/medium/high)、GPT-OSS 120B (medium)
- claude (Claude Code CLI) / pool claude: `claude-sonnet-5-5`、`claude-opus-5-5`（プローブ確認済みの ID）。effort は `--effort` で low / medium / high / xhigh / max の5段（2026-11-24、claude 2.1.325 の --help で実地確認）。Fable 5.2 は usage credits 専用（残高0）のため現行の fleet では対象外。
- codex / pool codex: `gpt-6.2-sol`（既定、プローブ確認済み）、`gpt-6.2-astra`（難しい一貫作業向け、2026-11-24 にプローブで稼働確認。カタログには旧世代の `gpt-6-astra` も残るが、現行は 6.2 系を使う）
- Orchestrator は司令塔モデルと実行役モデルを分けて指定できる（2026-11-23 の Antigravity 更新）。候補構成: 司令塔 = Neon High、実行役 = Hydrogen High。launch テンプレでペアを渡せるようにする拡張が要る（§6）。
- Carbon はプラン依存（Google AI Ultra / Enterprise 向けとされる）で、契約が変われば `agy models` から消え得る。Argon は fleet では未測定プール扱い（Gate の対象外）。そのため Heavy の agy 候補は **Neon xhigh を実用上の主力**とし、Carbon / Argon は見えているときに限る機会的な候補とする（2026-11-24、Claude の指摘を受けて追記）。

## 4. 選択表（役割×重さ → 第一候補。枠の状況で同格候補へ繰り替える）
| 役割 | light | standard | heavy |
|---|---|---|---|
| implementation | agy Neon Medium / Codex sol | Codex sol / agy Neon Medium / Sonnet 5.5 | Opus 5.5（effort high〜xhigh）/ gpt-6.2-astra（同）/ agy Neon xhigh |
| fix | 実装者と同一（セッション継続が既定） | 同左 | 同左 |
| review | agy Neon Medium（別系統） | Codex sol / Sonnet 5.5（別系統） | 実装の系統と排他に選ぶ（下記） |
| research | agy Flash / Neon Low | Orchestrator (Neon) / Neon Medium | Orchestrator（司令塔 Neon High＋実行 Hydrogen High） |
| judge | Gemini Decision（評価器） | 同左 | 同左 |

- heavy のレビュー候補は実装側と排他的に構成する: 実装が Claude 系なら gpt-6.2-astra / Neon xhigh、実装が GPT 系なら Opus 5.5 / Neon xhigh、実装が Gemini 系なら Opus 5.5 / gpt-6.2-astra。同系統しか残らない場合は Selector で無理に埋めず、保持して判断を仰ぐ。
- 段（Light / Standard / Heavy）と effort の対応（2026-11-24 の3段案を反映）: Light = 定型・小修正・下調べ・軽いレビュー（Claude: Sonnet 5.5 low／Codex: Sol low／agy: Hydrogen low〜medium、Flash）。Standard = 通常の実装・レビュー・fix（Claude: Sonnet 5.5 medium〜high／Codex: Sol medium〜high／agy: Neon medium〜high、Orchestrator (Neon)）。Heavy = 設計・難しい一貫作業・重要なレビュー・判定の最終確認（Claude: Opus 5.5 high〜xhigh／Codex: gpt-6.2-astra high〜xhigh／agy: Neon xhigh 主力、Carbon / Argon は機会的、Orchestrator (Carbon)）。Claude の Haiku 5.5 は当方の環境では未確認のため候補に入れない（確認できたら Light に追加する）。
- 同格候補の間では「週間リセットまでの近さ × 残量」で失効の近い枠から使う（もったいない順）。
- fix で実装者のプールが枯渇している場合は、別系統への切替を提案として記録し、Gate の保持に委ねる（勝手に系統を変えない）。

## 5. 記録
- ジョブに selector の結果（agent/model/pool、理由、残量スナップショット、候補から外したプールとその理由）を残す。gate_decisions と同様に後から追えること。

## 6. 段階導入
- **S1（記録のみ）**: Selector の提案を計算して記録するが適用はしない。コーディネーターの手動選択と比較し、一致率と外し方を見る（shadow 判定と同じ進め方）。
- **S2（未指定時の自動適用）**: role が明確なジョブ（review / judge 等）から自動適用を始める。
- **S3（チェーン自動編成）**: 親子チェーン全体（実装→レビュー→判定）の agent 割り当てを Selector が編成する。
- 各段階の昇格はデータを見て ゆうや が判断する。

## 7. 未決事項
- role / weight ヒントの API 形状（create_job のフィールド追加か、title の規約か）。
- Orchestrator のペア指定を launch テンプレへどう渡すか。
- GPT-OSS 120B の使いどころ（3p プールの節約先としての評価が先）。
- S1 の比較記録の置き場所（gate_decisions の拡張か、別テーブルか）。
- Selector と Gate の接点（2026-11-24、Claude の指摘で追加）: 未測定（unknown）プールの扱いを Selector と Gate でどう揃えるか（Gate は未測定をバイパスするが、Selector は候補から外す方針でよいか）。
- 登録時の alt_pools と Selector の選択の優先関係（Selector が選んだ pool が枯渇したとき、alt_pools へ倒すのか Gate の保持に委ねるのか）。
- 同格候補の評価式の具体形（残量率とリセットまでの時間の組み合わせ方）。
