# Slack Thread Fetcher CLI 設計判断記録 (`slack-fetcher`)

> CLIの使い方・フラグ・終了コード契約は [README.md](../../README.md) と各パッケージの実装・テストを参照してください。本ドキュメントはコードから読み取れない**スコープ境界**と**設計判断の理由・撤回条件**のみを記録します。

## 1. 用語定義・スコープ境界

### 用語定義 (`Terms`)
- **テキスト系添付 (`InlineAttachment`)**: `file.mode == "snippet"`、またはテキスト系MIMEタイプ（`text/*`, JSON, XML, YAML, JS/TS, SQL 等）に該当し、`--max-inline-bytes` 以下かつ有効なUTF-8である添付ファイル。構造化Markdown内にコードブロックとしてインライン展開する。
- **バイナリ添付 (`SavedAttachment`)**: PDF・画像・Office文書・アーカイブ等の非テキストファイル、またはインライン上限超過・非UTF-8の添付ファイル（単一上限 `--max-download-bytes`）。ローカルディレクトリへ保存し、Markdown内からは絶対パス・MIMEタイプ・サイズを参照する。

### スコープ外 (`Excludes`)
- **バイナリからのテキスト抽出・OCR**: PDF/DOCXパースや画像OCRはCLI内で行わない（マルチモーダル対応ローカルLLM側が保存先パスを直接読む前提）。
- **`--token` CLIフラグ**: `ps` 等のプロセス一覧からトークンが漏洩するため提供しない（手動登録は `--token-stdin` を使用）。
- **公開配布向け OAuth 中継サーバーと `client_secret` のバイナリ埋め込み**: 各ワークスペース内での運用を前提とするため外部Webサーバーは持たず、Public Client PKCE（`client_id` + `code_verifier`）のみを採用する。
- **スレッド外検索・横断クロール・永続キャッシュ**: `conversations.history` やDBキャッシュは持たず、1コマンド＝1スレッド取得のステートレス実行に限定する。
- **LLM APIの直接呼び出し**: UNIXパイプやファイル渡しで任意のLLM CLIと組み合わせるため、本ツール自体はLLM SDKを持たない。

---

## 2. アーキテクチャ意思決定 (`Decisions`)

### Decision 1: 実装言語とランタイム
```text
Decision: Go（標準ライブラリ中心の単一バイナリCLI）
Because: 任意のリポジトリ作業ディレクトリからランタイム非依存・ゼロセットアップで即座に呼び出せる配布性と起動速度が支配軸となるため。HTTP通信・ストリーミング保存・パス無害化・並行制御が標準ライブラリで完結し、依存保守コストも最小化できる。
Rejected:
  - TypeScript (Node.js / Bun) — 呼び出し環境にランタイムや依存解決を要求し、他言語プロジェクト作業中に呼ぶ汎用CLIとしてのポータビリティで単一バイナリに劣る。
  - Python — バイナリからのテキスト抽出を行わないため文書処理エコシステムの優位性がなく、venv等の環境維持コストだけが残る。
Costs: Slack APIの柔軟なJSONペイロード（多様なsubtypeやattachments）の型定義が動的言語よりやや冗長になる。
Revisit if: CLI単体でのPDF/Officeテキスト抽出や複雑なHTML変換が必須要件に加わった場合。
```

### Decision 2: Slack認証モデル（ワークスペース共有向け）
```text
Decision: ワークスペースごとに1つの PKCE 有効 Slack App（Public Client）に対する `auth login` ブラウザ認可＋自動リフレッシュを標準とし、環境変数 `SLACK_TOKEN` および `--token-stdin` による手動トークン登録も併用する。保存先は macOS Keychain（`security -i` + `-X` hex で argv 漏洩防止）および Linux/Windows の `0600` 設定ファイルとする。
Because: 利用メンバーが各自で Slack App を作成する手間をなくしつつ、管理者の強力な User Token を共有せずに各メンバー自身の閲覧権限だけのトークンを安全に発行・保持できることが支配軸となるため。
Rejected:
  - 管理者発行の単一 User Token 共有 — 管理者個人のDM・プライベートチャンネルまで全利用者が閲覧可能になり権限境界が崩壊する。
  - バイナリへの client_secret 埋め込み — 配布バイナリからシークレット抽出が可能であり不要なリスクを負う。
  - Bot Token専用設計 — 対象チャンネルごとに事前の `/invite` が必須となり、`not_in_channel` エラーで作業が頻繁に中断される。
Costs: 利用者は初回およびリフレッシュトークン失効後（PKCEアプリは30日）に `auth login` の再認可が必要になる。
Revisit if: ワークスペースを跨いだ不特定多数向けの単一公開アプリとして配布する必要が生じ、HTTPS コールバックを持つ OAuth 中継サーバーの運用を許容する場合。
```

### Decision 3: LLM向け出力フォーマット
```text
Decision: YAMLフロントマター付き構造化Markdown（標準出力、添付バイナリはローカルディレクトリ保存＋絶対パス記載）
Because: ローカルLLMエージェントのトークン効率・文脈境界の認識精度と、人間によるパイプ前後の目視確認性の両立が支配軸となるため。
Rejected: JSON単一出力 — 改行・コード片のエスケープでトークン効率と人間の可読性が低下する。
Costs: 下流で厳密なスキーマ再パースを行うプログラム連携には向かない。
Revisit if: LLMではなく機械的なデータ収集基盤が主たる呼び出し元となり、`--format json` が必要になった場合。
```

---

## 3. 最重要前提と実環境での検証方法 (`Falsification`)

- **最重要前提**: 対象Slackワークスペースにおいて、利用者が `channels:history`, `files:read`, `users:read`（必要に応じて `groups:history`, `im:history`, `mpim:history`）を持つトークンを発行でき、`conversations.replies` と `url_private_download` が管理者ポリシー（アプリ承認制限やEnterprise GridのファイルDL制限）で遮断されないこと。
- **安価な検証方法**: 実スレッド1件に対して `curl` で以下2点を確認する。いずれかが失敗する場合、Slack Web API方式自体が不成立となるため設計を見直す。
  1. `GET https://slack.com/api/conversations.replies?channel=<CHANNEL_ID>&ts=<THREAD_TS>&limit=5`（`Authorization: Bearer $SLACK_TOKEN`）が `"ok": true` と `files[].url_private_download` を返すこと。
  2. 取得した `url_private_download` への認証付きGETがHTMLログイン画面へ飛ばされず、実ファイルバイト列（`200 OK`）を返すこと。
