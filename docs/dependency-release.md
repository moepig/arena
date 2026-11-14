# 依存更新とリリース

依存更新の自動マージと、リリース PR から公開する手順を説明する。Dependabot、release-please、GitHub Actions を使い、認証には GITHUB_TOKEN を使う。

## 公開対象と互換性

公開 API、CLI の引数と出力、設定形式、保存データ、最低 Go バージョンを互換性の対象とする。GitHub Release はソースを公開する。コンテナや実行ファイルの配布は行わない。

## 設定

[Dependabot 設定](../.github/dependabot.yml)は更新対象とコミットの prefix、[release-please 設定](../release-please-config.json)と [manifest](../.release-please-manifest.json)は製品バージョンを管理する。必須チェックは GitHub Actions 提供の ci とする。

アプリケーション依存の prefix は fix、Actions 更新は chore にする。依存ごとの配布物への影響を解析しないため、テスト専用依存の更新もリリース候補になり得る。ビルドに影響する Actions 更新をリリースに含める場合は、管理者が PR タイトルを fix で始まる形式に直してからマージする。

## 自動マージ

[Dependabot auto-merge](../.github/workflows/dependabot-auto-merge.yml)は fetch-metadata で署名と更新種別を確認する。minor・patch は bot が承認し、対象 SHA の squash auto-merge を予約する。major、更新種別不明、draft、breaking-change ラベル付きの PR は予約を解除し、管理者が確認して手動でマージする。

導入時は main に ci、最新ベースでの検証、レビュー、push 後の承認無効化を設定する。Actions の PR 作成・承認も許可する。設定と CI の成功を確認してから、リポジトリ変数 DEPENDABOT_AUTO_MERGE を true にする。

## 公開

[Release Please](../.github/workflows/release-please.yml)は main への push と手動実行で起動する。定期実行は行わない。前回リリース以降に fix・feat などの対象変更があれば、リリース PR を作成する。既存 PR があれば更新し、対象変更がなければ新規 PR は作成しない。

GITHUB_TOKEN による依存 PR の自動マージでは push ワークフローが起動しない。自動マージ後に PR を作成・更新するには、Actions の Release Please から Run workflow を選び、main を指定して実行する。

PR の作成から公開までの流れを、次の図に示す。

```mermaid
flowchart TD
    trigger["main への push または手動実行"] --> changes{"未公開の fix・feat などがあるか"}
    changes -->|ない| none["新規リリース PR は作成しない"]
    changes -->|ある| pr["リリース PR を作成・更新"]
    pr --> review["管理者が CI と内容を確認"]
    review --> merge["リリース PR を main へ手動マージ"]
    merge --> rerun["マージ時の push で公開処理を自動開始"]
    rerun --> publish["タグを作成してソースを公開"]
```

リリース PR は管理者が手動でマージする。CI が承認待ちなら開始を承認し、バージョン、変更履歴、CI、互換性を確認する。マージに伴う main への push で Release Please が自動起動し、タグと GitHub Release を作成する。公開を始めるための追加操作は不要である。

release-please がソースの GitHub Release を直接公開する。公開を止める場合は Release Please を無効にし、停止中はリリース PR をマージしない。

既存リリースがないため、manifest の基準バージョンは仮に 0.1.0 としている。導入 PR で基準バージョンと bootstrap-sha を確認し、異なる値を使う場合は設定を修正する。

セキュリティ修正の根拠登録は行わず、GitHub 標準の通知から公開時期を判断する。自動マージを止める場合は DEPENDABOT_AUTO_MERGE を false にし、予約済み PR の auto-merge も解除する。
