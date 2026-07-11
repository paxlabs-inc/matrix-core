<p align="center">
  <img src="https://cdn.redixusercontent.ocfstudio.com/matrix.png" alt="Matrix" />
</p>

<p align="center">
  <a href="https://labs.paxeer.app"><img src="https://img.shields.io/badge/Project-Matrix-0A0A0A?style=flat-square&logo=data:image/svg+xml;base64,PHN2ZyB4bWxucz0iaHR0cDovL3d3dy53My5vcmcvMjAwMC9zdmciIHdpZHRoPSIxNiIgaGVpZ2h0PSIxNiIgdmlld0JveD0iMCAwIDI0IDI0IiBmaWxsPSJub25lIiBzdHJva2U9IiNmZmZmZmYiIHN0cm9rZS13aWR0aD0iMiIgc3Ryb2tlLWxpbmVjYXA9InJvdW5kIiBzdHJva2UtbGluZWpvaW49InJvdW5kIj48Y2lyY2xlIGN4PSIxMiIgY3k9IjEyIiByPSIxMCIvPjxwYXRoIGQ9Ik0xMiAxNnYtNCIvPjxwYXRoIGQ9Ik0xMiA4aC4wMSIvPjwvc3ZnPg==&logoColor=white" alt="Project: Matrix" /></a>
  <a href="https://labs.paxeer.app"><img src="https://img.shields.io/badge/Built%20by-PaxLabs-0A0A0A?style=flat-square&logoColor=white" alt="Built by PaxLabs" /></a>
  <a href="LICENSE.md"><img src="https://img.shields.io/badge/License-Matrix--Protocol-0A0A0A?style=flat-square" alt="License: Matrix-Protocol" /></a>
  <a href="#"><img src="https://img.shields.io/badge/Status-Active-0A0A0A?style=flat-square" alt="Status: Active" /></a>
  <a href="https://paxeer.app"><img src="https://img.shields.io/badge/Layer-Paxeer%20Network-0A0A0A?style=flat-square" alt="Paxeer Network" /></a>
</p>

<p align="center">
  <a href="https://github.com/paxlabs-inc/matrix-core/stargazers"><img src="https://img.shields.io/github/stars/paxlabs-inc/matrix-core?style=flat-square&color=0A0A0A" alt="GitHub Stars" /></a>
  <a href="https://github.com/paxlabs-inc/matrix-core/network/members"><img src="https://img.shields.io/github/forks/paxlabs-inc/matrix-core?style=flat-square&color=0A0A0A" alt="GitHub Forks" /></a>
  <a href="https://docs.matrixmcl.com"><img src="https://img.shields.io/badge/Docs-docs.matrixmcl.com-0A0A0A?style=flat-square" alt="Documentation" /></a>
</p>

<p align="center">
  <img src="https://img.shields.io/badge/Go-38.7%25-00ADD8?style=flat-square&logo=go&logoColor=white" alt="Go" />
  <img src="https://img.shields.io/badge/Solidity-26.3%25-363636?style=flat-square&logo=solidity&logoColor=white" alt="Solidity" />
  <img src="https://img.shields.io/badge/JavaScript-16.9%25-F7DF1E?style=flat-square&logo=javascript&logoColor=black" alt="JavaScript" />
  <img src="https://img.shields.io/badge/TypeScript-11.1%25-3178C6?style=flat-square&logo=typescript&logoColor=white" alt="TypeScript" />
  <img src="https://img.shields.io/badge/HTML-5.5%25-E34F26?style=flat-square&logo=html5&logoColor=white" alt="HTML" />
  <img src="https://img.shields.io/badge/Python-0.5%25-3776AB?style=flat-square&logo=python&logoColor=white" alt="Python" />
</p>

---

<h2 align="center">LLM のためのエージェントフレームワークと認知レイヤー。</h2>

<p align="center">
  Matrix は LLM をチャットの先へ進め、デジタル領域全体で実際の実行へと接続します <br/>
  そして、人間と機械が正確さを要求される仕事を協調して行えるようにします。
</p>

---

## Matrix とは？

Matrix は、Paxeer Network の Machine Economy ビジョンのために構築された認知レイヤーです。言語モデルを会話の枠を超えて実際の実行へ拡張し、エージェント間のオンチェーン金融調整、高リスクタスクの実行、そして重要かつ機密性の高い業務の安全な処理を可能にします。

