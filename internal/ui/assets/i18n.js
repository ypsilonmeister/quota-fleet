// Locale dictionaries and the small runtime behind data-i18n and t() (docs/17).
// This is the only UI source file that may contain Japanese text.
(function (root) {
  "use strict";

  const LOCALE_KEY = "qf-locale";
  // Japanese is the source text; a key missing elsewhere falls back to it.
  const SOURCE_LOCALE = "ja";
  const LOCALES = ["ja", "en"];
  // Attributes translated by data-i18n-<name> in static markup.
  const I18N_ATTRIBUTES = ["aria-label", "placeholder", "title"];

  const MESSAGES = {
    ja: {
      "lang.switchLabel": "表示言語の切替",
      "lang.ja": "JA",
      "lang.en": "EN",
      "lang.jaName": "日本語",
      "lang.enName": "English",

      "nav.dashboard": "ダッシュボード",
      "nav.jobs": "ジョブ一覧",
      "nav.newJob": "新規登録",
      "nav.held": "保持中",
      "nav.decisions": "ゲート判定",
      "nav.deliveries": "納品",
      "nav.agents": "エージェント版数",

      "theme.switchLabel": "表示モードの切替",
      "theme.dark": "ダーク",
      "theme.darkLabel": "ダーク（計器盤）",
      "theme.light": "ライト",
      "theme.lightLabel": "ライト（台帳）",

      "header.notifyTitle": "ジョブ完了のブラウザ通知（iOS / iPadOS の Web Push はホーム画面に追加した PWA のみ、iOS 16.4 以降）",
      "header.notify": "通知",
      "header.refresh": "更新",
      "header.token": "トークン",

      "auth.title": "API トークン認証",
      "auth.description": "API に接続するための Bearer トークンを入力してください。トークンはこのタブの sessionStorage にのみ保持されます。",
      "auth.tokenLabel": "Bearer トークン",
      "auth.tokenPlaceholder": "fleet.yaml の api.token",
      "auth.connect": "接続",
      "auth.connectNoAuth": "トークンなしで接続",
      "auth.required": "認証が必要です（401 Unauthorized）",
      "auth.connectFailed": "接続に失敗しました",
      "auth.connectError": "接続エラー: {message}",
      "auth.tokenMissing": "トークンを入力してください。",

      "common.loading": "読み込み中...",
      "common.untitled": "(無題)",
      "common.noTitle": "(タイトルなし)",
      "common.emptyText": "(なし)",
      "common.noneOption": "（なし）",
      "common.selectOption": "-- 選択 --",
      "common.all": "すべて",
      "common.filter": "絞り込み",
      "common.clear": "クリア",
      "common.cancel": "キャンセル",
      "common.error": "エラー: {message}",
      "common.jobLabel": "ジョブ #{id}",
      "common.openJob": "ジョブ #{id} を開く",
      "common.statusFetchFailed": "ステータス取得に失敗しました",
      "common.cancelFailed": "キャンセルに失敗しました",
      "common.cancelError": "キャンセルエラー: {message}",
      "common.newJobButton": "+ 新規ジョブ登録",

      "column.title": "タイトル",
      "column.status": "状態",
      "column.pool": "プール",
      "column.holdReason": "保持理由",
      "column.createdAt": "作成時刻",
      "column.parentJob": "親ジョブ",
      "column.host": "ホスト",

      "status.done": "done",
      "status.claimed": "実行中",
      "status.queued": "queued",
      "status.waiting": "waiting",
      "status.failed": "失敗",
      "status.cancelled": "キャンセル",
      "status.draft": "draft",

      "chain.summaryEmpty": "全0件",
      "chain.summaryTotal": "全{total}件",
      "chain.summaryBreakdown": "全{total}件中 {parts}",
      "chain.summaryItem": "{label} {count}件",
      "chain.summarySeparator": "・",
      "chain.fetchNetworkError": "通信エラー",
      "chain.fetchInvalidResponse": "不正なレスポンス形式",
      "chain.fetchGenericError": "取得エラー",
      "chain.unknownParent": "不明な親",
      "chain.unknownStatus": "不明な状態",
      "chain.errorSeparator": "、",
      "chain.errorMore": " 他 {count} 件",
      "chain.errorNotice": "一部の子ジョブ取得に失敗したため、チェーン構成に欠落がある可能性があります（失敗: {items}{extra}）。取得できた分のみ表示しています。",
      "chain.currentJob": "表示中ジョブ",
      "chain.directErrorNotice": "{target} の直下の子ジョブ取得が一部失敗したため、一覧に欠落がある可能性があります（失敗状態: {items}）。取得できた分のみ表示しています。",
      "chain.truncatedExpand": "他 {count} 件のノード配下の探索を上限で打ち切りました",
      "chain.truncatedUp": "祖先の探索を上限で打ち切りました",
      "chain.truncatedPages": "一部の子ジョブ一覧がページ取得上限に達しました",
      "chain.truncatedLimit": "探索上限に到達",
      "chain.truncatedSeparator": "・",
      "chain.truncatedDirectComplete": "表示中ジョブの直下の子ジョブは全件取得済みです。",
      "chain.truncatedNotice": "チェーンが大きいため一部省略されています（{reasons}）。{note}",
      "chain.tableHeading": "チェーン / ジョブ",
      "chain.orphanBefore": "(親 ",
      "chain.orphanAfter": " 未取得)",
      "chain.sectionTitle": "チェーン構成",
      "chain.unavailable": "チェーン情報を取得できませんでした。",
      "chain.currentTag": "現在表示中",

      "form.noMatchingHost": "選択中の agent ({agent}) に対応するホストがありません。",
      "form.derivedPool": "pool: {pool}（自動）",
      "form.derivedPoolNone": "pool: なし（自動判定対象外）",

      "page.notFound": "ページが見つかりません",
      "page.backToDashboard": "ダッシュボードに戻る",

      "dashboard.title": "ダッシュボード",
      "dashboard.statusCounts": "状態別件数",
      "dashboard.total": "total",
      "cooldown.badge": "冷却中",
      "cooldown.until": "冷却解除まで {minutes} 分（{until}）",
      "cooldown.hits": "連続 {hits} 回",
      "cooldown.queued": "プール冷却中のため起動待機（解除予定 {until}）",
      "cooldown.clear": "冷却解除",
      "cooldown.confirm": "プール {pool} の冷却を解除しますか？",
      "cooldown.clearFailed": "冷却解除に失敗しました。",
      "dashboard.unmeasured": "unmeasured (バイパス)",
      "dashboard.weeklyRemaining": "週次残量: {percent}%",
      "dashboard.shortRemaining": "短期残量: {percent}%",
      "dashboard.resetLabel": "リセット:",
      "dashboard.resetLabelWithTz": "リセット ({tz}):",
      "dashboard.resetNone": "なし",
      "dashboard.weeklyReset": "週次 {time}",
      "dashboard.weeklyResetMissing": "週次 なし",
      "dashboard.shortReset": "{window} {time}",
      "dashboard.shortResetMissing": "{window} なし",
      "dashboard.shortResetNotApplicable": "{window} 対象外",
      "dashboard.measuredAt": "測定: ",
      "dashboard.poolSummary": "プール残量サマリ",
      "dashboard.runningJobs": "実行中ジョブ ({count})",
      "dashboard.noRunningJobs": "実行中のジョブはありません。",
      "dashboard.heldJobs": "保持中ジョブ ({count})",
      "dashboard.noHeldJobs": "保持中のジョブはありません。",
      "dashboard.recentDeliveries": "直近の納品 ({count})",

      // Item names that read the same in both languages.
      "field.id": "ID",
      "field.jobID": "Job ID",
      "field.heldID": "Held ID",
      "field.requestID": "Request ID",
      "field.sessionID": "Session ID",
      "field.agent": "Agent",
      "field.host": "Host",
      "field.agentHost": "agent / host",
      "field.workdir": "Workdir",
      "field.model": "Model",
      "field.pool": "Pool",
      "field.altPools": "Alt Pools",
      "field.selectionMode": "選択モード",
      "field.selectedTarget": "Selector の選択",
      "field.selectionReason": "選択理由",
      "field.targetRef": "Target Ref",
      "field.worktreeSpec": "Worktree Spec",
      "field.attempts": "Attempts",
      "field.claimedBy": "Claimed By",
      "field.claimedAt": "Claimed At",
      "field.createdAt": "Created At",
      "field.updatedAt": "Updated At",
      "field.deliveredAt": "Delivered At",

      "jobs.title": "ジョブ一覧",
      "jobs.empty": "ジョブはありません。",
      "jobs.noMatch": "該当するジョブはありません。",
      "jobs.noMatchingChain": "該当するチェーンはありません。",
      "jobs.viewSwitchLabel": "表示切替",
      "jobs.viewFlat": "フラット表示",
      "jobs.viewChain": "チェーン表示",
      "jobs.searchPlaceholder": "タイトル部分一致...",
      "jobs.searchLabel": "タイトル検索",
      "jobs.chainTruncated": "一部の状態 ({statuses}) は最新 100 件まで取得してチェーンを構築しています。",
      "jobs.loadMore": "さらに読み込む",
      "jobs.listFetchFailed": "ジョブ一覧の取得に失敗しました",
      "jobs.statusListFetchFailed": "ジョブ一覧 ({status}) の取得に失敗しました",
      "jobs.loadMoreFailed": "追加取得に失敗しました",
      "jobs.loadMoreError": "追加取得エラー: {message}",

      "detail.invalidID": "不正なジョブ ID です: {id}",
      "detail.backToJobs": "ジョブ一覧に戻る",
      "detail.fetchFailed": "ジョブ #{id} の取得に失敗しました",
      "detail.cancelReasonPlaceholder": "キャンセル理由（任意）",
      "detail.cancelConfirm": "ジョブ #{id} をキャンセルしますか？",
      "detail.cascaded": "（子ジョブ {ids} もキャンセル）",
      "detail.cancelled": "ジョブ #{id} をキャンセルしました。{extra}",
      "detail.requeue": "Requeue（再投入）",
      "detail.requeueFailed": "Requeue に失敗しました",
      "detail.requeued": "ジョブ #{id} を queued に戻しました。",
      "detail.requeueError": "Requeue エラー: {message}",
      "detail.notDeliverable": "このジョブはまだ完了／失敗していないため納品対象外です。",
      "detail.deliveryState": "納品状態",
      "detail.deliveredAt": "納品時刻",
      "detail.jobState": "ジョブ状態",
      "detail.childrenPartial": "子ジョブ一覧 ({count}件取得・一部取得失敗)",
      "detail.children": "子ジョブ一覧 ({count})",
      "detail.noChildrenFetched": "取得できた子ジョブはありません（一部取得に失敗しているため、未表示の子ジョブがある可能性があります）。",
      "detail.noChildren": "子ジョブはありません。",
      "detail.heading": "ジョブ #{id}: {title}",
      "detail.chainList": "チェーン一覧",
      "detail.backToList": "一覧に戻る",
      "detail.instructions": "指示文 (instructions)",
      "detail.acceptance": "受け入れ条件 (acceptance_criteria)",
      "detail.result": "結果 (result_text)",
      "detail.lastError": "エラー (last_error)",
      "detail.gateHistory": "ゲート判定履歴 ({count})",
      "detail.deliveryRecord": "納品記録",

      "newJob.title": "新規ジョブ登録",
      "newJob.metaFetchFailed": "メタ情報の取得に失敗しました",
      "newJob.regenerate": "再生成",
      "newJob.titlePlaceholder": "ジョブのタイトル（必須）",
      "newJob.modelPlaceholder": "任意（例: claude-sonnet-4-5）",
      "newJob.modelDefault": "（指定なし / デフォルト）",
      "newJob.noHostOption": "（対応ホストなし）",
      "newJob.noPools": "利用可能なプール候補がありません",
      "newJob.instructionsPlaceholder": "エージェントへの指示文",
      "newJob.acceptancePlaceholder": "受け入れ条件（任意）",
      "newJob.targetRefPlaceholder": "任意（例: origin/main や PR URL）",
      "newJob.submit": "ジョブを登録",
      "newJob.submitting": "送信中...",
      "newJob.required": "request_id と title は必須です。",
      "newJob.heldDefaultReason": "クォータゲートにより保持されました",
      "newJob.heldHeading": "ゲート保持 (HTTP 202 Accepted)",
      "newJob.heldReason": "理由: {reason}",
      "newJob.heldSnapshot": "スナップショット: {snapshot}",
      "newJob.checkHeld": "保持中ジョブ画面で確認する",
      "newJob.created": "ジョブを作成しました (HTTP 201 Created)",
      "newJob.existing": "既存のジョブを返しました（冪等再送 / HTTP 200 OK）",
      "newJob.createdSummary": "ID: #{id} / タイトル: {title} / 状態: {status}",
      "newJob.openDetail": "ジョブ #{id} の詳細を開く",
      "newJob.again": "新しい request_id を発行して続けて登録",
      "newJob.failed": "登録に失敗しました",
      "newJob.submitError": "送信エラー: {message}",
      "newJob.requestIDLabel": "request_id（自動生成・冪等キー）",
      "newJob.titleLabel": "タイトル (title) *",
      "newJob.poolLabel": "Pool（自動判定）",
      "newJob.parentLabel": "親ジョブ (parent_id)",
      "newJob.altPoolsLabel": "代替プール (alt_pools)",
      "newJob.draftLabel": "ドラフトとして登録する (draft)",
      "newJob.advanced": "詳細設定（worktree_spec / target_ref）",

      "held.title": "保持中ジョブ",
      "held.fetchFailed": "保持中ジョブの取得に失敗しました",
      "held.noMatch": "該当する保持ジョブはありません。",
      "held.release": "解放試行",
      "held.releasing": "評価中...",
      "held.released": "保持ジョブ {id} を解放しました（ジョブ #{jobID} を作成）。",
      "held.skippedDefaultReason": "条件未回復のため見送り",
      "held.skipped": "解放見送り ({id}): {reason}",
      "held.releaseFailed": "解放試行に失敗しました",
      "held.releaseError": "解放試行エラー: {message}",
      "held.cancelConfirm": "保持ジョブ {id} をキャンセルしますか？",
      "held.cancelled": "保持ジョブ {id} をキャンセルしました。",
      "held.titleAgentHost": "タイトル / agent・host",
      "held.snapshot": "判定スナップショット",
      "held.actions": "操作",

      "decisions.title": "ゲート判定履歴",
      "decisions.fetchFailed": "ゲート判定履歴の取得に失敗しました",
      "decisions.jobIDPlaceholder": "例: 42",
      "decisions.empty": "ゲート判定履歴はありません。",
      "decisions.decision": "判定",
      "decisions.mode": "モード",
      "decisions.target": "対象",
      "decisions.targetJob": "Job #{id}",
      "decisions.targetHeld": "Held {id}",
      "decisions.requestedPool": "要求プール",
      "decisions.selectedPool": "選択プール",
      "decisions.blockedBy": "ブロック / バイパス",
      "decisions.reason": "理由",
      "decisions.measuredAt": "測定時刻",
      "decisions.decidedAt": "判定時刻",

      "deliveries.title": "納品",
      "deliveries.undelivered": "未納品",
      "deliveries.delivered": "納品済み",
      "deliveries.fetchFailed": "納品一覧の取得に失敗しました",
      "deliveries.empty": "該当する納品対象ジョブはありません。",
      "deliveries.mark": "納品済みにする",
      "deliveries.markFailed": "納品済みの記録に失敗しました",
      "deliveries.marked": "ジョブ #{id} を納品済みにしました。",
      "deliveries.markError": "納品更新エラー: {message}",
      "deliveries.columnDelivery": "納品",
      "deliveries.columnExcerpt": "結果 / エラー抜粋",
      "deliveries.columnUpdatedAt": "更新時刻",
      "deliveries.columnAction": "納品操作 / 時刻",

      "agents.title": "エージェント版数",
      "agents.fetchFailed": "エージェント版数の取得に失敗しました",
      "agents.checkNow": "今すぐ確認",
      "agents.checking": "確認中...",
      "agents.probeFailed": "プローブ実行に失敗しました",
      "agents.probeDone": "プローブが完了しました（新規イベント: {count}件）。",
      "agents.probeError": "プローブエラー: {message}",
      "agents.driftAlert": "ドリフトを検出しています: {agents}",
      "agents.driftManual": "（自動更新不可・手作業が必要: {hosts}）",
      "agents.driftSummary": "ドリフト要約",
      "agents.matrix": "ホスト × エージェント 版マトリクス",
      // Shown for the server's limitation_note; a test keeps it equal to the server text.
      "agents.limitationNote": "遅れ判定はホスト間の最大版との相対比較です。全ホストが一斉に古い場合は検出できません。",
      "agents.events": "変化イベント履歴（直近50件）",
      "agents.notComparable": "比較不能",
      "agents.maxVersion": "最大版: {version}",
      "agents.laggingHosts": "遅れ: {hosts}",
      "agents.manualHosts": "要手作業: {hosts}",
      "agents.failedHosts": "失敗: {hosts}",
      "agents.noDetails": "未取得または差異なし",
      "agents.notConfigured": "ホストまたはエージェントが設定されていません。",
      "agents.unchecked": "未確認",
      "agents.manualBadge": "要手作業",
      "agents.pinMismatch": "pinned: {pin} (不一致)",
      "agents.behind": "遅れ",
      "agents.noEvents": "記録されたエージェント版数イベントはありません。",
      "agents.columnTime": "時刻",
      "agents.columnType": "種別",
      "agents.columnAgent": "エージェント",
      "agents.columnChange": "変化",
      "agents.columnDetail": "詳細",

      "notify.titleDone": "ジョブ #{id} が完了しました",
      "notify.titleFailed": "ジョブ #{id} が失敗しました",
      "notify.titleCancelled": "ジョブ #{id} がキャンセルされました",
      "notify.stateDone": "done（完了）",
      "notify.stateFailed": "failed（失敗）",
      "notify.stateCancelled": "cancelled（キャンセル）",
      "notify.body": "{title}\n状態: {state}",
      "notify.fatalUnauthorized": "通知ストリームの認証に失敗しました（401）。",
      "notify.fatalForbidden": "通知ストリームには read スコープのトークンが必要です（403）。",
      "notify.fatalDisabled": "サーバー側でジョブ通知が無効です（fleet.yaml の api.job_events）。",
      "notify.noToken": "通知には read スコープの API トークンが必要です。トークンなしの接続では利用できません。",
      "notify.insecureContext": "ブラウザ通知は HTTPS または localhost で開いた場合のみ利用できます。",
      "notify.unsupported": "このブラウザは通知に対応していません。",
      "notify.unsupportedIOS": "このブラウザは通知に対応していません。iOS / iPadOS では、ホーム画面に追加した PWA（iOS 16.4 以降）でのみ Web Push が届きます。",
      "notify.denied": "ブラウザで通知が許可されていないため有効にできません。ブラウザのサイト設定を確認してください。",
      "notify.showFailed": "このブラウザでは通知を表示できませんでした。",
      "notify.toggleOn": "ジョブ完了のブラウザ通知: オン",
      "notify.toggleOff": "ジョブ完了のブラウザ通知: オフ",

      "push.off": "Web Push: オフ",
      "push.removing": "Web Push: 登録を解除しています。",
      "push.unconfirmed": "Web Push: 解除を確認できませんでした。通知が届き続ける可能性があります。通知ボタンを押すと解除を再試行します。",
      "push.removed": "Web Push: 解除しました。",
      "push.pending": "Web Push: 登録を確認しています。",
      "push.active": "Web Push: 登録済みです。画面を閉じていても完了・失敗の通知が届きます。",
      "push.unsupported": "Web Push: この環境では使えません。通知はこの画面を開いている間だけ届きます。",
      "push.noToken": "Web Push: 登録の確認には API トークンが必要です。",
      "push.serverOff": "Web Push: サーバー側で無効です（fleet.yaml の api.web_push）。通知はこの画面を開いている間だけ届きます。",
      "push.unauthorized": "Web Push: 認証に失敗したため登録を確認できません（401）。",
      "push.forbidden": "Web Push: 登録には read と operate スコープのトークンが必要です（403）。このトークンでは新しく登録できません。",
      "push.error": "Web Push: 登録を確認できませんでした。画面を閉じている間は届かない可能性があります。",
      "push.iosNote": "iOS / iPadOS では、ホーム画面に追加した PWA（iOS 16.4 以降）でのみ Web Push が届きます。",
      "push.banner": "{message} {note}",
      "push.labelActive": "通知: Push",
      "push.labelPending": "通知: 確認中",
      "push.labelTabOnly": "通知: タブのみ",
      "push.labelUnchecked": "通知: Push 未確認",
      "push.labelRemoving": "通知: 解除中",
      "push.labelUnconfirmed": "通知: 解除未確認"
    },

    // Counts are written as "label: n" so that no plural forms are needed.
    en: {
      "lang.switchLabel": "Switch display language",
      "lang.ja": "JA",
      "lang.en": "EN",
      "lang.jaName": "日本語",
      "lang.enName": "English",

      "nav.dashboard": "Dashboard",
      "nav.jobs": "Jobs",
      "nav.newJob": "New job",
      "nav.held": "Held",
      "nav.decisions": "Gate decisions",
      "nav.deliveries": "Deliveries",
      "nav.agents": "Agent versions",

      "theme.switchLabel": "Switch display mode",
      "theme.dark": "Dark",
      "theme.darkLabel": "Dark (console)",
      "theme.light": "Light",
      "theme.lightLabel": "Light (ledger)",

      "header.notifyTitle": "Browser notifications for finished jobs (on iOS / iPadOS, Web Push only works in the PWA added to the Home Screen, iOS 16.4 or later)",
      "header.notify": "Notify",
      "header.refresh": "Refresh",
      "header.token": "Token",

      "auth.title": "API token authentication",
      "auth.description": "Enter a Bearer token to connect to the API. The token is kept only in this tab's sessionStorage.",
      "auth.tokenLabel": "Bearer token",
      "auth.tokenPlaceholder": "api.token in fleet.yaml",
      "auth.connect": "Connect",
      "auth.connectNoAuth": "Connect without a token",
      "auth.required": "Authentication required (401 Unauthorized)",
      "auth.connectFailed": "Connection failed",
      "auth.connectError": "Connection error: {message}",
      "auth.tokenMissing": "Enter a token.",

      "common.loading": "Loading...",
      "common.untitled": "(untitled)",
      "common.noTitle": "(no title)",
      "common.emptyText": "(none)",
      "common.noneOption": "(none)",
      "common.selectOption": "-- Select --",
      "common.all": "All",
      "common.filter": "Filter",
      "common.clear": "Clear",
      "common.cancel": "Cancel",
      "common.error": "Error: {message}",
      "common.jobLabel": "Job #{id}",
      "common.openJob": "Open job #{id}",
      "common.statusFetchFailed": "Failed to fetch the status",
      "common.cancelFailed": "Failed to cancel",
      "common.cancelError": "Cancel error: {message}",
      "common.newJobButton": "+ New job",

      "column.title": "Title",
      "column.status": "Status",
      "column.pool": "Pool",
      "column.holdReason": "Hold reason",
      "column.createdAt": "Created",
      "column.parentJob": "Parent job",
      "column.host": "Host",

      "status.done": "done",
      "status.claimed": "running",
      "status.queued": "queued",
      "status.waiting": "waiting",
      "status.failed": "failed",
      "status.cancelled": "cancelled",
      "status.draft": "draft",

      "chain.summaryEmpty": "Total: 0",
      "chain.summaryTotal": "Total: {total}",
      "chain.summaryBreakdown": "Total: {total} — {parts}",
      "chain.summaryItem": "{label}: {count}",
      "chain.summarySeparator": ", ",
      "chain.fetchNetworkError": "Network error",
      "chain.fetchInvalidResponse": "Invalid response format",
      "chain.fetchGenericError": "Fetch error",
      "chain.unknownParent": "unknown parent",
      "chain.unknownStatus": "unknown status",
      "chain.errorSeparator": "; ",
      "chain.errorMore": "; more failures: {count}",
      "chain.errorNotice": "Some child jobs could not be fetched, so the chain may be incomplete (failed: {items}{extra}). Only the jobs that were fetched are shown.",
      "chain.currentJob": "the current job",
      "chain.directErrorNotice": "Some direct child jobs of {target} could not be fetched, so the list may be incomplete (failed statuses: {items}). Only the jobs that were fetched are shown.",
      "chain.truncatedExpand": "the search limit left nodes unexplored: {count}",
      "chain.truncatedUp": "the ancestor search stopped at its limit",
      "chain.truncatedPages": "some child job lists reached the page limit",
      "chain.truncatedLimit": "search limit reached",
      "chain.truncatedSeparator": "; ",
      "chain.truncatedDirectComplete": " All direct child jobs of the current job were fetched.",
      "chain.truncatedNotice": "Parts of this large chain are omitted ({reasons}).{note}",
      "chain.tableHeading": "Chain / Job",
      "chain.orphanBefore": "(parent ",
      "chain.orphanAfter": " not loaded)",
      "chain.sectionTitle": "Chain",
      "chain.unavailable": "The chain could not be loaded.",
      "chain.currentTag": "Current",

      "form.noMatchingHost": "No host supports the selected agent ({agent}).",
      "form.derivedPool": "pool: {pool} (auto)",
      "form.derivedPoolNone": "pool: none (not determined automatically)",

      "page.notFound": "Page not found",
      "page.backToDashboard": "Back to the dashboard",

      "dashboard.title": "Dashboard",
      "dashboard.statusCounts": "Jobs by status",
      "dashboard.total": "total",
      "cooldown.badge": "Cooldown",
      "cooldown.until": "Cooldown ends in {minutes} min ({until})",
      "cooldown.hits": "{hits} consecutive hits",
      "cooldown.queued": "Waiting for pool cooldown to end ({until})",
      "cooldown.clear": "Clear cooldown",
      "cooldown.confirm": "Clear cooldown for pool {pool}?",
      "cooldown.clearFailed": "Could not clear cooldown.",
      "dashboard.unmeasured": "unmeasured (bypass)",
      "dashboard.weeklyRemaining": "Weekly remaining: {percent}%",
      "dashboard.shortRemaining": "Short-term remaining: {percent}%",
      "dashboard.resetLabel": "Reset:",
      "dashboard.resetLabelWithTz": "Reset ({tz}):",
      "dashboard.resetNone": "none",
      "dashboard.weeklyReset": "Weekly {time}",
      "dashboard.weeklyResetMissing": "Weekly none",
      "dashboard.shortReset": "{window} {time}",
      "dashboard.shortResetMissing": "{window} none",
      "dashboard.shortResetNotApplicable": "{window} N/A",
      "dashboard.measuredAt": "Measured: ",
      "dashboard.poolSummary": "Pool quota summary",
      "dashboard.runningJobs": "Running jobs ({count})",
      "dashboard.noRunningJobs": "No jobs are running.",
      "dashboard.heldJobs": "Held jobs ({count})",
      "dashboard.noHeldJobs": "No jobs are held.",
      "dashboard.recentDeliveries": "Recent deliveries ({count})",

      // Item names that read the same in both languages.
      "field.id": "ID",
      "field.jobID": "Job ID",
      "field.heldID": "Held ID",
      "field.requestID": "Request ID",
      "field.sessionID": "Session ID",
      "field.agent": "Agent",
      "field.host": "Host",
      "field.agentHost": "agent / host",
      "field.workdir": "Workdir",
      "field.model": "Model",
      "field.pool": "Pool",
      "field.altPools": "Alt Pools",
      "field.selectionMode": "Selection mode",
      "field.selectedTarget": "Selector choice",
      "field.selectionReason": "Selection reason",
      "field.targetRef": "Target Ref",
      "field.worktreeSpec": "Worktree Spec",
      "field.attempts": "Attempts",
      "field.claimedBy": "Claimed By",
      "field.claimedAt": "Claimed At",
      "field.createdAt": "Created At",
      "field.updatedAt": "Updated At",
      "field.deliveredAt": "Delivered At",

      "jobs.title": "Jobs",
      "jobs.empty": "No jobs.",
      "jobs.noMatch": "No matching jobs.",
      "jobs.noMatchingChain": "No matching chains.",
      "jobs.viewSwitchLabel": "Switch view",
      "jobs.viewFlat": "Flat view",
      "jobs.viewChain": "Chain view",
      "jobs.searchPlaceholder": "Title contains...",
      "jobs.searchLabel": "Search titles",
      "jobs.chainTruncated": "For some statuses ({statuses}), chains are built from only the latest 100 jobs.",
      "jobs.loadMore": "Load more",
      "jobs.listFetchFailed": "Failed to fetch the job list",
      "jobs.statusListFetchFailed": "Failed to fetch the job list ({status})",
      "jobs.loadMoreFailed": "Failed to load more",
      "jobs.loadMoreError": "Error while loading more: {message}",

      "detail.invalidID": "Invalid job ID: {id}",
      "detail.backToJobs": "Back to jobs",
      "detail.fetchFailed": "Failed to fetch job #{id}",
      "detail.cancelReasonPlaceholder": "Reason for cancelling (optional)",
      "detail.cancelConfirm": "Cancel job #{id}?",
      "detail.cascaded": " (also cancelled child jobs: {ids})",
      "detail.cancelled": "Job #{id} was cancelled.{extra}",
      "detail.requeue": "Requeue",
      "detail.requeueFailed": "Failed to requeue",
      "detail.requeued": "Job #{id} was returned to queued.",
      "detail.requeueError": "Requeue error: {message}",
      "detail.notDeliverable": "This job is not subject to delivery because it is neither done nor failed yet.",
      "detail.deliveryState": "Delivery status",
      "detail.deliveredAt": "Delivered",
      "detail.jobState": "Job status",
      "detail.childrenPartial": "Child jobs (fetched: {count}; some requests failed)",
      "detail.children": "Child jobs ({count})",
      "detail.noChildrenFetched": "No child jobs were fetched. Some requests failed, so there may be child jobs that are not shown.",
      "detail.noChildren": "No child jobs.",
      "detail.heading": "Job #{id}: {title}",
      "detail.chainList": "All chains",
      "detail.backToList": "Back to the list",
      "detail.instructions": "Instructions (instructions)",
      "detail.acceptance": "Acceptance criteria (acceptance_criteria)",
      "detail.result": "Result (result_text)",
      "detail.lastError": "Error (last_error)",
      "detail.gateHistory": "Gate decision history ({count})",
      "detail.deliveryRecord": "Delivery record",

      "newJob.title": "New job",
      "newJob.metaFetchFailed": "Failed to fetch metadata",
      "newJob.regenerate": "Regenerate",
      "newJob.titlePlaceholder": "Job title (required)",
      "newJob.modelPlaceholder": "Optional (e.g. claude-sonnet-4-5)",
      "newJob.modelDefault": "(unspecified / default)",
      "newJob.noHostOption": "(no supported host)",
      "newJob.noPools": "No pools are available",
      "newJob.instructionsPlaceholder": "Instructions for the agent",
      "newJob.acceptancePlaceholder": "Acceptance criteria (optional)",
      "newJob.targetRefPlaceholder": "Optional (e.g. origin/main or a PR URL)",
      "newJob.submit": "Create job",
      "newJob.submitting": "Submitting...",
      "newJob.required": "request_id and title are required.",
      "newJob.heldDefaultReason": "Held by the quota gate",
      "newJob.heldHeading": "Held by the gate (HTTP 202 Accepted)",
      "newJob.heldReason": "Reason: {reason}",
      "newJob.heldSnapshot": "Snapshot: {snapshot}",
      "newJob.checkHeld": "View on the held jobs page",
      "newJob.created": "Job created (HTTP 201 Created)",
      "newJob.existing": "Returned the existing job (idempotent resend / HTTP 200 OK)",
      "newJob.createdSummary": "ID: #{id} / Title: {title} / Status: {status}",
      "newJob.openDetail": "Open details of job #{id}",
      "newJob.again": "Issue a new request_id and create another",
      "newJob.failed": "Failed to create the job",
      "newJob.submitError": "Submit error: {message}",
      "newJob.requestIDLabel": "request_id (generated; idempotency key)",
      "newJob.titleLabel": "Title (title) *",
      "newJob.poolLabel": "Pool (automatic)",
      "newJob.parentLabel": "Parent job (parent_id)",
      "newJob.altPoolsLabel": "Alternate pools (alt_pools)",
      "newJob.draftLabel": "Create as a draft (draft)",
      "newJob.advanced": "Advanced settings (worktree_spec / target_ref)",

      "held.title": "Held jobs",
      "held.fetchFailed": "Failed to fetch held jobs",
      "held.noMatch": "No matching held jobs.",
      "held.release": "Try release",
      "held.releasing": "Evaluating...",
      "held.released": "Held job {id} was released (created job #{jobID}).",
      "held.skippedDefaultReason": "Conditions have not recovered yet",
      "held.skipped": "Not released ({id}): {reason}",
      "held.releaseFailed": "Failed to try a release",
      "held.releaseError": "Release error: {message}",
      "held.cancelConfirm": "Cancel held job {id}?",
      "held.cancelled": "Held job {id} was cancelled.",
      "held.titleAgentHost": "Title / agent, host",
      "held.snapshot": "Decision snapshot",
      "held.actions": "Actions",

      "decisions.title": "Gate decision history",
      "decisions.fetchFailed": "Failed to fetch the gate decision history",
      "decisions.jobIDPlaceholder": "e.g. 42",
      "decisions.empty": "No gate decisions.",
      "decisions.decision": "Decision",
      "decisions.mode": "Mode",
      "decisions.target": "Target",
      "decisions.targetJob": "Job #{id}",
      "decisions.targetHeld": "Held {id}",
      "decisions.requestedPool": "Requested pool",
      "decisions.selectedPool": "Selected pool",
      "decisions.blockedBy": "Blocked by / Bypass",
      "decisions.reason": "Reason",
      "decisions.measuredAt": "Measured",
      "decisions.decidedAt": "Decided",

      "deliveries.title": "Deliveries",
      "deliveries.undelivered": "Undelivered",
      "deliveries.delivered": "Delivered",
      "deliveries.fetchFailed": "Failed to fetch deliveries",
      "deliveries.empty": "No matching jobs for delivery.",
      "deliveries.mark": "Mark delivered",
      "deliveries.markFailed": "Failed to record the delivery",
      "deliveries.marked": "Job #{id} was marked as delivered.",
      "deliveries.markError": "Delivery update error: {message}",
      "deliveries.columnDelivery": "Delivery",
      "deliveries.columnExcerpt": "Result / error excerpt",
      "deliveries.columnUpdatedAt": "Updated",
      "deliveries.columnAction": "Delivery action / time",

      "agents.title": "Agent versions",
      "agents.fetchFailed": "Failed to fetch agent versions",
      "agents.checkNow": "Check now",
      "agents.checking": "Checking...",
      "agents.probeFailed": "Failed to run the probe",
      "agents.probeDone": "Probe finished (new events: {count}).",
      "agents.probeError": "Probe error: {message}",
      "agents.driftAlert": "Version drift detected: {agents}",
      "agents.driftManual": " (cannot auto-update; manual work needed: {hosts})",
      "agents.driftSummary": "Drift summary",
      "agents.matrix": "Version matrix: host × agent",
      "agents.limitationNote": "Lag is judged relative to the highest version among the hosts. It cannot be detected when every host is outdated.",
      "agents.events": "Change events (latest 50)",
      "agents.notComparable": "Not comparable",
      "agents.maxVersion": "Highest version: {version}",
      "agents.laggingHosts": "Behind: {hosts}",
      "agents.manualHosts": "Manual work needed: {hosts}",
      "agents.failedHosts": "Failed: {hosts}",
      "agents.noDetails": "Not fetched yet, or no differences",
      "agents.notConfigured": "No hosts or agents are configured.",
      "agents.unchecked": "Not checked",
      "agents.manualBadge": "Manual",
      "agents.pinMismatch": "pinned: {pin} (mismatch)",
      "agents.behind": "Behind",
      "agents.noEvents": "No agent version events recorded.",
      "agents.columnTime": "Time",
      "agents.columnType": "Type",
      "agents.columnAgent": "Agent",
      "agents.columnChange": "Change",
      "agents.columnDetail": "Detail",

      "notify.titleDone": "Job #{id} is done",
      "notify.titleFailed": "Job #{id} failed",
      "notify.titleCancelled": "Job #{id} was cancelled",
      "notify.stateDone": "done",
      "notify.stateFailed": "failed",
      "notify.stateCancelled": "cancelled",
      "notify.body": "{title}\nStatus: {state}",
      "notify.fatalUnauthorized": "Authentication for the notification stream failed (401).",
      "notify.fatalForbidden": "The notification stream requires a token with the read scope (403).",
      "notify.fatalDisabled": "Job notifications are disabled on the server (api.job_events in fleet.yaml).",
      "notify.noToken": "Notifications require an API token with the read scope. They are not available without a token.",
      "notify.insecureContext": "Browser notifications are available only over HTTPS or on localhost.",
      "notify.unsupported": "This browser does not support notifications.",
      "notify.unsupportedIOS": "This browser does not support notifications. On iOS / iPadOS, Web Push only arrives in the PWA added to the Home Screen (iOS 16.4 or later).",
      "notify.denied": "Notifications cannot be turned on because the browser does not allow them. Check the site settings in your browser.",
      "notify.showFailed": "This browser could not show the notification.",
      "notify.toggleOn": "Browser notifications for finished jobs: on",
      "notify.toggleOff": "Browser notifications for finished jobs: off",

      "push.off": "Web Push: off",
      "push.removing": "Web Push: removing the registration.",
      "push.unconfirmed": "Web Push: the removal could not be confirmed. Notifications may keep arriving. Press the notification button to try again.",
      "push.removed": "Web Push: removed.",
      "push.pending": "Web Push: checking the registration.",
      "push.active": "Web Push: registered. Notifications for done and failed jobs arrive even with this page closed.",
      "push.unsupported": "Web Push: not available here. Notifications only arrive while this page is open.",
      "push.noToken": "Web Push: an API token is needed to check the registration.",
      "push.serverOff": "Web Push: disabled on the server (api.web_push in fleet.yaml). Notifications only arrive while this page is open.",
      "push.unauthorized": "Web Push: the registration cannot be checked because authentication failed (401).",
      "push.forbidden": "Web Push: registering requires a token with the read and operate scopes (403). This token cannot make a new registration.",
      "push.error": "Web Push: the registration could not be checked. Notifications may not arrive while this page is closed.",
      "push.iosNote": "On iOS / iPadOS, Web Push only arrives in the PWA added to the Home Screen (iOS 16.4 or later).",
      "push.banner": "{message} {note}",
      "push.labelActive": "Notify: Push",
      "push.labelPending": "Notify: checking",
      "push.labelTabOnly": "Notify: tab only",
      "push.labelUnchecked": "Notify: Push unconfirmed",
      "push.labelRemoving": "Notify: removing",
      "push.labelUnconfirmed": "Notify: not confirmed off"
    }
  };

  function has(object, key) {
    return Object.prototype.hasOwnProperty.call(object, key);
  }

  function detectLocale(language) {
    return /^ja(?:[-_]|$)/i.test(String(language || "")) ? "ja" : "en";
  }

  // resolveLocale prefers the stored choice and otherwise follows the browser language.
  function resolveLocale(stored, language) {
    return LOCALES.indexOf(stored) >= 0 ? stored : detectLocale(language);
  }

  function browserLanguage() {
    return typeof navigator !== "undefined" && navigator ? navigator.language : "";
  }

  function readStoredLocale() {
    try {
      return localStorage.getItem(LOCALE_KEY);
    } catch (_err) {
      return null;
    }
  }

  // Outside a page (unit tests) there is no visitor to detect, so the source text is used.
  let current = SOURCE_LOCALE;

  function getLocale() {
    return current;
  }

  // t returns the message for key, replacing each {name} with params.name. Values are
  // inserted in one pass, so braces inside them are left alone.
  function t(key, params) {
    let message = key;
    if (has(MESSAGES[current], key)) {
      message = MESSAGES[current][key];
    } else if (has(MESSAGES[SOURCE_LOCALE], key)) {
      message = MESSAGES[SOURCE_LOCALE][key];
    }
    if (!params) {
      return message;
    }
    return message.replace(/\{(\w+)\}/g, function (placeholder, name) {
      return has(params, name) ? String(params[name]) : placeholder;
    });
  }

  // lazy returns a message that is looked up each time it is displayed. Pass it to
  // setText or setAttr (or to app.js el()) so that it follows a locale change.
  function lazy(key, params) {
    return function () {
      return t(key, params);
    };
  }

  // resolve turns a lazy message into its current text; other values pass through.
  function resolve(value) {
    return typeof value === "function" ? value() : value;
  }

  // Bindings are kept on the node, so they disappear together with it.
  function bind(node, target, value) {
    if (typeof value === "function") {
      if (!node.qfI18n) {
        node.qfI18n = new Map();
      }
      node.qfI18n.set(target, value);
    } else if (node.qfI18n) {
      node.qfI18n.delete(target);
    }
  }

  const TEXT_TARGET = "#text";

  function setText(node, value) {
    bind(node, TEXT_TARGET, value);
    node.textContent = String(resolve(value));
  }

  function setAttr(node, name, value) {
    bind(node, name, value);
    node.setAttribute(name, String(resolve(value)));
  }

  // translate redraws every message below node in place, which keeps form input,
  // focus and scroll position. Bindings win over the static data-i18n attributes.
  function translate(node) {
    if (node.getAttribute) {
      const key = node.getAttribute("data-i18n");
      if (key) {
        node.textContent = t(key);
      }
      for (const name of I18N_ATTRIBUTES) {
        const attrKey = node.getAttribute("data-i18n-" + name);
        if (attrKey) {
          node.setAttribute(name, t(attrKey));
        }
      }
    }
    if (node.qfI18n) {
      for (const entry of node.qfI18n) {
        const text = String(entry[1]());
        if (entry[0] === TEXT_TARGET) {
          node.textContent = text;
        } else {
          node.setAttribute(entry[0], text);
        }
      }
    }
    if (node.childNodes) {
      for (const child of Array.from(node.childNodes)) {
        translate(child);
      }
    }
  }

  function syncLocaleButtons() {
    const container = document.getElementById("locale-switch");
    if (!container) {
      return;
    }
    for (const btn of container.querySelectorAll("[data-set-locale]")) {
      const active = btn.getAttribute("data-set-locale") === current;
      btn.classList.toggle("active", active);
      btn.setAttribute("aria-pressed", active ? "true" : "false");
    }
  }

  function applyToDocument() {
    if (typeof document === "undefined") {
      return;
    }
    document.documentElement.setAttribute("lang", current);
    translate(document.documentElement);
    syncLocaleButtons();
  }

  // setLocale switches the language of the whole page. persist stores the choice; an
  // automatically detected language is never stored.
  function setLocale(locale, persist) {
    current = resolveLocale(locale, browserLanguage());
    if (persist) {
      try {
        localStorage.setItem(LOCALE_KEY, current);
      } catch (_err) {
        // Storage may be unavailable; the choice still applies to this page.
      }
    }
    applyToDocument();
    return current;
  }

  const api = {
    LOCALE_KEY: LOCALE_KEY,
    SOURCE_LOCALE: SOURCE_LOCALE,
    LOCALES: LOCALES,
    MESSAGES: MESSAGES,
    detectLocale: detectLocale,
    resolveLocale: resolveLocale,
    getLocale: getLocale,
    setLocale: setLocale,
    t: t,
    lazy: lazy,
    resolve: resolve,
    setText: setText,
    setAttr: setAttr,
    translate: translate
  };

  if (typeof module !== "undefined" && module.exports) {
    module.exports = api;
  }
  if (root) {
    root.__qfI18n = api;
  }

  if (typeof document !== "undefined" && typeof window !== "undefined") {
    setLocale(readStoredLocale(), false);
  }
})(typeof globalThis !== "undefined" ? globalThis : (typeof window !== "undefined" ? window : null));
