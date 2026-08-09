# p2pquake-notify

[P2P地震情報](https://www.p2pquake.net/) のJSON API v2 (WebSocket) に常時接続し、緊急地震速報・津波予報・地震情報を3系統のDiscord webhookへ振り分ける常駐サービスです。AWSリソースは [tamura09/aws-terraform](https://github.com/tamura09/aws-terraform) が所有し、このリポジトリはソースとイメージのビルド/デプロイだけを持ちます。

## なぜECS Fargateなのか

他の通知系 (`cost-notify`, `iassistant-notify`) はEventBridgeで起動するLambdaですが、この口だけ形が違います。上流がpush型のWebSocketなので接続を張りっぱなしにする必要があり、実行時間に上限のあるLambdaでは成立しないためです。RESTのポーリングに落とすこともできますが、1分間隔のポーリングでは緊急地震速報の意味がなくなります。

リージョンは `ap-northeast-1`。上流が国内にあるので、`us-east-1` に置くと往復で150msほど損をします。これはアプリ側のどんな最適化よりも効きます。

## 通知の振り分け

| ルート | SSMパラメータ | 対象 |
| --- | --- | --- |
| `dev` | `/p2pquake-notify/discord/dev-webhook-url` | ピア分布 (code 555) を除く全メッセージ。訓練報も揺れた報告も未対応のcodeも流します。 |
| `alert` | `/p2pquake-notify/discord/alert-webhook-url` | 緊急地震速報 (556) と津波予報 (552)。全国。 |
| `local` | `/p2pquake-notify/discord/local-webhook-url` | `P2PQUAKE_LOCAL_PREFECTURES` (既定は岩手県) に関わる地震情報 (551)・津波予報 (552)・緊急地震速報 (556)。 |

パラメータ名を設定しなかったルートは無効になります。3つとも未設定なら起動時に落とします — 通知先ゼロで動き続けるのは、動いているように見えて何も届かない一番たちの悪い壊れ方だからです。

`local` に code 554 (揺れ検知) を入れていないのは、554が地域情報を持たず、地域フィルタと併用すると決して一致しないためです。

**訓練報・テスト配信 (`test: true`) は `dev` 以外には流しません。** 訓練のたびに本番の通知先へ誤報が飛ぶと、いざという時に誰も信じなくなります。

**ピア分布 (code 555) はどのルートへも流しません。** 人間が読んで意味のある情報を含まないうえ、地震が無くても一定間隔で届き続けるので、`dev` に流すだけでも他の通知を押し流します。このcodeの用途は「上流と繋がっている」ことの証明だけで、それは `p2pquake_last_message_age_seconds` として Grafana に出ています。

## 環境変数

| 変数 | 既定値 | 説明 |
| --- | --- | --- |
| `P2PQUAKE_DEV_WEBHOOK_PARAMETER_NAME` | (なし) | dev用webhook URLを保存したSSM SecureString名 |
| `P2PQUAKE_ALERT_WEBHOOK_PARAMETER_NAME` | (なし) | 速報用webhook URLのSSM SecureString名 |
| `P2PQUAKE_LOCAL_WEBHOOK_PARAMETER_NAME` | (なし) | 地域用webhook URLのSSM SecureString名 |
| `P2PQUAKE_LOCAL_PREFECTURES` | `岩手県` | 地域ルートの対象。カンマ区切り。部分一致で判定します |
| `P2PQUAKE_LOCAL_MIN_SCALE` | `-1` (しきい値なし) | 地域ルートの最小震度。`30`=震度3、`45`=震度5弱 |
| `P2PQUAKE_WS_URL` | `wss://api.p2pquake.net/v2/ws` | 接続先。サンドボックスへの切り替えに使います |
| `P2PQUAKE_HISTORY_URL` | `https://api.p2pquake.net/v2/history` | 再接続時のギャップ補完に読むREST |
| `P2PQUAKE_BACKFILL_LIMIT` | `20` | 補完で読み直す件数（**code 1つあたり**）。`0`で補完しない |
| `P2PQUAKE_STALE_AFTER` | `10m` | この時間まったく受信が無ければ接続が死んだとみなす |
| `GRAFANA_REMOTE_WRITE_URL_PARAMETER_NAME` | (なし) | 未設定ならハートビート送信を無効にします |
| `GRAFANA_PROMETHEUS_USERNAME_PARAMETER_NAME` | (なし) | Grafana CloudのPrometheus username (instance ID) |
| `GRAFANA_PUSH_TOKEN_PARAMETER_NAME` | (なし) | Grafana Cloudのpush token |
| `METRICS_INTERVAL` | `1m` | ハートビートの送信間隔 |

webhook URLそのものではなくSSMのパラメータ名を渡します。ECSのタスク定義はコンソールからもAPIからも平文で読めるので、値を置く場所ではありません。

## 死活監視

**地震はめったに起きないので、「通知が来ない」だけでは正常なのか死んでいるのか区別がつきません。** このサービスに関しては死活監視が機能そのものと同じくらい重要です。

Grafana Cloudへ毎分押し込む `p2pquake_last_message_age_seconds` を見てください。上流は地震が無くてもピア分布 (code 555) を定期的に流すので、この値が伸び続けているなら通知経路のどこかが死んでいます。No Data も同じ意味で扱ってください。

| メトリクス | 意味 |
| --- | --- |
| `p2pquake_last_message_age_seconds` | 最終受信からの経過秒。**これがアラートの主役** |
| `p2pquake_ws_connected` | WebSocketが繋がっているか (0/1) |
| `p2pquake_ws_reconnects_total` | 再接続の累計。増え続けるなら接続が不安定 |
| `p2pquake_notification_failures_total` | Discordへの送信に最終的に失敗した件数 |
| `p2pquake_messages_received_total` | 受信メッセージの累計 |
| `p2pquake_duplicates_dropped_total` | 重複排除で落とした件数 |
| `p2pquake_decode_failures_total` | デコードできなかった件数。増えたら上流の形式変更を疑う |

## ローカルでの動作確認

サンドボックスがダミーの地震を流し続けてくれるので、本物の地震を待たずに通知の見た目とルーティングを確認できます。

```bash
P2PQUAKE_WS_URL=wss://api-realtime-sandbox.p2pquake.net/v2/ws P2PQUAKE_DEV_WEBHOOK_PARAMETER_NAME=/p2pquake-notify/discord/dev-webhook-url AWS_REGION=ap-northeast-1 go run .
```

SSMを読むのでAWSの認証情報が要ります。テストだけならネットワークもAWSも不要です。

```bash
go test ./...
```

## CI/CD

[.github/workflows/build.yml](.github/workflows/build.yml) がPRと `main` へのpushで `go vet` / `go test` を実行します。`main` へのpushではさらにarm64イメージをECR (`p2pquake-notify`) へpushし、`aws ecs update-service --force-new-deployment` でサービスを入れ替えます。認証はGitHub OIDCで、`aws-terraform` が定義する `github-actions-p2pquake-notify` ロールを引き受けます。

タスク定義は `aws-terraform` が `:latest` を指したまま所有し、ワークフローは書き換えません。タスク定義にcommit SHAを埋めると、Terraformとデプロイワークフローが同じ属性を取り合って毎回planに差分が出ます。

デプロイ中は新旧のタスクを重ねません (`deployment_maximum_percent = 100`)。重ねるとWebSocket接続が一時的に2本になり、同じ地震が2回通知されます。引き換えに入れ替えの数十秒だけ受信が途切れますが、その間のイベントは再接続時の履歴補完が拾います。

## 上流について

- P2P地震情報は無保証の無償サービスです。可用性の保証はありません。
- `code: 556` は気象庁の緊急地震速報 (**警報**) のみで、震度4以下相当の「予報」は流れてきません。予報レベルまで必要なら [DM-D.S.S (dmdata.jp)](https://dmdata.jp/) のような有償の配信を併用することになります。
- `code: 554` はP2P地震情報のクライアントが揺れを検知したという意味で、気象庁の発表ではありません。誤検知があります。通知本文にも毎回その旨を書いています。
- **`/v2/history` は1リクエストにつき1つの `codes` しか受け付けません。** `codes=551,552,556` は400で、`codes=551&codes=552` は最初の1つだけが効きます。そのため履歴補完は code ごとにリクエストを分けています。
- **`codes` を指定せずに `/v2/history` を叩いてはいけません。** ピア分布 (code 555) が絶えず記録されているので `limit` の枠が丸ごとそれで埋まり、地震も津波も1件も返ってきません。補完は成功したように見えて常に空振りします。
- 型定義は [types.go](types.go) にありますが、必須として扱っているのは `code` だけです。上流がフィールドを増減しても落ちないよう、他はすべて「無ければゼロ値」で成立するようにしています。未対応のcodeは生JSONのまま `dev` ルートへ流れるので、そこで形式変更に気付けます。
