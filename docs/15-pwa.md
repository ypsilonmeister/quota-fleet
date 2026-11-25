# docs/15 — UI の PWA 化

- 状態: 実装（Issue #13）
- 前提: docs/07（UI）、docs/14（ジョブ完了のブラウザ通知）

## 1. 目的と範囲

`/ui/` をホーム画面へインストールし、独立したアプリ（standalone）として起動できるようにする。通知用の Service Worker（docs/14）は置き換えず拡張する。

非目的: Web Push（Issue #7）、API レスポンスのオフラインキャッシュ、ページ内の独自インストールボタン（ブラウザ標準の導線を使う）。

インストールと Service Worker はセキュアコンテキスト（HTTPS または localhost）が前提。`serve` は HTTP で待ち受けるので、HTTPS は前段で用意する。

## 2. Web App Manifest

`internal/ui/assets/manifest.webmanifest` を `GET /ui/manifest.webmanifest` で配信する（`Content-Type: application/manifest+json`、`Cache-Control: no-cache`、無認証）。

| 項目 | 値 | 決め方 |
|---|---|---|
| `id` | `/ui/` | 将来 `start_url` を変えても同じアプリと扱われるように固定する |
| `name` / `short_name` | `quota-fleet` | |
| `start_url` / `scope` | `/ui/` | UI のパス。API（`/jobs` など）はスコープ外 |
| `display` | `standalone` | |
| `theme_color` | `#0c0d10` | ダーク（既定の表示）の `--chrome-bg`。上部バーの色 |
| `background_color` | `#111215` | ダークの `--bg`。起動画面の地の色 |

manifest の色は 1 組しか持てないため、既定のダークに合わせる。ページを開いた後は `direction.js` が `<meta name="theme-color">` を表示モードの `--chrome-bg`（ダーク `#0c0d10`、ライト `#efe9dc`）に書き換える。色の一致はテストで固定している。

### アイコン

外部素材は使わず、`internal/ui/gen_icons.go`（Go 標準ライブラリのみ）で描く。図柄は利用枠のゲージに見立てた 3 本の棒で、色はダークの `--bg` / `--info` / `--success` / `--warn`。

| ファイル（`internal/ui/assets/`） | サイズ | 用途 |
|---|---|---|
| `icon-192.png` | 192 | `purpose: any`（角丸、角は透過）。favicon にも使う |
| `icon-512.png` | 512 | `purpose: any` |
| `icon-maskable-512.png` | 512 | `purpose: maskable`。全面塗り、図柄は中央の半径 40% の円内 |
| `apple-touch-icon.png` | 180 | iOS。全面塗り（角は iOS が切り抜く） |

作り直すときは `go generate ./internal/ui` を実行し、PNG をコミットする。

### index.html

manifest の link、`theme-color`、`mobile-web-app-capable`、`apple-mobile-web-app-capable`、`apple-mobile-web-app-status-bar-style`（`black`。本文がステータスバーに重ならない）、`apple-mobile-web-app-title`、`apple-touch-icon` を置く。

## 3. CSP

`ui.ContentSecurityPolicy`（`default-src 'self'`）は変更していない。`manifest-src`、`img-src`、`worker-src`（`child-src`、`script-src` を経由）、`connect-src` はいずれも `default-src` に帰着し、manifest・アイコン・Service Worker はすべて同一オリジンから配信するため。

## 4. Service Worker のキャッシュ

### 登録

`app.js` が起動時に常に登録する。docs/14 の実装では通知が有効なときだけ登録していたが、アプリシェルのキャッシュは通知の設定と無関係なため変更した。登録先（`/ui/sw.js`、スコープ `/ui/`）は同じ。

`register()` は、進行中の更新のインストール（シェル取得を含む）が終わるまで解決しない。そのため `notify.js` は、有効な Service Worker を持つ登録がすでにあれば（`getRegistration`）それをすぐに使い、`register()` の完了を待たない。

登録後、ページは有効な Service Worker に `{ type: "qf-cache-shell" }` を送る（`notify.js` の `cacheShell`）。送るのはページの読み込み時と、ブラウザがオンラインに復帰したとき（`online` イベント）。Service Worker は、キャッシュに欠けがあれば一式を取得し、揃っていれば何もしない。

### 対象と対象外の線引き

許可リスト方式にする。`shellCacheKey` がキーを返したリクエストだけに `respondWith` し、それ以外は何もしない（ブラウザが Service Worker なしと同じように処理する）。

