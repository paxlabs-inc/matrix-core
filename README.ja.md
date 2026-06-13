<!--
parent:
  order: false
-->
<p align="center">
  <img src="https://zezsqawedbikldiedlse.supabase.co/storage/v1/object/public/cdn.deus.paxeer.app/bcd6e442-788a-4377-b605-42ce2896d32e.png" alt="Paxeer Network" width="1200">
</p>
<p align="center">
<p align="center">
  <img src="https://img.shields.io/badge/Project-Matrix-FFFFFF?style=for-the-badge&labelColor=004CED" alt="Project: Matrix" />
  <img src="https://img.shields.io/badge/Built_by-PaxLabs-004CED?style=for-the-badge&labelColor=000000" alt="Built by PaxLabs" />
  <img src="https://img.shields.io/badge/License-Matrix--Protocol-004CED?style=for-the-badge&labelColor=000000" alt="License: Matrix-Protocol" />
  <img src="https://img.shields.io/badge/Status-Active-00C896?style=for-the-badge&labelColor=000000" alt="Status: Active" />
</p>

<p align="center">
  <img src="https://img.shields.io/badge/Chain-HyperPaxeer-004CED?style=for-the-badge&labelColor=000000" alt="Chain: HyperPaxeer" />
  <img src="https://img.shields.io/badge/Chain_ID-125-FFFFFF?style=for-the-badge&labelColor=004CED" alt="Chain ID: 125" />
  <img src="https://img.shields.io/badge/Block_Time-400ms-00C896?style=for-the-badge&labelColor=000000" alt="Block Time: 400ms" />
  <img src="https://img.shields.io/badge/Finality-400ms-00C896?style=for-the-badge&labelColor=000000" alt="Finality: 400ms" />
</p>

<p align="center">
  <a href="https://github.com/paxlabs-inc/matrix-core/actions/workflows/ci.yml"><img src="https://github.com/paxlabs-inc/matrix-core/actions/workflows/ci.yml/badge.svg?branch=main" alt="ci" /></a>
  <a href="https://github.com/paxlabs-inc/matrix-core/actions/workflows/lint.yml"><img src="https://github.com/paxlabs-inc/matrix-core/actions/workflows/lint.yml/badge.svg?branch=main" alt="lint" /></a>
  <a href="https://github.com/paxlabs-inc/matrix-core/actions/workflows/docker.yml"><img src="https://github.com/paxlabs-inc/matrix-core/actions/workflows/docker.yml/badge.svg?branch=main" alt="docker" /></a>
  <img src="https://img.shields.io/badge/Go-1.22-004CED?logo=go&logoColor=white" alt="Go 1.22" />
  <img src="https://img.shields.io/badge/Modules-9-004CED" alt="9 Go modules" />
</p>

---

## Matrix とは？