多くのエージェントスタックがこの種の仕事で破綻する理由は、自然言語を最下層まで持ち込んでしまうからです。人間の言語は情報漏れのあるチャネルです。私たちが生物学的に推論し、知覚し、近似する方法は、生成するすべての文に染み出します。チャットでは問題ありません。しかし、エージェントが資金を移動し、不可逆な書き込みを行い、あるいは機密情報を保持している場合には問題になります。Matrix は、人間の推論や人間の言語が持つ曖昧さを、曖昧であってはならない部分に到達させることなく、人間と機械がまさにその種の仕事を協調して行うための方法を提供します。

これは 3 つのレイヤーによって実現されます。

## 3 つのレイヤー

### 1 — The Matrix Compiler (MCL)

**スタック全体の最上位に位置する司令塔。** MCL は、3 つの厳密なエージェントから成るコホートです。これらのエージェントは、人間の言語や人間の入力が持つ制約と曖昧さから切り離された closed-verb プロトコル上で、互いに通信し、計画し、行動します。機械の精度で協調し、それを資金、不可逆な操作、機密情報の取り扱いといった、現実世界の高リスクかつ機密性の高いタスクに適用します。

タスクが重大な結果を伴う領域に入った時点で、ここに送られます。3 つのエージェントは結果空間を計算し、作業に必要なすべての入力を保持していることを確認し、不足していれば明確化を求め、条件がそろった時点で仕様どおりに一度だけ実行します。推測に基づいて何かが実行されることはありません。

### 2 — The Cortex

**完全なメモリ、コンテキスト、そして不変状態のエンジン。** Cortex は、すべてのエージェントに永続的で耐久性のあるメモリを提供します。アクターごとのイベントタイムライン、アクティブな注意、型付き状態を持ち、すべて追記専用で、バイト単位で決定論的にリプレイできます。継続性は、モデルが各セッションで擬似的に演じなければならない幻想ではなくなります。ユーザーにとって実在し、エージェントにとって壊れることのないものになります。Matrix 上で動作するエージェントは、空の状態で目覚めません。

### 3 — The Loop Manager

**エージェントごとのループエンジン。** Loop Manager は、各エージェントについて、ユーザー、LLM、Cortex の間で絶えず流入・交換される情報を調整し、作業が重大な結果を伴うものになった瞬間に MCL パイプラインへエスカレーションします。これは、ターン、ツール、時間をまたいでエージェントの一貫性を維持するランタイムであり、どの時点で即興を続けるのではなく判断を上位へ引き上げるべきかを正確に把握します。

## 全体の連携

```
                        +-----------------------------+
                        |            User             |
                        +--------------+--------------+
                                       |
                                       v
                    +------------------+------------------+
                    |           Loop Manager              |
                    |     per-agent coordination loop     |
                    |     user  <->  LLM  <->  Cortex     |
                    +----+---------------+-----------+----+
                         |               |           |
                 reversible work         |       escalation
                         |               |           |
                         v               v           v
                    +---------+    +-----------+  +------------------+
                    |   LLM   |    |  Cortex   |  |  Matrix Compiler |
                    | (chat,  |    |  memory   |  |  (MCL)           |
                    |  tools) |    |  context  |  |  3 rigorous      |
                    +---------+    |  immutable|  |  closed-verb     |
                                   +-----------+  |  agents          |
                                                  +------------------+
                                                    money / on-chain /
                                                    irreversible /
                                                    confidential
```

デフォルトの会話エージェント（**Neo**）は Loop Manager 内で動作し、可逆な作業では shell、code、fetch、web ツールを使用できます。リスクが高まった瞬間、Loop Manager は MCL へエスカレーションし、その厳密性が不要になれば制御は Neo に戻ります。

## モジュール

ルートの Makefile は複数の兄弟 Go モジュールを駆動します。各モジュールは独自の `go.mod` を持ち、それぞれ独立して `go build` / `go test` を実行できます。上記 3 レイヤーはこれらのモジュールに対応しており、**MCL** はコンパイラコホート、**cortex** はメモリエンジン、**executor** は Loop Manager を実装します。


  <img src="https://www.readmecodegen.com/api/file-tree-embed?repo=paxlabs-inc%2Fmatrix-core&branch=main&maxDepth=1&foldersOnly=true&transparentBg=true&showHeader=true" alt="Dynamic File Tree" />


