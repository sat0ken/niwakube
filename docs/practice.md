# 練習問題 (CKA / CKAD)

niwakube のクラスタで CKA / CKAD の練習をする機能。Web UI の「練習」から使う。

- 問題は `internal/practice/problems/<id>.yaml` に 1 問 1 ファイルで書き、niwakubed に埋め込む
- **本物の試験問題は使わない** (受験規約で公開が禁じられている)。
  公開されている出題範囲 ([CNCF のカリキュラム](https://github.com/cncf/curriculum)) に沿ったオリジナルの問題にする
- 採点はクラスタのオブジェクトを API で読んで確かめる。ノードの中でシェルは動かさない

## 流れ

1. **出題** (`POST /api/practice/problems/{id}/setup {"cluster":"demo"}`):
   `namespaces` に書いた Namespace を消し、消え終わるのを待ってから `setup` のマニフェストを server-side apply する。
   何度押しても出題直後の状態に戻る (やり直しにも使う)
2. 受験者が Web UI のターミナル (kubectl) で解く
3. **採点** (`POST /api/practice/problems/{id}/check {"cluster":"demo"}`):
   `checks` の項目ごとに合否を出す。点数は「配点 × 合格した項目の割合」(切り捨て)。
   答えが分かってしまうので、実際の値は返さない

## 問題の書き方

```yaml
id: ckad-readiness-probe        # ファイル名と同じ。英小文字・数字・-
exam: CKAD                      # CKA | CKAD
domain: アプリの監視と保守        # カリキュラムの分野
title: Deployment に readinessProbe を付ける
weight: 6                       # 配点
namespaces: [web]               # この問題の Namespace。出題のたびに消して作り直す (setup で作ること)
setup: |                        # 出題前に適用するマニフェスト (複数ドキュメント可)
  apiVersion: v1
  kind: Namespace
  metadata:
    name: web
  ...
task: |                         # 問題文
  Namespace web の Deployment api に ...
checks:                         # 採点の項目
  - description: readinessProbe のパスが /hostname   # 受験者に見せる説明
    apiVersion: apps/v1
    kind: Deployment
    namespace: web
    name: api                   # name か selector (ラベルセレクター) のどちらか
    jsonpath: '{.spec.template.spec.containers[0].readinessProbe.httpGet.path}'
    equals: /hostname           # equals / contains / exists: true のどれか。無ければ「オブジェクトがある」で合格
hint: |                         # ヒント (Web UI で開いたときだけ見える)
solution: |                     # 解説と解き方の例
answer: |                       # 解いた状態にするマニフェスト (CI が当てて満点になることを確かめる)
```

- `jsonpath` は `kubectl -o jsonpath` と同じ書き方。結果は空白区切りの 1 つの文字列にして比べる。
  `selector` のときは `{"items": [...]}` に対して評価する
- 状態を見る項目 (`.status` の Ready など) は、すぐには満たされない。CI は解答を当ててから最大 300 秒、満点になるまで採点を繰り返す
- `default` や `kube-system` などは `namespaces` に使えない
- イメージは CI でも取得できるもの (`registry.k8s.io/e2e-test-images/agnhost:2.39` など) を使う

## テスト

- `go test ./internal/practice/`: すべての問題を読み込み、形を確かめる。解答を当てると `.status` 以外の項目がすべて合格し、
  出題直後は合格しない項目があることを確かめる
- E2E (`scripts/e2e-cluster.sh`): 本物のクラスタで、全問を「出題 → 満点でない → 解答を当てる → 満点」と確かめる
