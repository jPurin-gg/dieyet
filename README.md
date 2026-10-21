# dieyet
死(過去形)

スマホのカメラで商品のバーコードを読み、食べたものを記録して、1 日の栄養（エネルギー・たんぱく質・脂質・炭水化物・食塩相当量）を集計する Web アプリ。

- Go 製の単一バイナリ。画面（素の HTML / JS）を内蔵。データは筋トレ管理アプリの PostgreSQL に相乗りする（`users` を共有し、自分のテーブルは `food` スキーマに持つ）
- バーコードはブラウザ側で [zxing-wasm](https://github.com/Sec-ant/zxing-wasm) がカメラ映像から読む（`web/vendor/` に版を固定して同梱）
- ログインはニックネーム＋合言葉。初めての組み合わせなら、そのまま新しいアカウントになる。筋トレ管理アプリと同じルールで、同じ `users` を使うので、同じ組み合わせならどちらに入っても同じ人
- 栄養成分が「不明」な記録は 0 にせず、「？」として合計から外す
- 未登録の商品は Open Food Facts を照会して入力の下書きにする（日本の商品は無いことが多いので、手入力が基本）

技術選定の経緯と決定は [docs/技術選定.md](docs/技術選定.md)。

## 動かす

Go 1.27 以降と PostgreSQL（14 以降。本番は筋トレ管理アプリのもの）。

```sh
# 開発用の PostgreSQL
docker run -d --name dieyet-pg -e POSTGRES_PASSWORD=dev -p 127.0.0.1:5432:5432 postgres:18-alpine

# 開発（平文 HTTP なので Cookie の Secure 属性を外す）
DIEYET_DATABASE_URL='postgres://postgres:dev@127.0.0.1:5432/postgres?sslmode=disable' \
DIEYET_INSECURE_COOKIE=1 go run ./cmd/dieyet
# → http://127.0.0.1:8080
```

起動時に `food` スキーマと自分のテーブルを作る（マイグレーション）。`users` が無いデータベースでは、合意した形で `users` も作る。

本番は HTTPS（Cloudflare Tunnel）の後ろに置き、`DIEYET_INSECURE_COOKIE` は付けない。

| 環境変数 | 既定 | 意味 |
|---|---|---|
| `DIEYET_ADDR` | `127.0.0.1:8080` | 待ち受けアドレス |
| `DIEYET_DATABASE_URL` | （必須） | PostgreSQL の URL。例 `postgres://dieyet:secret@db.lan:5432/workout?sslmode=disable` |
| `DIEYET_INSECURE_COOKIE` | （空） | `1` で Cookie の `Secure` を外す。開発専用 |
| `DIEYET_OFF` | （有効） | `0` で Open Food Facts の照会を止める |
| `DIEYET_PRUNE_IDLE_DAYS` | `0`（消さない） | 食事の記録が 1 件もないアカウントを N 日後に消す。`users` を他のアプリと共有するときは 0 のままにする |

カメラ（`getUserMedia`）はブラウザが安全なコンテキストでしか許可しない。`localhost` は例外として使える。スマホの実機で試すなら HTTPS が要る。

### ビルドとテスト

```sh
CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o dieyet ./cmd/dieyet
go vet ./...
# テストは PostgreSQL が要る。テストごとに空のデータベースを作って消す
DIEYET_TEST_DATABASE_URL='postgres://postgres:dev@127.0.0.1:5432/postgres?sslmode=disable' go test ./...
```

`DIEYET_TEST_DATABASE_URL` が無いと DB を使うテストはスキップされる。

### Docker

```sh
docker build -t dieyet .
docker run -d --name dieyet -p 127.0.0.1:8080:8080 \
  -e DIEYET_DATABASE_URL='postgres://dieyet:secret@db.lan:5432/workout?sslmode=disable' dieyet
```

`FROM scratch` のイメージで、中身はバイナリと CA 証明書だけ。

## 筋トレ管理アプリと共有するもの

共有するのは `users` だけ。それ以外は各アプリが自分のテーブルを持ち、`user_id` でつなぐ。

**`users` の形**（筋トレ管理アプリが持ち主）

```sql
CREATE TABLE users (
  id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  nickname      text NOT NULL,          -- 一意ではない。身元は (nickname, 合言葉) の組み合わせ
  pass_hash     text NOT NULL,          -- PHC 文字列。下を参照
  created_at    timestamptz NOT NULL DEFAULT now(),
  last_login_at timestamptz
);
CREATE INDEX users_nickname ON users (nickname);
```

**合言葉のハッシュ**: argon2id を PHC 文字列で保存する。どの言語の argon2 ライブラリでも作れて検証できる。

```
$argon2id$v=19$m=19456,t=2,p=1$<salt の base64 (padding なし)>$<hash の base64 (padding なし)>
```

dieyet が作るときのパラメータは m=19456 KiB, t=2, p=1, salt 16 バイト, hash 32 バイト。検証は文字列の中のパラメータに従うので、筋トレ側が別のパラメータで作っても通る。

**ログインのルール**（両方のアプリで同じ）: ニックネームで `users` を引き、合言葉がどれかのハッシュと一致すればその人。一致する行が無ければ、確認なしで新しい行を作る。

**dieyet 用の DB ロール**（筋トレ側の DB で一度だけ実行）

```sql
CREATE ROLE dieyet LOGIN PASSWORD '...';
GRANT CONNECT ON DATABASE workout TO dieyet;
GRANT SELECT, INSERT, UPDATE, REFERENCES ON users TO dieyet;  -- 初回ログインで行を作り last_login_at を更新する。REFERENCES は food.* から users への外部キー用
GRANT USAGE, SELECT ON SEQUENCE users_id_seq TO dieyet;        -- id の採番
CREATE SCHEMA food AUTHORIZATION dieyet;                 -- dieyet のテーブルはここに。起動時に自分で作る
```

筋トレ側が食事の記録を読むには `GRANT USAGE ON SCHEMA food` と `GRANT SELECT ON ALL TABLES IN SCHEMA food` を筋トレ側のロールに付ける。`food.logs` は `user_id`、`eaten_on`（端末のローカル日付）、`deleted_at IS NULL` で絞る。

**セッション**はアプリごとに別（dieyet は `food.sessions`）。ホスト名が違えば Cookie は共有できないので、各アプリで 1 回ずつログインする。

## API

すべて JSON。認証は Cookie（`__Host-dieyet_session`）。別オリジンからの書き込みは `Sec-Fetch-Site` / `Origin` で拒否する。

| メソッドとパス | 内容 |
|---|---|
| `POST /api/login` | `{nickname, passphrase}`。組み合わせが既存ならログイン、初めてなら作成。`{user, created}` |
| `POST /api/logout` | セッションを失効 |
| `GET /api/me` | ログイン中の利用者 |
| `GET /api/products/{barcode}` | `{found, product, suggestion}`。未登録なら Open Food Facts の下書きを `suggestion` に |
| `PUT /api/products/{barcode}` | 商品を登録・更新（共有の商品マスタ）。`{name, brand, unit, kcal, protein_g, fat_g, carbs_g, salt_g}`。不明な項目は省略か `null` |
| `GET /api/logs?date=YYYY-MM-DD` | その日の記録と合計 `{date, entries, totals}` |
| `POST /api/logs` | 記録。`{barcode, servings, date}` か、バーコードなしで `{name, unit, servings, date, kcal, ...}` |
| `DELETE /api/logs/{id}` | 自分の記録を消す |

バーコードは 8 桁か 12〜14 桁の数字。UPC-A（12 桁）は先頭に 0 を足して EAN-13 に正規化する。

## 設計上の約束

- 合言葉は argon2id でハッシュして保存。ログインは IP とニックネームごとに回数制限
- 利用者の主キーは内部の番号。ニックネームは重複してよい
- DB は筋トレ管理アプリと共有する。`users` は共有、このアプリが持つテーブルは `food` スキーマ（`food.products`、`food.logs`、`food.sessions`、`food.schema_migrations`）。他のアプリは自分のスキーマだけ書き、`food` は読むだけにする
- 記録には、その時点の商品の栄養値を写して保存する。後から商品を直しても過去の記録は変わらない
- 削除は論理削除（`deleted_at`）
- 日付は端末のローカル日付（`eaten_on`）で区切る。サーバーは時刻を UTC で持つ
- 依存は `github.com/jackc/pgx/v5`（PostgreSQL ドライバ）と `golang.org/x/crypto`（argon2）のみ。`CGO_ENABLED=0` で静的リンク

## 開発中の注意

- サーバーに置いたファイルの秘密情報（DB、Cookie など）はコミットしない。`.gitignore` 参照
- `web/vendor/zxing-wasm/VERSION.txt` に同梱した版とハッシュを記録している。更新するときは同じ手順で確認してから差し替える
