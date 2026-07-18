# API リファレンス

**proto が唯一の API 定義**です(`api/proto/arena/v1/`)。connect-go により同一ハンドラが
gRPC / gRPC-Web / JSON(Connect protocol)を 1 ポートで話します。OpenAPI ドキュメントは
`buf generate` の生成物(`gen/openapi/arena.v1.yaml`)であり、手書き管理しません。

- エンドポイント: `POST /arena.v1.<Service>/<Method>`(JSON は `Content-Type: application/json`)
- ページングは `page_size` / `page_token` 方式
- namespace 省略時は `default`

## FleetService

| RPC | 説明 |
|-----|------|
| `CreateFleet` | Fleet 作成。(namespace, name) 重複は `ALREADY_EXISTS` |
| `GetFleet` / `ListFleets` | 参照(namespace 単位) |
| `UpdateFleet` | spec 全置換。`version` 必須(楽観ロック、競合は `ABORTED`) |
| `DeleteFleet` | 削除。稼働中 GameServer が居る間は `FAILED_PRECONDITION`(先に 0 台へスケール) |
| `ScaleFleet` | replicas 直接指定。autoscaling 有効時は `FAILED_PRECONDITION` |
| `ApplyFleet` | 宣言的 upsert(arenactl の実体)。下記参照 |

### ApplyFleet

(namespace, name) で同定し、無ければ作成、あれば spec を全置換する。
version はサーバー側で read-modify-write + 内部リトライするためクライアントは扱わない。

- `dry_run: true` — 書き込まず、実行した場合の action(CREATED / UPDATED / UNCHANGED)、
  **正規化済み spec**、現状との構造化 diff を返す。クライアント側で正規化を再実装しない
  (デフォルト補完やフィールド順序の揺れで偽差分が出るのを防ぐ)
- **replicas の所有権**: `autoscaling.enabled: true` の spec が replicas を明示すると
  `FAILED_PRECONDITION`。省略時はサーバーの現在値を維持(autoscaler の決定を巻き戻さない)
- template 変更時は `generation` が +1 され、`spec_hash` が更新される

## AllocationService

| RPC | 説明 |
|-----|------|
| `Allocate` | Ready サーバーを 1 台割り当てる。`idempotency_key` **必須** |
| `Release` | 割り当てを解放。サーバーは Ready へ戻り再利用される |
| `GetAllocation` | 割り当ての参照 |

### Allocate の例

```json
POST /arena.v1.AllocationService/Allocate
{
  "idempotencyKey": "match-12345-attempt-1",
  "fleetName": "shooter-jp",
  "namespace": "default",
  "selectors": { "matchLabels": { "version": "v1.2.3" } },
  "metadata": { "sessionId": "match-12345" }
}

→ 200
{
  "allocationId": "…",
  "gameServer": {
    "id": "…",
    "state": "STATE_ALLOCATED",
    "address": "203.0.113.24",
    "ports": [{ "name": "game", "port": 7777, "protocol": "PROTOCOL_UDP" }]
  }
}
```

- **同一 `idempotencyKey` の再送は同一 Allocation に収束する**(タイムアウト後の再送が
  二重割り当てにならない)。クライアントはエラー時に同じキーで安全にリトライできる
- `selectors` なしが高速パス。あり(ラベル一致)はスローパス
- クライアントはレスポンスの `address:port` に**直接**接続する(LB を経由しない)

## GameServerService

| RPC | 説明 |
|-----|------|
| `GetGameServer` | ID で参照 |
| `ListGameServers` | fleet 単位で列挙(state フィルタ可、ページング) |

## エラーモデル

gRPC ステータスコードに統一(Connect が JSON へも同一セマンティクスで写像):

