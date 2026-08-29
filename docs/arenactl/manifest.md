# Fleet manifest

本ドキュメントは、`arenactl apply`、`diff`、`get` が扱う YAML manifest の書式を説明する。Manifest は ECS の Task Definition と Service に近い field 名を使用し、Arena API の `FleetSpec` へ変換される。

## 最小構成

1 個の UDP port を公開する Fleet の例を、以下に示す。

```yaml
name: shooter-jp
namespace: default
desiredCount: 3
tags:
  game: shooter

taskDefinition:
  cpu: "1024"
  memory: "2048"
  tags:
    version: v1
  containerDefinitions:
    - name: gameserver
      image: 123456789012.dkr.ecr.ap-northeast-1.amazonaws.com/shooter:v1
      portMappings:
        - name: game
          containerPort: 7777
          protocol: udp
```

`namespace` を省略すると `default` になる。`name` は 63 文字以下で、`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$` に一致する必要がある。

## 読み込み規則

Manifest loader の規則は次のとおりである。

- 未知の field は error とする
- File 内の `---` 区切りを複数 document として読む
- Directory は再帰的に探索し、`.yaml` と `.yml` だけを読む
- `${VAR}` を環境変数で展開する
- 未定義の `${VAR}` は error とする
- `$VAR` は展開しない
- 読み込んだ Fleet に `arena.dev/managed-by=arenactl` label を付ける

環境変数を用いる manifest の一部を、以下に示す。

```yaml
taskDefinition:
  containerDefinitions:
    - name: gameserver
      image: ${GAME_SERVER_IMAGE}
```

適用 command の例を、以下に示す。

```console
$ GAME_SERVER_IMAGE=example.com/game:v2 arenactl apply -f fleet.yaml
```

## Fleet field

Top-level field を、以下にまとめる。

| Field | Type | 性質 |
| --- | --- | --- |
| `name` | string | Fleet 名。必須 |
| `namespace` | string | Namespace。省略時は `default` |
| `tags` | map | Fleet label |
| `desiredCount` | int | 希望台数。0 から 10000 |
| `scheduling` | string | `packed` または `distributed` |
| `taskDefinition` | object | GameServer Task の template。必須 |
| `autoScaling` | object | Autoscaling 設定 |
| `strategy` | object | Template 更新方法 |
| `allocationOverflow` | object | 縮小または更新中の Allocated GameServer へ付与する metadata |
| `capacity` | object | ECS capacity provider strategy |
| `network` | object | Fleet 単位の network override |
| `drainGraceSeconds` | int | Game container の ECS stop timeout。0 から 120 |

Autoscaling が有効な場合は `desiredCount` を省略する必要がある。新規 Fleet は `minCapacity` から開始する。

`scheduling` は API と DynamoDB に保存されるが、現行の ECS launcher は placement strategy に反映しない。

## Task Definition

`taskDefinition` field を、以下にまとめる。

| Field | Type | 性質 |
| --- | --- | --- |
| `cpu` | string | ECS Task の CPU。省略時 `1024` |
| `memory` | string | ECS Task の memory MiB。省略時 `2048` |
| `tags` | map | 作成する GameServer の label |
| `annotations` | map | 作成する GameServer の annotation |
| `containerDefinitions` | list | Game container と補助 container。1 個以上 |
| `gameContainer` | string | 複数 container 時の game container 名 |
| `volumes` | list | EFS または host volume |

単一 container の場合はその container を game container とする。複数 container の場合は `gameContainer` が `containerDefinitions[].name` のいずれかと一致する必要がある。Controller は `arena-sidecar` container を追加するため、manifest に sidecar を記載しないこと。

Task の CPU と memory は game container の `resources` として API に保存される。Sidecar には CPU 128 unit、memory 256 MiB を固定で割り当て、残りを game container へ割り当てる。

## Container

`containerDefinitions[]` の field を、以下にまとめる。