| モジュール | 役割 |
|--------|------|
| **MCL** | Matrix Compiler コホート。3 つの厳密な closed-verb エージェントが、機械レベルの精度で高リスクかつ機密性の高いタスクを計画・実行します。 |
| **cortex** | Pebble 上のアクター単位の型付きメモリエンジン。追記専用 journal、Merkle アンカー付き snapshot、バイト単位の決定論的 replay。永続的、不変、耐久性を備えます。 |
| **bridge** | MCL から cortex へのアダプター。明確なインターフェース境界を維持するための独立した Go モジュール。 |
| **executor** | Loop Manager。エージェント単位のループエンジン、ライフサイクル状態機械、MCP dispatch、ユーザー単位の daemon、Liaison narrator、end-to-end テストハーネス。 |
| **neo** | ループ内部で動作するデフォルトの会話エージェント。重大な結果を伴う操作は自動的に MCL へエスカレーションします。 |
| **gateway** | PAX クレジット台帳、無料枠 allowlist、料金表の適用を備えた従量制 LLM プロキシ。 |
| **router** | ユーザーごとの Fly Machine プロビジョニングと、wake-then-reverse-proxy 方式のフロントドア。 |
| **deus** | エージェントサービスのマーケットプレイス：registry、discovery、従量制 invocation、EIP-712 receipt、hosted execution。 |
| **tachyon** | エージェントネイティブな Solidity/EVM エンジン — compile、test、simulate、deploy。（git submodule） |
| **uwac** | Universal Web Agent Connector — ユーザーごとの MCP ツールを提供する OAuth vault。 |
| **layerx** | エージェント残高のための settlement fabric と custody spine。 |
| **chronos** | 集中型エージェント scheduler と wake-up system。 |
| **atlas** | 追加のインフラストラクチャ orchestration layer。 |
| **context** | コンテキスト管理サブシステム。 |
| **journal** | 決定論的な状態 replay のための追記専用 journal サブシステム。 |
| **knowledge** | 正規参照：matrix.kvx のプロジェクト状態、モデル、schema 定義。 |
| **skills** | SKILL.mtx capability manifest と、SKILL.md による文章形式の capability 説明。 |
| **tools** | MCP サーバー：paxeer、browser、tachyon、deus、uwac、web-search、media、cortex。 |
| **agents** | DID に紐付いた agent manifest（default.json、neo.json）と MCP サーバーテンプレート。 |
| **protocol** | プロトコル定義と wire format。 |
| **marketplace** | Deus marketplace と developer dashboard（Cloudflare Workers 上の React Router）。 |
| **client** | Matrix コンシューマーアプリケーション（Next.js / React）。 |
| **deploy** | daemon コンテナイメージ、Fly Machine deploy、共有サービスイメージ、box install script。 |

## 主な設計判断

- **Closed-verb coordination（D7）**：MCL エージェントは 10 個の closed verb — `find`、`acquire`、`build`、`modify`、`deliver`、`analyze`、`negotiate`、`schedule`、`monitor`、`delegate` — を介して協調します。これにより、エージェント間の intent は正確になり、runtime 中に prose から推測されることはありません。

- **8 個の closed object kind**：`service`、`model`、`agent`、`knowledge`、`intent`、`asset`、`plan`、`capability`。すべての operand はこのいずれかです。非構造化 blob が重大な実行領域へ渡されることはありません。

- **Replay invariant（セクション 13.4）**：派生状態は常に journal からバイト単位で同一に再構築できます。これはすべての pull request で `make ci` により強制されます。エージェントが行ったことはすべて追跡され、行っていないことが隠されることもありません。

- **Immutable memory**：Cortex は追記専用かつ content-addressed であるため、エージェントの継続性を密かに書き換えることはできません。エージェントにとって耐久性があり、ユーザーにとって信頼できます。

- **Signed receipts**：重大な実行はすべて EIP-712 receipt — inputs、outputs、cost、hash — で終了し、誰でも事後に検証できます。

## クイックスタート

### 前提条件

- Go 1.22+
- GNU Make 4.x
- Node.js 20+
- Python 3.11+
- Buildx 対応 Docker

### ビルド

```bash
# Clone the repository
git clone https://github.com/paxlabs-inc/matrix-core.git
cd matrix-core

# Build all nine Go modules
make build

# Install runnable CLIs into ./bin
make install

# Run tests (go test -count=1 -race ./... per module)
make test

# Full CI check (gofmt + vet + tests; mirrors GitHub Actions)
make ci
```

### 設定