| 状況 | コード | クライアントの対応 |
|------|-------|------------------|
| Ready 在庫なし | `RESOURCE_EXHAUSTED` | バックオフ再送(冪等キーで安全) |
| claim 競合の連続 | `ABORTED` | 再送 |
| バージョン競合(UpdateFleet) | `ABORTED` | 再取得して再送 |
| autoscaling 有効時の Scale / replicas 指定 Apply | `FAILED_PRECONDITION` | min/max の変更で意図を表現 |
| 稼働中 Fleet の削除 | `FAILED_PRECONDITION` | 先に scale 0 |
| 認証なし・無効トークン | `UNAUTHENTICATED` | トークン再取得 |
| 権限なし | `PERMISSION_DENIED` | — |

## 認証・認可

呼び出し元は **AWS IAM を唯一のアイデンティティ基盤**とします。独自のユーザー DB・
パスワード・長命 API キーはありません。`-authz-file`(と `-server-id`)を与えない場合、
認証は無効です(ローカル開発専用)。

### 認証: SigV4 presigned STS トークン

aws-iam-authenticator / Vault AWS auth と同じ方式です:

```
1. クライアントは自分の AWS クレデンシャルで sts:GetCallerIdentity を presign し、
   URL を Bearer トークンとして付与する
     Authorization: Bearer arena-v1.<base64(presigned URL)>
   presign 時に x-arena-server ヘッダ(= API のホスト名)を署名に含める
2. arena-api はトークンを検証(STS エンドポイント・Action・有効期限 ≦15 分・
   x-arena-server の署名束縛)後、URL を実行して IAM プリンシパル ARN を得る
3. 検証結果はトークン失効までメモリキャッシュ(ホットパスで毎回 STS を呼ばない)
```

- 秘密の配布・ローテーションが不要。失効・剥奪は IAM 側の仕組みがそのまま効く
- トークンはホスト名に署名で束縛されるため他サービスへ流用できない

| 呼び出し元 | クレデンシャル |
|-----------|--------------|
| arenactl(人間) | AWS SSO / プロファイル(標準チェーン)。`-auth iam` で自動生成 |
| CI(GitOps) | GitHub Actions OIDC → apply 用ロールを Assume |
| Matchmaker | ECS タスクロール(トークンは ~10 分キャッシュして再利用) |

### 認可: ロールマッピング(RBAC-lite)

IAM ARN を arena 内ロールへマッピングし、RPC × namespace で認可します。
assumed-role のセッション ARN は IAM ロール ARN に正規化して照合されます。

```yaml
# 例: authz bindings(SSM /arena/{env}/authz またはファイル。1 分間隔でリロード)
bindings:
  - principal: "arn:aws:iam::123456789012:role/arena-admin"
    role: admin
  - principal: "arn:aws:iam::123456789012:role/arena-ci-apply"
    role: fleet-editor
    namespaces: ["default", "shooter-*"]     # 前方一致ワイルドカード可
  - principal: "arn:aws:iam::123456789012:role/matchmaker-prod"
    role: allocator
    namespaces: ["shooter-*"]
```

| ロール | 許可される RPC |
|-------|---------------|
| `admin` | すべて |
| `fleet-editor` | Fleet CRUD / Apply / Scale + 参照系 |
| `allocator` | Allocate / Release + 参照系 |
| `viewer` | 参照系のみ |

変更系 RPC は認証済みプリンシパル ARN 付きで監査ログ(構造化ログ)に記録されます。

### Sidecar の認証(別系統)

SDK Gateway のストリームは IAM トークンではなく **Task ARN 突合**で認証します:
sidecar が ECS Task メタデータから得た Task ARN を提示し、gateway が
`ecs:DescribeTasks` の `startedBy == "arena:{gameserver_id}"` と照合して
gameserver_id のなりすましを防ぎます。GameServer Task に IAM の API 権限は一切不要です。

## SDK Gateway(内部プロトコル)

`arena/gateway/v1/sdk_gateway.proto` は sidecar ⇄ arena-api の**内部**プロトコルであり、
ゲーム開発者向けの公開 SDK(`arena/v1/sdk.proto`)とは分離されています。
内部プロトコルの変更はゲームサーバーイメージに波及しません。
ゲームサーバーからの利用方法は [sdk.md](sdk.md) を参照してください。
