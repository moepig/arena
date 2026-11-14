# 依存更新とリリース

Dependabot、GitHub Actions、release-please の標準機能で依存更新と公開を行う。ワークフローの認証は GITHUB_TOKEN だけを使う。独自 Action や状態管理用アプリケーションは設けない。

## 公開対象と互換性

公開 API、CLI の引数と出力、設定形式、保存データ、最低 Go バージョンを互換性の対象とする。GitHub Release はソースを公開する。コンテナや実行ファイルの配布は行わない。

## 設定

[Dependabot](../.github/dependabot.yml)に更新対象とコミットの prefix を定義する。[release-please 設定](../release-please-config.json)と [manifest](../.release-please-manifest.json)で単一の製品バージョンを管理する。必須 CI は ci とする。

アプリケーション依存の prefix は fix、Actions 更新は chore とする。依存ごとの配布物への影響は解析しないため、テスト専用依存でもリリース対象になり得る。ビルドに影響する Actions 更新を含める場合は、管理者が fix で始まる PR タイトルへ修正してマージする。

## 自動マージ

[Dependabot auto-merge](../.github/workflows/dependabot-auto-merge.yml)は fetch-metadata の署名検証と更新種別を使う。minor・patch を bot が承認し、指定した SHA の squash auto-merge を予約する。major、更新種別不明、draft、breaking-change ラベル付きの PR は管理者が確認して手動でマージする。

main に GitHub Actions 提供の ci、最新ベースでの検証、レビュー、push 後の承認無効化を設定する。Actions の PR 作成・承認も許可する。CI と保護設定を確認してから、リポジトリ変数 DEPENDABOT_AUTO_MERGE を true にする。

## 公開

[Release Please](../.github/workflows/release-please.yml)がリリース PR を作成する。GITHUB_TOKEN が作成・更新した PR の CI が承認待ちになった場合は、管理者が開始を承認する。バージョン、変更履歴、CI、互換性を確認してリリース PR を手動でマージする。

release-please がタグと GitHub Release を公開する。main への push のほか、30 分ごとの定期実行と手動起動でも処理する。公開を止める場合は Release Please を無効にし、リリース PR をマージしない。

既存リリースがないため、manifest の基準版は仮に 0.1.0 としている。導入 PR のレビューで基準版と bootstrap-sha を確認し、異なる値を使う場合は設定を修正する。

セキュリティ修正の独自判定と根拠登録は行わない。GitHub 標準の通知から公開時期を判断する。自動マージを止める場合は変数を false にし、既存 PR の予約も解除する。
