# 練習問題 (CKA / CKAD)

niwakube のクラスタで CKA / CKAD の練習をする機能。Web UI の「練習」から使う。

- 問題は `internal/practice/problems/<id>.yaml` に 1 問 1 ファイルで書き、niwakubed に埋め込む
- **本物の試験問題は使わない** (受験規約で公開が禁じられている)。
  公開されている出題範囲 ([CNCF のカリキュラム](https://github.com/cncf/curriculum)) に沿ったオリジナルの問題にする
- 採点はクラスタのオブジェクトを API で読んで確かめる。ノードの中でシェルは動かさない

## 流れ

1. **出題** (`POST /api/practice/problems/{id}/setup {"cluster":"demo"}`):
   1. ノードに入れるクラスタなら、全問の `cleanup` を動かす (ほかの問題がノードに残した taint や cordon などを戻す)
   2. `namespaces` に書いた Namespace を消し、消え終わるのを待つ
   3. `setup` のマニフェストを server-side apply し、`nodeSetup` をノードで動かす

   何度押しても出題直後の状態に戻る (やり直しにも使う)
2. 受験者が Web UI のターミナル (kubectl) で解く
3. **採点** (`POST /api/practice/problems/{id}/check {"cluster":"demo"}`):
   `checks` の項目ごとに合否を出す。点数は「配点 × 合格した項目の割合」(切り捨て)。
   答えが分かってしまうので、実際の値は返さない
4. **模範解答** (`POST /api/practice/problems/{id}/solve {"cluster":"demo"}`): `answer` のマニフェストと `answerRun` を当てる。
   CI が満点になることを確かめるのに使い、Web UI の「模範解答をクラスタに当てる」でも使える

## ノードに入る問題 (CKA)

- クラスタを作るときに、niwakubed がクラスタごとの SSH の鍵を作って全ノードに入れる (ワーカーを足したときも)。
  鍵はそのクラスタのノードにしか入れない。**この機能より前に作ったクラスタには鍵が無い**ので、ノードに入る問題は出せない
- Web UI のターミナルからは `ssh cp`、`ssh worker` (2 台目以降は `worker2` ...) かノード名で入れる。ユーザーは ubuntu で、sudo できる
- niwakubed は同じ鍵で `nodeSetup`・`cleanup`・`answerRun` とノードの採点項目を動かす (コマンドは問題のファイルに書いたものだけ)
- Talos はノードにシェルも SSH も無いので、ノードに入る問題は出せない
- kubeadm のコントロールプレーンには、etcd のイメージと同じバージョンの etcdctl / etcdutl を入れる

## 問題の書き方

```yaml
id: ckad-readiness-probe        # ファイル名と同じ。英小文字・数字・-
exam: CKAD                      # CKA | CKAD
domain: アプリの監視と保守        # カリキュラムの分野
title: Deployment に readinessProbe を付ける
weight: 6                       # 配点
requires: kubeadm               # 出せるディストリ (省略するとどれでも)
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

ノードを使う項目 (`node` は `cp` / `worker` / `cp2` / `worker2` ...。コマンドは ubuntu ユーザーの bash で動くので、root が要るものは sudo を付ける):

```yaml
nodeSetup:                      # 出題のときにノードで動かす (マニフェストの後)
  - node: worker
    run: sudo systemctl disable --now kubelet
cleanup:                        # この問題がノードに残す変更を戻す。どの問題を出すときも先に動く。何度動かしてもよいこと
  - node: worker
    run: sudo systemctl enable --now kubelet
checks:
  - description: ワーカーの kubelet が動いている
    node: worker                # ノードでコマンドを動かし、終了コードが 0 なら合格
    run: systemctl is-active --quiet kubelet
  - description: ワーカーが Ready
    apiVersion: v1
    kind: Node
    name: '{{worker}}'          # {{cp}} / {{worker}} はノードの名前に置き換わる (name、selector、fieldSelector、jsonpath、equals、run)
    jsonpath: '{.status.conditions[?(@.type=="Ready")].status}'
    equals: "True"
  - description: deployer は Pod を消せない
    canI:                       # 権限 (kubectl auth can-i --as=system:serviceaccount:<ns>:<name> と同じ)
      serviceAccount: team-a/deployer
      verb: delete
      resource: pods            # 複数形。API グループが要るものは group: apps
      namespace: team-a
    allowed: false              # できる (true) / できない (false) なら合格
answerRun:                      # 解いた状態にするノードのコマンド
  - node: worker
    run: sudo systemctl enable --now kubelet
```

- オブジェクトは `name` のほか、`selector` (ラベル) と `fieldSelector` (`spec.nodeName={{worker}}` など) で一覧から選べる

- `jsonpath` は `kubectl -o jsonpath` と同じ書き方。結果は空白区切りの 1 つの文字列にして比べる。
  `selector` のときは `{"items": [...]}` に対して評価する
- 状態を見る項目 (`.status` の Ready など) は、すぐには満たされない。CI は解答を当ててから最大 300 秒、満点になるまで採点を繰り返す
- `default` や `kube-system` などは `namespaces` に使えない
- イメージは CI でも取得できるもの (`registry.k8s.io/e2e-test-images/agnhost:2.39` など) を使う

## テスト

- `go test ./internal/practice/`: すべての問題を読み込み、形を確かめる。解答を当てると、本物のクラスタが無くても確かめられる項目
  (`.status`、コントローラーや kubelet が作るもの、ノードのコマンド、権限 以外) がすべて合格することを確かめる
- E2E (`scripts/e2e-cluster.sh`): 本物のクラスタで、そのディストリで出せる全問を「出題 → 満点でない → 模範解答を当てる → 満点」と確かめる
