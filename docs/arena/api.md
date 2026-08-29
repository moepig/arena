# コントロールプレーン API

本ドキュメントは、`arena-api` が公開する RPC、共通規則、認証と認可、エラー処理を説明する。各 message の完全な schema は `api/proto/arena/v1` が定義する。

## Transport

`arena-api` は Connect、gRPC、gRPC-Web を同一 port で提供する。平文 endpoint では h2c を使用する。Connect JSON の procedure path は `/arena.v1.<Service>/<Method>` である。

生成済み OpenAPI document は `gen/openapi/arena.v1.yaml` である。このファイルは `buf generate` の出力であり、直接編集しないこと。

Namespace を持つ request で値を省略した場合は `default` を使用する。List API の page size は既定 100、上限 1000 である。次ページには response の `next_page_token` を渡す。

## FleetService

Fleet を扱う RPC を、以下に示す。

| RPC | 振る舞い |
| --- | --- |
| `CreateFleet` | Fleet を新規作成する |
| `GetFleet` | namespace と name で Fleet を取得する |
| `ListFleets` | namespace 内の Fleet を列挙する |
| `UpdateFleet` | `version` を用いる楽観ロックで labels と spec を置換する |
| `DeleteFleet` | active GameServer が存在しない Fleet を削除する |
| `ScaleFleet` | autoscaling が無効な Fleet の希望台数を変更する |
| `ApplyFleet` | namespace と name を key に作成または更新する |

`UpdateFleet` の `version` が保存済みの値と異なる場合は `ABORTED` となる。`ApplyFleet` は最大 3 回の read-modify-write を内部で試行するため、client は version を指定しない。

`ApplyFleet` の `dry_run` は書き込みを行わず、作成、更新、変更なしの action、正規化済み spec、現在との差分を返す。Autoscaling が有効な spec に `replicas` を含めると `FAILED_PRECONDITION` となる。Autoscaler が希望台数を所有するためである。

Template が変化すると `generation` が増加し、`spec_hash` が更新される。

## GameServerService

GameServer を扱う RPC を、以下に示す。

| RPC | 振る舞い |
| --- | --- |
| `GetGameServer` | GameServer ID で取得する |
| `ListGameServers` | namespace、Fleet、state で絞り込んで列挙する。`fleet_name` は必須 |
| `DeleteGameServer` | 対象を drain または unhealthy にして、controller に停止させる |

`DeleteGameServer` は Fleet の希望台数を変更しない。Controller は削除した GameServer の代替を起動する。

## AllocationService

Allocation を扱う RPC を、以下に示す。

| RPC | 振る舞い |
| --- | --- |
| `Allocate` | Ready GameServer を割り当てる |
| `Release` | Allocation を解放する |
| `GetAllocation` | Allocation ID で記録を取得する |

`Allocate` では `idempotency_key` が必須である。`fleet_name` と `fleet_selector` は一方だけを指定する必要がある。同じ `idempotency_key` の再送は同じ Allocation に収束する。

単一 Fleet から割り当てる Connect JSON request を、以下に示す。

```http
POST /arena.v1.AllocationService/Allocate HTTP/1.1
Content-Type: application/json

{
  "idempotencyKey": "match-123-attempt-1",
  "namespace": "default",
  "fleetName": "shooter-jp",
  "metadata": {
    "sessionId": "match-123"
  },
  "gameServerMetadata": {
    "annotations": {
      "session": "match-123"
    }
  }
}
```

候補選択の指定を、以下にまとめる。

| Field | 性質 |
| --- | --- |
| `selectors[]` | 先頭から順に試す fallback chain |
| `match_labels` | 全 entry の完全一致 |
| `match_fields` | `id` または `spec_hash` の完全一致 |
| `required[]` | Equals、NotEquals、In、NotIn、Exists、NotExists による必須条件 |
| `preferred[]` | 一致数が多い候補を優先する条件 |
| `fleet_selector` | Fleet label で最大 8 Fleet を選ぶ条件 |
| `counter_filters[]` | Counter の available capacity による必須条件 |
| `priorities[]` | Counter の available capacity による並び順 |
| `allow_allocated` | 条件に合う Allocated GameServer への追加 Allocation |