| Field | Type | 性質 |
| --- | --- | --- |
| `name` | string | Container 名。複数 container では必須 |
| `image` | string | Container image。必須 |
| `environment` | list | `name` と `value` の環境変数 |
| `portMappings` | list | Game container の port。補助 container には指定不可 |
| `healthCheck` | object | Arena SDK heartbeat の期待値。Game container のみ |
| `command` | list | ECS `entryPoint` |
| `args` | list | ECS `command` |
| `workingDirectory` | string | Working directory |
| `secrets` | list | `name` と Secrets Manager または SSM の `valueFrom` ARN |
| `containerHealthCheck` | object | ECS container health check |
| `mountPoints` | list | Volume mount |
| `resources` | object | 補助 container の CPU と memory |

Game container へ `resources` を指定する場合は、`taskDefinition.cpu` と `taskDefinition.memory` を同時に指定できない。補助 container の `resources.cpu` と `resources.memory` は ECS container reservation へ変換する。

Container command と secret を含む例を、以下に示す。

```yaml
taskDefinition:
  cpu: "2048"
  memory: "4096"
  containerDefinitions:
    - name: gameserver
      image: example.com/game:v2
      command: ["/app/server"]
      args: ["--port", "7777"]
      workingDirectory: /app
      environment:
        - name: LOG_LEVEL
          value: info
      secrets:
        - name: MATCH_SECRET
          valueFrom: arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:match
      containerHealthCheck:
        command: ["CMD-SHELL", "test -f /tmp/started"]
        intervalSeconds: 10
        timeoutSeconds: 5
        retries: 3
        startPeriodSeconds: 30
```

## Port

`portMappings[]` の field を、以下にまとめる。

| Field | Type | 性質 |
| --- | --- | --- |
| `name` | string | Port 名。必須 |
| `containerPort` | int | 1 から 65535 |
| `protocol` | string | `udp` または `tcp`。省略時 `udp` |

Controller は Game container へ `ARENA_PORT_<NAME>` を追加する。Port 名は大文字へ変換し、`-` を `_` に置換する。`game-udp` は `ARENA_PORT_GAME_UDP` となる。

API の `PortSpec` は Passthrough と TCPUDP を持つが、現行 manifest schema はこれらを表現しない。必要な場合は FleetService API を直接使用する。

## Heartbeat

`healthCheck` の field と API field の対応を、以下にまとめる。

| Field | API field |
| --- | --- |
| `startPeriod` | `initial_delay_seconds` |
| `interval` | `period_seconds` |
| `retries` | `failure_threshold` |

現行 controller はこれら 3 field を失効判定に使用しない。失効判定は sidecar の固定 10 秒 heartbeat、最初の Health 後の固定 30 秒 timeout、controller の固定 30 秒 sweep と固定 60 秒猶予で動作する。

API は health sweep の無効化 field を持つが、現行 manifest schema はこの field を表現しない。Heartbeat の動作は、[../arena/sdk.md](../arena/sdk.md) を参照。

## Volume

EFS volume の例を、以下に示す。

```yaml
taskDefinition:
  volumes:
    - name: game-data
      efs:
        fileSystemId: fs-0123456789abcdef0
        rootDirectory: /server
  containerDefinitions:
    - name: gameserver
      image: example.com/game:v2
      mountPoints:
        - volume: game-data
          containerPath: /data
          readOnly: true
```

各 volume は `efs` または `host` の一方だけを持つ必要がある。Mount point の `volume` は宣言済みの名前を参照する必要がある。現行 launcher が登録する Task Definition は Fargate compatibility を要求するため、host volume は AWS 側で利用できない構成がある。

## 更新 strategy

RollingUpdate の例を、以下に示す。

```yaml
strategy:
  type: rollingUpdate
  rollingUpdate:
    maxSurge: 25%
    maxUnavailable: 0
    drainTimeoutSeconds: 3600
```

`type` は `rollingUpdate` または `recreate` である。RollingUpdate の `maxSurge` と `maxUnavailable` は非負の整数または percentage を string で指定する。`maxSurge` は切り上げ、`maxUnavailable` は切り捨てで希望台数へ適用する。両方が 0 になる設定は拒否する。

省略時は RollingUpdate として動作し、内部計算では 25% 相当の budget を使用する。`drainTimeoutSeconds` が 0 の場合、旧世代の Allocated GameServer を強制 drain しない。

## Allocation overflow

縮小または更新で希望台数を超えて残る Allocated GameServer へ付与する metadata の例を、以下に示す。

