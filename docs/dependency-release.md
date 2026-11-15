# 依存更新とリリース

arena の CI と配布方式を示す。設定の導入と共通操作は [管理リポジトリの運用手順](https://github.com/moepig/github/blob/main/docs/operations/dependency-release.md)に従う。管理設定と実イベントの受入検証を終えてから、自動マージを有効にする。

## 公開対象と互換性

ソースを公開する。

Release Please はルートの 1 パッケージを管理する。既存の manifest と bootstrap-sha を公開済み版に合わせて維持し、依存先の major 更新だけで製品の major を上げない。互換性を壊す製品変更は Conventional Commits の BREAKING CHANGE に記録する。

## 設定

CI、自動マージ、リリースは .github/workflows に定義する。保護対象は CODEOWNERS で指定し、配布するファイル名と GHCR の配布先は [.github/scripts/release-policy.json](../.github/scripts/release-policy.json)に限定する。

専用 App の Contents: write と Pull requests: write を、依存 PR の承認・予約と Release Please に使う。秘密鍵は main だけを許可した automation Environment に置く。ビルドは読み取り権限、公開は GITHUB_TOKEN の必要な書き込み権限を使う。App ID と bot の識別子は管理リポジトリの Terraform 設定を正本とする。

## 自動マージ

署名を検証した Dependabot の同一リポジトリ・main 向けのアプリケーション依存で、minor・patch だけを対象とする。major、種別不明、Actions、Docker、draft は手動判断とする。必須 ci、最新ベース、レビュー条件を GitHub が確認する。承認と予約には確認した head SHA を指定する。

保留には draft を使い、API で状態と予約解除を確認する。breaking-change ラベルは保留条件にしない。metadata 取得失敗や対象外では予約解除を試み、失敗も通知する。自動マージを無効にしても予約解除を省略しない。

## 公開

App による依存 PR の実際のマージが main の push を起こし、Release Please がリリース PR を作成・更新する。PR の CI は自動で開始する。人がバージョン、変更履歴、互換性、CI を確認して手動マージする。

その push でタグと draft を作成する。同じ release-please.yml の後続ジョブが対象を検査し、必要なビルドと公開を行う。ソースのみの場合はビルドを省略する。公開ジョブは release-manifest.json を最初に添付し、既存の配布物と照合して不足分だけを追加する。

全配布物を照合して draft を解除した後、SemVer の最大の正式版だけで Latest を更新する。古い版やプレリリースは Latest を変更しない。既存の添付やバージョンタグを上書きしない。

## 失敗時の再実行

元の実行が残っていれば失敗したジョブを再実行する。期間超過や出力喪失時は、main の release-please.yml に既存の release_id だけを指定する。タグや SHA を任意入力しない。

復旧は専用 App の PR、人による最終 head の承認、タグ、main の履歴、manifest の版を照合する。元の artifact を再利用するときも実行元と digest を検査する。記録と配布物が一致しない場合は停止する。公開済み Release の復旧は Latest の修復だけを行う。

停止、再開、鍵の失効、受入記録は [共通の操作手順](https://github.com/moepig/github/blob/main/docs/operations/dependency-release.md)に従う。
