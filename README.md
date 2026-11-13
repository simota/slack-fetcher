# slack-fetcher

SlackスレッドのURLを受け取り、Slack Web API経由でスレッド内の全メッセージと添付ファイルを取得し、ローカルLLMエージェント（Claude Code 等）向けの**YAMLフロントマター付き構造化Markdown**へ変換するCLIツールです。

- **テキスト系添付ファイル**（`snippet` / `text/*` / JSON / YAML / XML 等、既定 `256 KiB` 以下かつ有効なUTF-8）はコードブロックとしてMarkdown内にインライン展開します。
- **バイナリ添付ファイル**（PDF・画像・Office文書・アーカイブ・上限超過テキスト等）はローカルディレクトリへ保存し、Markdown内からは絶対パス・MIMEタイプ・サイズを参照します。

設計判断とスコープ境界は [docs/plans/slack-fetcher-cli.md](docs/plans/slack-fetcher-cli.md) を参照してください。

---

## ビルド・インストール

Go 1.24 以降が必要です（標準ライブラリのみで動作し、外部ランタイム依存はありません）。

```sh
# 個人利用向けビルド
go build -o bin/slack-fetcher ./cmd/slack-fetcher

# チーム・組織内配布用ビルド（Client ID を埋め込む場合、Client Secret は不要・埋め込み禁止）
go build \
  -ldflags "-X github.com/shingoimota/slack-fetcher/internal/auth.DefaultClientID=<YOUR_SLACK_CLIENT_ID>" \
  -o bin/slack-fetcher ./cmd/slack-fetcher

# もしくは PATH 配下へインストール
go install ./cmd/slack-fetcher
```

---

## 認証セットアップ（ワークスペース共有運用）

### 1. 管理者による Slack App 作成（対象ワークスペースで1回のみ）

管理者が対象ワークスペースに Slack App を1つ作成して承認すると、各メンバーは個別にアプリを作成せず `slack-fetcher auth login` だけで自分自身の権限の User Token (`xoxp-` / `xoxe.xoxp-`) を取得できます。

1. Slack API 管理画面（Your Apps -> Create New App -> From a manifest）で以下のマニフェストを用いてアプリを作成します。
2. App設定画面の **OAuth & Permissions** で **PKCE を有効化（Public Client 化）** します（`client_secret` を配布バイナリに含めず安全にトークン交換するため。なお PKCE 有効化は不可逆設定であり、リフレッシュトークンは30日間有効・CLIが自動更新します）。
3. 確認した **Client ID** を上記 `-ldflags` でバイナリに埋め込んで配布するか、メンバーへ `SLACK_CLIENT_ID` として共有します。

```yaml
display_information:
  name: slack-fetcher
  description: Read Slack threads and attachments into structured Markdown
oauth_config:
  redirect_urls:
    - http://localhost:53682/callback
  scopes:
    user:
      - channels:history
      - files:read
      - users:read
      - groups:history
      - im:history
      - mpim:history
settings:
  org_deploy_enabled: false
  socket_mode_enabled: false
  token_rotation_enabled: false
```

### 2. 利用者によるログイン

プロセス一覧からの漏洩を防ぐため `--token` フラグは提供していません。以下のいずれかで認証してください。

```sh
# 方法1: ブラウザで OAuth PKCE ログイン（Client ID 埋め込み済みバイナリの場合）
slack-fetcher auth login

# Client ID をコマンドまたは環境変数で渡す場合
slack-fetcher auth login --client-id "<YOUR_SLACK_CLIENT_ID>"
# SSH環境などブラウザ自動起動できない場合
slack-fetcher auth login --no-browser

# 方法2: 既に発行済みの xoxp- / xoxb- トークンを標準入力から保存
printf '%s\n' "$MY_TOKEN" | slack-fetcher auth login --token-stdin

# 方法3: 環境変数で直接指定（保存済み認証情報より最優先）
export SLACK_TOKEN="xoxp-..."

# 認証状態の確認・削除（生トークンはマスク表示）
slack-fetcher auth status
slack-fetcher auth logout
```

**認証情報の保存先**:
- **macOS**: macOS Keychain（サービス名 `slack-fetcher`、環境変数 `SLACK_FETCHER_KEYCHAIN_SERVICE` で変更可）
- **Linux / Windows**: `$XDG_CONFIG_HOME/slack-fetcher/credentials.json` または OS 標準設定ディレクトリ配下（パーミッション `0600`）

---

## 使い方

```sh
# 標準出力へ構造化Markdownを出し、そのままローカルLLMへ渡す
slack-fetcher "https://example.slack.com/archives/C01234567/p1700000000123456" \
  | claude -p "このスレッドの決定事項と未解決課題を整理して"

# 返信パーマリンクからスレッド全体を取得し、出力先と添付保存先を指定する
slack-fetcher \
  --attachment-dir ./tmp/slack-thread \
  --output ./tmp/slack-thread/thread.md \
  "https://example.slack.com/archives/C01234567/p1700000099654321?thread_ts=1700000000.123456&cid=C01234567"
```

### 対応URL形式
- `https://<workspace>.slack.com/archives/<CHANNEL_ID>/p<16-digit-ts>`
- `https://<workspace>.slack.com/archives/<CHANNEL_ID>/p<16-digit-ts>?thread_ts=<ts>`
- `https://app.slack.com/client/<TEAM_ID>/<CHANNEL_ID>/thread/<CHANNEL_ID>-<ts>`

### フラグ

| フラグ | 既定値 | 説明 |
|---|---|---|
| `--attachment-dir <dir>` | `$TMPDIR/slack-fetcher/<channel>_<thread_ts>` | バイナリ添付の保存先ディレクトリ |
| `-o, --output <file>` | `stdout` | 構造化Markdownの出力先（進捗・警告は常に `stderr`） |
| `--max-inline-bytes <n>` | `262144` (256 KiB) | テキスト系添付をインライン展開する最大バイト数 |
| `--max-download-bytes <n>` | `52428800` (50 MiB) | 単一添付ファイルの最大ダウンロードバイト数 |
| `--timeout <duration>` | `60s` | コマンド全体のタイムアウト |

### 終了コード
- `0`: 取得・変換成功（退職者 `users.info` 失敗や削除済みファイル等の部分スキップは `stderr` 警告・出力内マーカーで記録して `0`）
- `2`: 引数不正・URL形式不正
- `3`: 認証・権限エラー（未認証、`invalid_auth`, `token_expired`, `missing_scope`, `not_in_channel`）
- `4`: スレッド不在（`thread_not_found`, `channel_not_found`）またはリトライ上限超過等の通信・入出力失敗