Matrix は [Paxeer Network](https://paxeer.app) の上に位置する認知層・UX 層です。
非開発者からの自然言語リクエストを、型付きで検査可能・修正可能な
**Intent IR（意図中間表現）** に変換し、エージェントが実際に実行できるようにします。
その際、今日「非開発者 ↔ エージェント」のワークフローを破綻させる
4 つの典型的な失敗モードを回避します。

1. **プロンプトの脆さ** — わずかな言い換えで出力が大きく変わってしまう。
2. **意図の消失** — 自然言語が複数ステップの実行を通じて保持されない。
3. **共有オントロジーの欠如** — どの実体を指しているかでユーザーとエージェントが食い違う。
4. **構造化された訂正手段の不在** — ドリフトが起きるとユーザーは一から書き直すしかない。

Matrix は、共有されたメモリ + 実行基盤の上に **2 本のエージェントレール** を提供します。

- **Neo** — *デフォルト*の会話型ツール呼び出しエージェント。馴染みやすく堅牢で、
  可逆な作業（shell・コード・fetch・web）には完全に許容的です。金銭が絡む、
  あるいは不可逆な作業は厳格レールに委譲します。
- **MCL パイプライン** — *厳格*レール。自然言語 → 型付き Intent IR →
  プラン → 再生可能なウォークへと進み、高リスク / オンチェーン / 不可逆な作業を扱います。

スタックは次のように階層化されています。

| レイヤー      | 役割                                                                                          |
| ------------ | --------------------------------------------------------------------------------------------- |
| **MCL**      | NL → 型付き Intent IR に変換するプロトコル。動詞はクローズド（10 個）、オブジェクトもクローズド（8 種）。 |
| **cortex**   | Pebble 上のアクター単位の型付きメモリグラフ。追記専用ジャーナル、Merkle アンカー付きスナップショット。 |
| **bridge**   | MCL コンパイラの `Cortex` インターフェースを稼働中の cortex インスタンスに繋ぐグルー。           |
| **executor** | プランウォーカー、ライフサイクルマシン、MCP ツールディスパッチ、ユーザー単位デーモン、Liaison ナレーター、e2e ハーネス。 |
| **neo**      | デフォルトの会話型エージェント — ページング cortex メモリ付きのツール呼び出しループ。           |
| **gateway**  | 計量型 LLM プロキシ + PAX クレジット台帳（無料枠ホワイトリスト + レートカード）。                |
| **router**   | ユーザー単位の Fly Machine プロビジョニング + wake-then-reverse-proxy のフロントドア。           |
| **deus**     | エージェントサービスのマーケットプレイス：登録、検索、計量呼び出し、EIP-712 レシート、ホスティング。 |
| **uwac**     | Universal Web Agent Connector — OAuth ボールト → ユーザー単位の MCP ツール。                    |
| **tachyon**  | エージェントネイティブな Solidity/EVM エンジン — コンパイル / テスト / シミュレート / デプロイ。  |
| **agents**   | DID にバインドされたマニフェスト。人格ではなくプロトコル。                                       |
| **tools**    | エージェントが呼び出す MCP サーバー（チェーンに触れるものを含む）。                              |


---

## リポジトリ構成

```text
matrix/
├── cortex/        アクター単位の型付きメモリグラフ（Pebble）+ replay 不変条件 + Merkle スナップショット
├── MCL/           MatrixScript コンパイラ — lexer/parser/validator/canonical/interpreter + Intent IR + envelopes + LLM クライアント
├── bridge/        MCL ↔ cortex アダプタ（独立 Go モジュール。replace ディレクティブでリンク）
├── executor/      ライフサイクルマシン、ランタイムウォーカー、MCP クライアント + ツールレジストリ、ユーザー単位デーモン（+ Liaison ナレーター）、e2e ハーネス
├── neo/           Neo — デフォルトの会話型ツール呼び出しエージェント（金銭/不可逆な作業は MCL に委譲）
├── gateway/       計量型 LLM プロキシ + PAX クレジット台帳（無料枠ホワイトリスト + レートカード）
├── router/        ユーザー単位の Fly Machine プロビジョニング + wake-then-reverse-proxy フロントドア
├── deus/          エージェントサービスのマーケットプレイス：登録、検索、計量呼び出し、EIP-712 レシート、ホスト実行
├── uwac/          Universal Web Agent Connector — OAuth ボールト → ユーザー単位の MCP ツール（構築中）
├── tachyon/       エージェントネイティブな Solidity/EVM エンジン — コンパイル/テスト/シミュレート/デプロイ（git サブモジュール）
├── chronos/       集中型エージェントスケジューラ / wake-up システム（設計凍結済み）
├── agents/        DID バインドのエージェントマニフェスト（default.json、neo.json）+ MCP サーバーテンプレート
├── tools/         MCP サーバー — paxeer、browser、tachyon、deus、uwac、web-search、media、cortex
├── skills/        SKILL.mtx 能力マニフェスト + SKILL.md 散文本体
├── client/        Matrix コンシューマーアプリ（Next.js / React）
├── marketplace/   Deus マーケットプレイス + 開発者ダッシュボード（Cloudflare Workers 上の React Router）
├── deploy/        デーモンイメージ、Fly Machine デプロイ、共有サービスイメージ、box インストールスクリプト
├── rules/         アイデンティティ + 言語別コーディング規約
├── knowledge/     正規リファレンス（matrix.kvx プロジェクト状態、モデル）
└── runs/          一時的なハーネス出力（gitignore 済み）
```

### Go モジュール

ルートの `Makefile` は **9 つ** の兄弟 Go モジュール — MCL、bridge、executor、gateway、
router、cortex、tachyon、deus、neo — に加えて **uwac**（および進行中の **chronos**）を駆動します。
各モジュールは独自の `go.mod` を持ち、独立して `go build`/`go test` 可能です。
モジュール間のインポートは開発中は `replace` ディレクティブを、公開時は明示的なバージョンを使います。

```text
cortex   → matrix/cortex                    型付きメモリグラフ、replay 不変条件、スナップショット/Merkle
MCL      → matrix/mcl                       コンパイラ + Intent IR + envelopes + LLM クライアント
bridge   → matrix/bridge                    MCL ↔ cortex アダプタ
executor → matrix/executor                  プランウォーカー、ライフサイクル、MCP ディスパッチ、デーモン、Liaison
neo      → matrix/neo                       デフォルトの会話型エージェント
gateway  → matrix/gateway                   計量型 LLM プロキシ + PAX クレジット台帳
router   → matrix/router                    ユーザー単位の Fly Machine ルーティング
deus     → github.com/paxlabs-inc/deus      エージェントサービスのマーケットプレイス
uwac     → github.com/paxlabs-inc/uwac      外部アプリコネクタ
tachyon  → github.com/paxlabs-inc/tachyon-tools   Solidity/EVM エンジン（サブモジュール）
```

---

## クイックスタート

### 前提条件

- Go **1.22+**（全モジュールでツールチェーンを固定）。
- `make`（GNU make 4.x）。
- MCP サーバー駆動のフロー向け：`node` ≥ 20、`npx`、`python3` ≥ 3.11、`uv`。
- デーモンイメージ向け：buildx 付きの `docker`。

### クローンとビルド

```bash
git clone https://github.com/paxlabs-inc/matrix-core.git
cd matrix-core
make build           # 9 つの Go モジュールをすべてビルド
make install         # 実行可能な CLI を ./bin に配置
make test            # モジュールごとに `go test -count=1 -race ./...`
make vet             # モジュールごとに `go vet ./...`
make ci              # gofmt-check + vet + tests（GitHub Actions と同等）
```

`golangci-lint` をローカルに用意する場合：

```bash
make lint-install    # v1.61.0 に固定
make lint
```

### シークレットの設定

```bash
cp .env.example .env
# dry-run でないコンパイル向けに FIREWORKS_API_KEY / TOGETHER_API_KEY を記入
# 認証付きでデーモンを動かす場合は MATRIX_DAEMON_TOKEN を記入
```

`.env` は gitignore されています。`.env.example` は Matrix が読むすべての変数を記載しています。

### 最初の意図をコンパイルする

```bash
./bin/mclc compile \
  -skill skills/writing-plans/SKILL.mtx \
  -prose "Build a deployment pipeline for my Node.js app" \
  -verb  build
```

`FIREWORKS_API_KEY` を設定すると、コンパイラは実際の Intent Frame（動詞、型付きオブジェクト、
ブロッキングする未知項）を出力します。キーがない場合は dry-run モードにフォールバックし、
完全に補間されたプロンプト構造を表示します。

### エンドツーエンドのウォークを実行する

```bash
./bin/mcl-execute walk \
  -prose       "Summarise the README and write it to /tmp/summary.md" \
  -manifest    agents/default.json \
  -cortex-root ./runs/dev-cortex \
  -skills-root ./skills
```

これはエージェントマニフェストを読み込み、宣言された MCP サーバーを起動し、prose を
Intent + PlanTree にコンパイルし、プランをウォークし、各ステップを cortex の Event として
ジャーナルに記録し、最後に `cortex.Attest` が `KindAttest` + `KindLearnWeights` を
アトミックに書き込んで終了します。

### デーモンを実行する

```bash
./bin/mcl-execute daemon \
  -addr        :8080 \
  -cortex-root ./runs/dev-cortex \
  -manifest    agents/default.json \
  -skills-root ./skills
```

ルート：

| メソッド | パス             | 目的                                     |
| ------- | ---------------- | ---------------------------------------- |
| GET     | `/healthz`       | 死活監視 + SSE ブローカー統計            |
| POST    | `/chat`          | エージェントと会話（Neo フロントドア）   |
| GET     | `/events`        | Server-Sent Events のテール（トランスクリプト） |
| POST    | `/messages`      | prose メッセージを送信（厳格レール）     |
| GET     | `/intents/{id}`  | ID で意図 envelope チェーンを読む        |
| GET     | `/me`            | ユーザー単位の設定 + アイデンティティ    |
| POST    | `/shutdown`      | グレースフルなドレイン                   |

---

## ドキュメント

- [`ARCHITECTURE.md`](./ARCHITECTURE.md) — システムマップ、モジュール境界、主要な不変条件
- [`CONTRIBUTING.md`](./CONTRIBUTING.md) — 開発環境構築、テストの規律、コミットスタイル
- [`SECURITY.md`](./SECURITY.md) — 脆弱性の開示
- [`CHANGELOG.md`](./CHANGELOG.md) — Keep-a-Changelog 形式のリリースノート
- [`deploy/daemon/README.md`](./deploy/daemon/README.md) — イメージビルド + Fly デプロイ
---

## ライセンス

**Matrix-Protocol License** のもとでソース公開されています — [`LICENSE.md`](./LICENSE.md) を参照。

短い説明：読む・使う・デプロイする・統合するのは自由です。**改変して再配布**する場合は、
同じライセンスのもとで変更を公開してください。商業トリガーのしきい値（請求した手数料が
12 か月で 10 万 USD 超 **または** コントロール下の流動性が 1,000 万 USD 超）を超えると、
商用ライセンスが必要になります。`LICENSE.md` の拘束力のない要約は便宜のためのものであり、
ライセンス本文が正式なものです。

---

<p align="center">
  <a href="https://labs.paxeer.app">Paxlabs Inc.</a> により構築 · <code>SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol</code>
</p>