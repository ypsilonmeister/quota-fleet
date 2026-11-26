# PR #29（Issue #25）の測定ログ

PR 本文の測定表を作ったときの実行ログ。ログと集計出力は内容を編集せずに置いている。

## 構成

| パス | 内容 |
|---|---|
| `logs/base_<テスト名>.log` | 修正前（main `ec5f08e`）通常、`-test.count=300` |
| `logs/base.race_<テスト名>.log` | 修正前 `-race`、`-test.count=150` |
| `logs/after_norace_<テスト名>.log` | 修正後 通常、`-count=300` |
| `logs/after_race_<テスト名>.log` | 修正後 `-race`、`-count=300` |
| `logs/baseline_summary.txt` / `logs/after_summary.txt` | 下記スクリプトの出力（件数、修正後は最大所要時間も） |
| `scripts/baseline.sh` / `scripts/after.sh` | ログを生成したスクリプト |
| `baseline-binaries.txt` | 修正前の測定に使ったバイナリの `go version -m`（依存モジュールの行は省略） |
| `SHA256SUMS` | このディレクトリのファイルのハッシュ（`README.md` と自身を除く） |

## 表との照合

このディレクトリで実行する。出力は「失敗数」「実行数」「`DATA RACE` の行数」「最大所要時間」。

```sh
for f in logs/*.log; do
  echo "$(basename "$f") fail=$(grep -c '^--- FAIL' "$f") run=$(grep -c '^=== RUN' "$f") race=$(grep -c 'DATA RACE' "$f") max=$(sed -nE 's/^--- (PASS|FAIL): .* \(([0-9.]+)s\)$/\2/p' "$f" | sort -g | tail -1)s"
done
```

修正前の失敗理由は、次の出力で確認できる（テストの出力行は serve の終了待ちタイムアウトの2種類の文言だけ）。

```sh
grep -h '^    ' logs/base*.log | sort | uniq -c
```

## ログだけでは確認できないこと

- 修正前のバイナリのビルドコマンドは保存していない（`go test -c`、`-race` 版は `CGO_ENABLED=1`）。バイナリはここに含めていない。`go version -m` の出力にコミットの情報はない。失敗行の行番号（297 / 510 / 879 / 1067）は `ec5f08e` の `cmd/quota-fleet/main_test.go` の `t.Fatalf` の行と一致する。
- 修正後の実行はコミット `af8d104` の作成前に作業ツリーで行った。ログにコミットの情報はない。
- CPU 数を記録した出力は保存していない。Go のバージョンは、修正前のバイナリの分だけ `baseline-binaries.txt` で確認できる。