`allow_allocated` には 1 個以上の `counter_filters` が必要である。追加 Allocation を作成しても GameServer は Allocated のままである。

## EventService

`ListEvents` は Fleet または GameServer の event を新しい順に返す。`resource_type` は `fleet` または `gameserver`、`resource_id` は対象の内部 ID である。Limit は既定 50、上限 200 である。

Event は DynamoDB TTL により 7 日後に削除される。Event の書き込みは best effort であり、状態判定には使用しない。

## 認証

`arena-api` に `-authz-file` を指定すると、control-plane RPC に IAM 認証を適用する。未指定の場合は認証を無効にする。

> [!WARNING]
> `-authz-file` を指定しない構成を、信頼できない network へ公開してはいけない。

Token は、`sts:GetCallerIdentity` の SigV4 presigned URL を URL-safe Base64 で符号化し、`arena-v1.` prefix を付けた bearer token である。Presign 時には `x-arena-server` header を署名対象へ含める。Token の最大有効期間は 15 分である。

Authorization header の形式を、以下に示す。

```http
Authorization: Bearer arena-v1.<base64url-presigned-url>
```

`-server-id` は `x-arena-server` の値であり、`-authz-file` と同時に指定する必要がある。`arenactl -auth iam` は server URL の host へ token を自動的に束縛する。

SDK Gateway はこの interceptor の対象外である。`-cluster` が設定されている場合は、ECS Task ARN と `startedBy` による sidecar identity 検証を使用する。

## 認可

認可 file は IAM principal と Arena role の対応を YAML で定義する。Assumed-role session ARN は対応する IAM role ARN へ正規化する。Namespace は完全一致または末尾 `*` の prefix 一致で制限できる。

認可 file の例を、以下に示す。

```yaml
bindings:
  - principal: arn:aws:iam::123456789012:role/arena-fleet-editor
    role: fleet-editor
    namespaces:
      - shooter-*
  - principal: arn:aws:iam::123456789012:role/arena-matchmaker
    role: allocator
    namespaces:
      - default
```

Role と許可範囲を、以下にまとめる。

| Role | 許可される RPC |
| --- | --- |
| `viewer` | Fleet と GameServer の参照、Allocation の参照 |
| `allocator` | `viewer` の範囲、Allocation の作成と解放 |
| `fleet-editor` | `viewer` の範囲、Fleet の作成、更新、削除、scale、apply |
| `admin` | SDK Gateway を除くすべての control-plane RPC |

現行の権限定義では、`ListEvents` と `DeleteGameServer` は `admin` のみが呼び出せる。認可 file は 1 分ごとに再読込する。再読込に失敗した場合は直前の有効な設定を維持する。

## エラー

代表的な Connect と gRPC status code を、以下にまとめる。

| Code | 条件 | 対応 |
| --- | --- | --- |
| `INVALID_ARGUMENT` | 必須 field の不足、selector や spec の不正 | Request を修正する |
| `NOT_FOUND` | 対象 resource が存在しない | ID、namespace、name を確認する |
| `ALREADY_EXISTS` | Fleet の重複作成 | 取得または apply を使用する |
| `RESOURCE_EXHAUSTED` | 条件に合う割り当て候補がない | 同じ idempotency key で backoff 後に再送する |
| `ABORTED` | Version 競合または Allocation claim 競合 | 再取得後に再送する |
| `FAILED_PRECONDITION` | Autoscaling 中の scale、active GameServer がある Fleet の削除 | 前提状態を解消する |
| `UNAUTHENTICATED` | Token がない、無効、期限切れ | Token を再生成する |
| `PERMISSION_DENIED` | Role または namespace の許可がない | 認可 binding を確認する |