```bash
# Copy the example environment file
cp .env.example .env

# Required for consequential (non-dry-run) execution:
#   FIREWORKS_API_KEY
#   TOGETHER_API_KEY
#
# Required for authenticated daemon mode:
#   MATRIX_DAEMON_TOKEN
```

### エージェントループを実行

```bash
./bin/mcl-execute walk \
  -prose "Summarise the README and write it to /tmp/summary.md" \
  -manifest    agents/default.json \
  -cortex-root ./runs/dev-cortex \
  -skills-root ./skills
```

### Daemon を起動

```bash
./bin/mcl-execute daemon \
  -addr        :8080 \
  -cortex-root ./runs/dev-cortex \
  -manifest    agents/default.json \
  -skills-root ./skills
```

## API リファレンス

daemon は、エージェントとの対話のための軽量 HTTP API を公開します。

| メソッド | パス | 用途 |
|--------|------|---------|
| `GET` | `/healthz` | Liveness probe + SSE broker 統計 |
| `POST` | `/chat` | エージェントと会話（Neo を介した conversational loop） |
| `GET` | `/events` | transcript をリアルタイム追跡するための Server-Sent Events stream |
| `POST` | `/messages` | 重大な結果を伴うメッセージを送信（MCL コホートへエスカレーション） |
| `GET` | `/intents/{id}` | intent ID によって intent envelope chain を読み取る |
| `GET` | `/me` | ユーザー単位の設定と identity |
| `POST` | `/shutdown` | graceful drain と shutdown |

## ドキュメント

| リソース | 説明 |
|----------|-------------|
| [アーキテクチャガイド](ARCHITECTURE.md) | システムマップ、モジュール境界、主要な invariant、設計根拠 |
| [コントリビューションガイド](CONTRIBUTING.md) | 開発環境のセットアップ、テスト規律、commit style、PR process |
| [セキュリティポリシー](SECURITY.md) | 脆弱性の開示と責任ある報告 |
| [Changelog](CHANGELOG.md) | Keep-a-Changelog 形式のリリースノート |
| [MCL ドキュメント](docs/MCL-docs/index.md) | MCL 言語リファレンス、closed-verb grammar、エージェント内部構造 |
| [Daemon デプロイガイド](deploy/daemon/README.md) | 本番デプロイ、Fly Machine 設定、運用 |
| [完全なドキュメント](https://docs.matrixmcl.com) | docs.matrixmcl.com の完全なドキュメントサイト |

## コントリビューション

Matrix Core はオープンソースであり、自由に **fork および変更** できます。ただし、`main` ブランチはコアチームのみが厳格に開発しています。依頼されていない pull request は通常 merge されず、外部からの変更は、その contributor と直接協力した場合にのみ受け入れられます。

何かを開く前に、[コントリビューションガイド](CONTRIBUTING.md) 冒頭の contribution policy を読んでください。Issue、bug report、security disclosure は常に歓迎します。

Contributors:
- dev-paxeer
- Andrew
- paxlabs-inc
- cursoragent
- Sidiora-Technologies

## ライセンス

Matrix Core は [Matrix-Protocol License](LICENSE.md) の下で source-available として提供されています。

Matrix Core は自由に閲覧、使用、デプロイ、統合できます。ソフトウェアを変更して再配布する場合は、変更内容を同じライセンスの下で公開する必要があります。以下の commercial trigger threshold を超えると、PaxLabs Inc. の商用ライセンスが必要です。

- 任意の 12 か月間に請求した手数料が **100,000 米ドル** を超える場合、または
- 管理下の流動性が **10,000,000 米ドル** を超える場合。

完全な条件については [LICENSE.md](LICENSE.md) を参照してください。

## 各言語版 README

- [スペイン語](README.es.md)
- [日本語](README.ja.md)
- [ポルトガル語](README.pt-BR.md)
- [ロシア語](README.ru.md)
- [中国語（簡体字）](README.zh-CN.md)

## 関連

- [Paxeer Network](https://paxeer.app) — Matrix Core が構築されている L1 blockchain。400ms blocks、400ms finality、agentic workload 向けに特化して設計されています。
- [PaxLabs](https://labs.paxeer.app) — 人間とエージェントの協働の未来を構築しています。

---

<p align="center">
  <a href="https://labs.paxeer.app"><strong>PaxLabs Inc.</strong></a> によって開発
</p>

<p align="center">
  <sub>SPDX-License-Identifier: Matrix-Protocol</sub>
</p>