スコープが制限するのは「どのページが制御されるか」であり、制御下のページが出す `/jobs` や `/events` への `fetch` も `fetch` イベントになる。そのため除外はパスの許可リストで行う。

| 対象（同一オリジンの GET のみ） | キャッシュのキー |
|---|---|
| UI ページへの遷移（`/ui/`、`/ui/jobs`、`/ui/jobs/new`、`/ui/jobs/{id}`、`/ui/gate/held`、`/ui/gate/decisions`、`/ui/deliveries`、`/ui/agents`。クエリは問わない） | `/ui/`（どれも同じ index.html） |
| `SHELL_ASSETS`（CSS、JS、manifest、アイコン）のパス完全一致、クエリなし | そのパス |

対象外はそれ以外のすべて。API（`/jobs`、`/status`、`/meta`、`/deliveries`、`/gate/*`、`/agents*` など）、SSE（`/events`）、`/mcp`、GET 以外のメソッド、別オリジン、`/ui/sw.js` 自身、未知の `/ui/` パス。

### 取得の順序

- **ネットワーク優先**。オンラインではサーバーの応答をそのまま返すので、デプロイした変更はすぐ反映される。エラー応答（5xx など）もそのまま返す。
- ネットワークに到達できず `fetch` が失敗したときだけキャッシュを返す。
- キャッシュに書くのはシェル一式の一括取得（`addAll`、HTTP キャッシュを経由しない）だけ。取得するのは更新版のインストール時と、キャッシュが欠けている状態でページから依頼されたとき。通常のリクエストの応答は書かないので、キャッシュは常に一度に取得した一式になる。

オフラインでできるのは UI の枠が開くことまでで、API を使う画面はエラーになる。

### キャッシュ名とバージョン

キャッシュ名は `qf-shell-<version>`。`<version>` は、index.html、`shellAssets` の全ファイルとそのパス、`sw.js` の原文、CSP をまとめた SHA-256 の先頭 12 桁で、サーバーが起動時に `sw.js` の `__QF_SHELL_VERSION__` を置き換える。手で番号を上げる作業はない。CSP を含めるのは、キャッシュした応答がヘッダーも保持するため。

### インストールの失敗

- 有効な Service Worker がすでにある場合（更新）: 一括取得の完了を待ってから有効化する。失敗したら新しい版のインストールを中止し、古い版が残る。
- 初回: 一括取得を待たずに有効化する。通知の表示には有効な Service Worker が必要で、`notify.js` はそれを 10 秒しか待たないため。キャッシュは有効化後にページからの依頼で作る。
- 初回の取得に失敗した場合は、次の依頼（ページの読み込み、オンライン復帰）で同じ版のまま取り直す。サーバーだけが停止して復旧した場合は `online` イベントが起きないので、次のページ読み込みが契機になる。
- 初回の有効化の直後は、取得が終わるまでオフライン用のキャッシュがない。
- ページからの依頼で取得する内容は、その時点でサーバーが配信している版になる。デプロイ直後は Service Worker より新しいファイルが入ることがあり、新しい版の有効化で置き換わる。

## 5. Service Worker の更新手順

1. UI のファイルを変更する。
   - 静的ファイルを追加する場合は、`embed.go` の `//go:embed` と `shellAssets`、`sw.js` の `SHELL_ASSETS`、`pwa_test.go` の一覧を揃える。
   - ページのルートを追加する場合は、`embed.go`、`sw.js` の `isShellPage`、`pwa_test.go` の `pages` を揃える（Go 側だけの追加はテストで検出できない）。
2. `go test ./...` を実行し、ビルドしてデプロイ（`serve` を再起動）する。
3. ブラウザは、UI ページの読み込み時などに `/ui/sw.js`（`Cache-Control: no-store`）を取り直し、内容が違えば新しい版をインストールする。
4. 新しい版は一式を取得してから、すぐに有効化する（`skipWaiting`）。古い `qf-shell-*` キャッシュを削除し、開いているタブを引き継ぐ（`clients.claim`）。
5. 開いたままの画面の HTML や JS は差し替わらない。再読み込みかアプリの再起動で新しい版になる。画面内の移動はページの読み込みではないため、それを機にした更新確認は起きない。

確認するには、開発者ツールの Application で Service Worker とキャッシュ名を見る。

## 6. 制限

- トークンは従来どおり sessionStorage に保持する（docs/07）。インストールしたアプリを終了して起動し直すと、再入力が必要になる見込み。
- 通知は開いているページの SSE 受信が起点で、アプリが停止している間は届かない（Web Push は Issue #7）。
- 通知クリックの挙動（docs/14 の実装）は変えていない。