```yaml
allocationOverflow:
  labels:
    arena.dev/overflow: "true"
  annotations:
    arena.dev/reason: rollout
```

ゲーム側は `WatchGameServer` で metadata の更新を受信できる。

## Capacity と network

Fargate と Fargate Spot の比率、および Fleet 固有の network を指定する例を、以下に示す。

```yaml
capacity:
  providers:
    - name: FARGATE
      base: 1
      weight: 1
    - name: FARGATE_SPOT
      weight: 3

network:
  assignPublicIp: true
  subnets:
    - subnet-0123456789abcdef0
  securityGroups:
    - sg-0123456789abcdef0
```

`network` の list が空の場合は controller の flag を使用する。`assignPublicIp` を省略した場合も controller の値を使用する。

Capacity provider を省略すると launcher は Fargate launch type を使用する。現行 Task Definition は Fargate compatibility 固定であるため、任意の EC2 capacity provider はそのままでは使用できない。

## Autoscaling

Autoscaling の共通 field を、以下にまとめる。

| Field | Type | 性質 |
| --- | --- | --- |
| `enabled` | bool | Autoscaler の有効化 |
| `minCapacity` | int | 下限。0 以上 |
| `maxCapacity` | int | 上限。1 以上かつ下限以上 |
| `policy` | object | 希望台数の計算方法 |

### Buffer policy

Allocated 数に Ready buffer を加える例を、以下に示す。

```yaml
autoScaling:
  enabled: true
  minCapacity: 2
  maxCapacity: 100
  policy:
    type: buffer
    buffer:
      bufferSize: 5
```

`bufferSize` と `bufferPercent` のいずれかを使用する。Percentage は Allocated 数に掛けて切り上げる。Percentage による buffer は最低 1 である。

### Schedule policy

時刻別の希望台数を指定する例を、以下に示す。

```yaml
autoScaling:
  enabled: true
  minCapacity: 2
  maxCapacity: 100
  policy:
    type: schedule
    schedule:
      - cron: "0 8 * * *"
        desiredCount: 30
      - cron: "0 23 * * *"
        desiredCount: 5
```

Cron は minute、hour、day of month、month、day of week の 5 field である。`*`、整数、範囲、step、comma list を使用できる。過去 24 時間 1 分以内で最後に一致した entry を採用する。Timezone は controller process の local timezone である。

### Webhook policy

External endpoint から希望台数を取得する例を、以下に示す。

```yaml
autoScaling:
  enabled: true
  minCapacity: 2
  maxCapacity: 100
  policy:
    type: webhook
    webhook:
      url: https://autoscaler.example.com/desired
      headers:
        Authorization: Bearer example
```

Controller は Fleet identity、現在の replicas、state 別の数、Counter 集計を JSON で POST し、`{"replicas": 20}` を受け取る。Timeout は 3 秒である。3 回失敗すると circuit を 30 秒開く。失敗または `replicas: null` の場合は現在値を維持する。

### Counter policy

Fleet 全体の Counter capacity に buffer を持たせる例を、以下に示す。

```yaml
autoScaling:
  enabled: true
  minCapacity: 2
  maxCapacity: 100
  policy:
    type: counter
    counter:
      key: rooms
      bufferSize: 10
```

`bufferSize` と `bufferPercent` のいずれかを指定する。Counter snapshot がない場合は希望台数を変更しない。

### Chain policy

時間帯に応じて policy を切り替える例を、以下に示す。

```yaml
autoScaling:
  enabled: true
  minCapacity: 2
  maxCapacity: 100
  policy:
    type: chain
    chain:
      - schedule:
          cron: "0 18 * * *"
          durationSeconds: 21600
        policy:
          type: webhook
          webhook:
            url: https://autoscaler.example.com/event
      - policy:
          type: buffer
          buffer:
            bufferSize: 5
```

現在時刻が schedule window に入る最初の entry を使用する。Schedule がない entry は常に一致するため、fallback として最後に置く。`durationSeconds` が 0 以下の場合は 1 時間である。Chain の中に Chain は指定できない。

## Export

`arenactl get fleet` の出力は同じ manifest schema である。Server が所有する status、ID、version、generation を除外する。Autoscaling が有効な場合は `desiredCount` を除外する。管理 label は `tags` に含まれる。
