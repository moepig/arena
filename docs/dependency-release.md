# 依存更新とリリース

ECS 上のゲームサーバー管理サービスである。GitHub Release ではソースとタグを公開する。AWS リソースの作成とサービスのデプロイは利用環境の構成に従う。

## フローの関係

依存更新のマージとリリース PR のマージは、別の PR・別の main 更新である。依存 PR は条件を満たすと自動マージし、リリース PR は人がレビューしてマージする。

```mermaid
flowchart TD
    dependency["Dependabot の依存 PR"] --> auto["自動マージフロー"]
    auto -->|"必須 CI・レビュー条件を満たす"| merged["依存 PR を main へマージ"]
    merged -->|"push"| candidate["Release Please がリリース候補を判定"]
    candidate -->|"公開対象の変更あり"| releasePR["App がリリース PR を作成・更新"]
    candidate -->|"対象の変更なし"| done["終了"]
    releasePR -->|"pull_request"| ci["リリース PR の CI"]
    ci -->|"成功"| review["人がバージョン・変更履歴をレビュー"]
    review -->|"承認・マージ"| releaseMerge["リリース PR を main へマージ"]
    releaseMerge -->|"push"| publish["リリースフローで公開"]
```

App token による依存 PR のマージとリリース PR の作成により、通常の push・pull_request を起点に処理をつなぐ。通常の開発 PR のマージも、main の push からリリース候補の判定へ進む。

## CI と配布の確認

Go の vet・race test・build と、DynamoDB Local・Valkey・floci を使う統合テストを必須 ci に集約する。詳細は [開発手順](development/workflow.md)を参照する。

## 導入条件

専用 GitHub App、main 限定の automation Environment、必須 ci、レビュー、CODEOWNERS、immutable releases を設定してから導入する。実イベントの受入が済むまで DEPENDABOT_AUTO_MERGE は false とする。

## 依存更新

Dependabot の署名検証済み metadata と YAML の条件を使う。単一のアプリケーション依存の minor・patch だけを自動承認する。複数依存のグループ、major、不明、Actions、Docker は手動レビューとする。

既存予約を解除してから判定し、gh api で対象 SHA を承認する。gh pr merge --auto --squash --match-head-commit で予約し、必須 CI とレビューを GitHub が判定する。github-script は現在の PR を取得する API 呼び出しだけに使う。

自動マージは [dependabot-auto-merge.yml](../.github/workflows/dependabot-auto-merge.yml)で行う。PR の CI は [ci.yml](../.github/workflows/ci.yml)が pull_request で別に実行する。

```mermaid
flowchart TD
    event["依存 PR の作成・更新<br/>pull_request_target"] --> author{"同一リポジトリの<br/>Dependabot PR"}
    author -->|"いいえ"| skip["ジョブをスキップ"]
    author -->|"はい"| current["App token を発行<br/>現在の PR を取得・既存予約を解除"]
    current --> metadata["fetch-metadata で署名と更新情報を検証"]
    metadata -->|"成功"| eligible{"有効化済み・open・非 draft<br/>main 宛て・head SHA 一致<br/>単一の Go modules 依存の minor・patch"}
    metadata -->|"失敗"| failed["失敗として停止"]
    eligible -->|"対象外"| manual["自動承認・新規予約なし<br/>人が内容を判断"]
    eligible -->|"対象"| approve["App が対象 SHA を承認"]
    approve --> reserve["gh pr merge で auto-merge を予約<br/>head SHA を指定"]
    reserve --> rules["GitHub が必須 CI・レビュー・<br/>最新ベースの条件を判定"]
    rules -->|"条件を満たす"| merge["squash merge → main の push"]
```

予約時点で CI が実行中なら、GitHub が条件の成立を待つ。CI の失敗、必要なレビューの不足、draft の状態ではマージしない。取得・承認・予約などの操作が失敗した場合は、その実行を停止して原因を確認する。

## リリース

main の push で Release Please がリリース PR を作成・更新する。人がバージョン、変更履歴、CI を確認してマージする。設定は release-please-config.json と .github/workflows/release-please.yml に置く。

ソース公開は Release Please が行う。

GitHub の immutable releases で公開後の添付とタグを保護する。Latest は GitHub の標準動作に従う。

人がリリース PR をマージした後の処理を示す。[release-please.yml](../.github/workflows/release-please.yml)が main の push で実行する。

```mermaid
flowchart TD
    start["リリース PR のマージによる main の push"] --> create
    create["Release Please がタグと<br/>ソース Release を公開"] --> immutable["immutable releases が<br/>公開済みタグを保護"]
    immutable -.-> deploy["AWS への配置は利用環境の手順で実施"]
```

破線の配置作業は、この GitHub Actions ワークフローでは実行しない。

## リリース時に人が行う操作

[導入条件](#導入条件)の設定後、App が作成したリリース PR を開き、次の順で操作する。CI またはレビュー条件を満たさない場合は、マージを保留する。

| 操作 | 方法 |
| --- | --- |
| リリース内容の確認 | Files changed でバージョンと変更履歴を確認する |
| CI の確認 | PR の Checks で必須 ci の成功を確認する |
| [PR の承認](https://docs.github.com/en/pull-requests/how-tos/review-pull-requests/approving-a-pull-request-with-required-reviews) | Files changed → Review changes → Approve → Submit review |
| [公開を開始するマージ](https://docs.github.com/en/pull-requests/how-tos/merge-and-close-pull-requests/merging-a-pull-request) | PR 下部の Squash and merge を選び、マージを確定する |
| 公開処理の確認 | Actions → Release Please でマージ後の main の実行が成功したか確認する |
| 成果物の確認 | Releases で対象タグのソース Release が公開済みか確認する |
| AWS へデプロイする場合 | [AWS 配置条件](arena/operations.md#aws-配置条件)を満たし、公開タグのコードを環境別の手順で配置する |

マージ後のタグ作成と Release の公開はワークフローが行う。失敗した場合は Actions のログを確認し、[再実行の条件](#失敗時の再実行)に従う。

## 失敗時の再実行

配布先への書き込み前なら、失敗したジョブを再実行する。一部配布後は再実行を止め、管理者が draft、添付、GHCR を調査する。原則として修正版を別バージョンで公開する。同名ファイルを上書きしない。

## 停止と再開

有効化変数を false にし、ワークフローを無効化する。実行中・待機中の実行をキャンセルし、終了を確認してから各 PR の auto-merge を解除する。最後に実行と予約を再確認する。公開停止中はリリース PR をマージしない。

管理者による App・保護設定の操作は、[GitHub 設定の管理](https://github.com/moepig/github/blob/automation/dependency-release/docs/management.md)を参照する。
