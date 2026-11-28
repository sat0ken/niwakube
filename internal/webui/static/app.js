// niwakube の Web UI。ビルドの手順は無く、niwakubed に埋め込んで配信する。
// 画面はハッシュ (#/...) で切り替え、表示中の画面は 5 秒ごとに更新する。

const app = document.getElementById("app");
const REFRESH_MS = 5000;
// SSH 経由用の kubeconfig の proxy-url (PC で ssh -D 1080 を使う前提)
const SSH_PROXY = "socks5://localhost:1080";
// ダッシュボードで選んだクラスタ (5 秒ごとの再描画でも消えないようにここで持つ)
const selectedClusters = new Set();

// ---------- 小さな道具 ----------

// html`...` はテンプレートの値を HTML エスケープする。エスケープしたくない断片は raw() で包む。
class Raw { constructor(s) { this.s = s; } toString() { return this.s; } }
const raw = (s) => new Raw(s);
const esc = (v) => String(v ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
function html(strings, ...values) {
  let out = strings[0];
  values.forEach((v, i) => {
    if (Array.isArray(v)) out += v.map((x) => (x instanceof Raw ? x.s : esc(x))).join("");
    else out += v instanceof Raw ? v.s : esc(v);
    out += strings[i + 1];
  });
  return raw(out);
}

async function api(method, path, body) {
  const opts = { method, headers: {} };
  if (body !== undefined) {
    opts.headers["Content-Type"] = "application/json";
    opts.body = JSON.stringify(body);
  }
  const res = await fetch(path, opts);
  if (res.status === 401 && path.startsWith("/api/")) {
    // ログインが切れた (または、まだしていない)。ログインの画面に切り替える
    showLogin();
    throw new Error("ログインしてください");
  }
  const text = await res.text();
  let data = null;
  try { data = text ? JSON.parse(text) : null; } catch { data = text; }
  if (!res.ok) throw new Error((data && data.error) || `${res.status} ${res.statusText}`);
  return data;
}

let toastTimer;
function toast(msg, isError = false) {
  const t = document.getElementById("toast");
  t.textContent = msg;
  t.className = "show" + (isError ? " error" : "");
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { t.className = ""; }, isError ? 7000 : 3500);
}

const gib = (mib) => (mib / 1024).toFixed(mib % 1024 === 0 ? 0 : 1);
const pct = (a, b) => (b > 0 ? Math.min(100, Math.round((a / b) * 100)) : 0);

function phaseBadge(phase) {
  const cls = { ready: "ok", provisioning: "info", degraded: "warn", stopping: "", stopped: "" }[phase] ?? "warn";
  const label = { ready: "Ready", provisioning: "構築中", degraded: "異常", stopping: "停止中", stopped: "停止" }[phase] ?? phase;
  return html`<span class="badge ${cls}">${label}</span>`;
}

function stateBadge(state) {
  const cls = { running: "ok", stopped: "", paused: "info", unknown: "warn", "not-found": "danger" }[state] ?? "warn";
  const label = { running: "実行中", stopped: "停止", paused: "一時停止", unknown: "不明", "not-found": "なし" }[state] ?? state;
  return html`<span class="badge ${cls}">${label}</span>`;
}

// クラスタの進行中の操作 (operation) を表示用の文にする
function operationLabel(op) {
  if (!op) return "";
  if (op.startsWith("failed:")) return `失敗: ${op.slice(7).trim()}`;
  const snap = op.match(/^(snapshot|restore) ([^:]+): (.+)$/);
  if (snap) {
    const step = snap[3]
      .replace(/^starting nodes$/, "ノードを起動しています")
      .replace(/^stopping nodes$/, "ノードを止めています")
      .replace(/^starting$/, "準備しています")
      .replace(/^copying NFS data$/, "NFS のデータを複製しています")
      .replace(/^restoring NFS data$/, "NFS のデータを戻しています")
      .replace(/^copying (\S+) (.+)$/, "$1 を複製しています $2")
      .replace(/^restoring (\S+) (.+)$/, "$1 を戻しています $2")
      .replace(/^removing (\S+)$/, "$1 を消しています");
    return `${snap[1] === "snapshot" ? "スナップショット" : "復元"} ${snap[2]}: ${step}`;
  }
  for (const [re, label] of [[/^resizing (.+)$/, "サイズを変更中"], [/^adding (.+)$/, "ワーカーを追加中"], [/^removing (.+)$/, "ワーカーを削除中"]]) {
    const m = op.match(re);
    if (m) return `${label}: ${m[1]}`;
  }
  return op;
}

// クラスタの画面で操作が進んでいる間は、画面全体を 5 秒ごとに更新する
let clusterOpActive = false;

function setNav(name) {
  document.querySelectorAll("nav a").forEach((a) => a.classList.toggle("active", a.dataset.nav === name));
}

// 入力中 (フォームにフォーカスがある) は自動更新で書き換えない
const isEditing = () => document.activeElement && document.activeElement.closest && document.activeElement.closest("form");

// ---------- 画面: ダッシュボード ----------

async function viewHome() {
  setNav("home");
  const [host, clusters, cache] = await Promise.all([api("GET", "/api/host"), api("GET", "/api/clusters"),
    api("GET", "/api/cache").catch(() => null)]); // キャッシュが無効なら出さない
  document.getElementById("host-name").textContent = `${host.hostname} · ${host.version}`;
  const memUsed = host.memory.totalMiB - host.memory.availableMiB;
  const diskUsed = host.disk.totalGiB - host.disk.freeGiB;
  app.innerHTML = html`
    <h1>ダッシュボード</h1>
    <div class="grid">
      <div class="card stat">
        <div class="label">CPU</div>
        <div class="value">${host.allocated.cpus} / ${host.cpus}</div>
        <div class="sub">動いている VM に割り当てた vCPU / ホストの CPU</div>
        <div class="meter"><span data-pct="${pct(host.allocated.cpus, host.cpus)}"></span></div>
      </div>
      <div class="card stat">
        <div class="label">メモリ</div>
        <div class="value">${gib(memUsed)} / ${gib(host.memory.totalMiB)} GiB</div>
        <div class="sub">VM に割り当て ${gib(host.allocated.memoryMiB)} GiB</div>
        <div class="meter"><span data-pct="${pct(memUsed, host.memory.totalMiB)}"></span></div>
      </div>
      <div class="card stat">
        <div class="label">ディスク (${host.disk.path})</div>
        <div class="value">${diskUsed.toFixed(0)} / ${host.disk.totalGiB.toFixed(0)} GiB</div>
        <div class="sub">空き ${host.disk.freeGiB.toFixed(0)} GiB</div>
        <div class="meter"><span data-pct="${pct(diskUsed, host.disk.totalGiB)}"></span></div>
      </div>
      <div class="card stat">
        <div class="label">VM</div>
        <div class="value">${host.allocated.running} / ${host.allocated.machines}</div>
        <div class="sub">実行中 / 全体</div>
      </div>
      ${cache ? html`<div class="card stat">
        <div class="label">イメージのキャッシュ</div>
        <div class="value">${(cache.bytes / 2 ** 30).toFixed(1)}${cache.maxBytes ? ` / ${(cache.maxBytes / 2 ** 30).toFixed(0)}` : ""} GiB</div>
        <div class="sub">レイヤー ${cache.blobs} 個 · 起動してからキャッシュで返した量 ${(cache.bytesFromCache / 2 ** 30).toFixed(1)} GiB${cache.lastPrune && cache.lastPrune.removed ? ` · 前回の整理で ${cache.lastPrune.removed} 個消去` : ""}</div>
        ${cache.maxBytes ? html`<div class="meter"><span data-pct="${pct(cache.bytes, cache.maxBytes)}"></span></div>` : ""}
      </div>` : ""}
    </div>
    <div class="card">
      <h1>クラスタ <a class="button" href="#/clusters/new">クラスタを作る</a></h1>
      ${clusters.length === 0
        ? html`<div class="empty">まだクラスタはありません。「クラスタを作る」から始めましょう。</div>`
        : html`<div class="table-wrap"><table>
            <thead><tr><th class="check"><input type="checkbox" id="sel-all" title="すべて選ぶ"
              ${clusters.every((c) => selectedClusters.has(c.name)) ? raw("checked") : ""}></th>
              <th>名前</th><th>状態</th><th>ディストリ</th><th>CNI</th><th class="num">ノード</th><th>API</th></tr></thead>
            <tbody>${clusters.map((c) => html`<tr>
              <td class="check"><input type="checkbox" data-sel="${c.name}" ${selectedClusters.has(c.name) ? raw("checked") : ""}></td>
              <td><a href="#/clusters/${encodeURIComponent(c.name)}"><strong>${c.name}</strong></a></td>
              <td>${phaseBadge(c.status.phase)}</td>
              <td>${c.distro} <span class="hint">${c.version}</span></td>
              <td>${c.cni}${c.hubble ? " + Hubble" : ""}</td>
              <td class="num">${c.status.readyNodes} / ${c.status.nodes}</td>
              <td><code>${c.endpoint}</code></td>
            </tr>`)}</tbody></table></div>
          <div class="actions spaced-top">
            <button data-kubeconfig="plain">選んだクラスタの kubeconfig</button>
            <button data-kubeconfig="ssh">選んだクラスタの kubeconfig (SSH 経由用)</button>
            <span class="hint" id="sel-count"></span>
          </div>
          <p class="hint">選んだクラスタを 1 つの kubeconfig にまとめます (クラスタごとに context が入る。切り替えは
            <code>kubectl config use-context &lt;名前&gt;</code>)。SSH 経由用は PC で <code>ssh -N -D 1080 &lt;ユーザー&gt;@&lt;ミニPC&gt;</code>
            をつないだまま使います。</p>`}
    </div>`;

  // 消えたクラスタは選択から外す
  for (const n of [...selectedClusters]) if (!clusters.some((c) => c.name === n)) selectedClusters.delete(n);
  const syncSelection = () => {
    const n = selectedClusters.size;
    app.querySelectorAll("[data-kubeconfig]").forEach((b) => { b.disabled = n === 0; });
    const count = document.getElementById("sel-count");
    if (count) count.textContent = n ? `${n} 個のクラスタを選択中` : "クラスタを選んでください";
    const all = document.getElementById("sel-all");
    if (all) all.checked = clusters.length > 0 && n === clusters.length;
  };
  app.querySelectorAll("[data-sel]").forEach((cb) => cb.addEventListener("change", () => {
    if (cb.checked) selectedClusters.add(cb.dataset.sel); else selectedClusters.delete(cb.dataset.sel);
    syncSelection();
  }));
  const all = document.getElementById("sel-all");
  if (all) all.addEventListener("change", () => {
    clusters.forEach((c) => { if (all.checked) selectedClusters.add(c.name); else selectedClusters.delete(c.name); });
    app.querySelectorAll("[data-sel]").forEach((cb) => { cb.checked = all.checked; });
    syncSelection();
  });
  app.querySelectorAll("[data-kubeconfig]").forEach((b) => b.addEventListener("click", () => {
    const params = new URLSearchParams();
    // 一覧の順に並べる (current-context は先頭のクラスタ)
    clusters.filter((c) => selectedClusters.has(c.name)).forEach((c) => params.append("cluster", c.name));
    if (b.dataset.kubeconfig === "ssh") params.set("proxy", SSH_PROXY);
    location.href = `/api/kubeconfig?${params}`;
  }));
  syncSelection();
}

// ---------- 画面: クラスタを作る ----------

const SIZES = {
  small: { label: "small", cpus: 2, memoryMiB: 2048, diskGiB: 30 },
  medium: { label: "medium", cpus: 2, memoryMiB: 4096, diskGiB: 30 },
  large: { label: "large", cpus: 4, memoryMiB: 8192, diskGiB: 30 },
};

function presetPicker(name, selected) {
  return html`<div class="presets" data-preset="${name}">${Object.entries(SIZES).map(([k, s]) => html`
    <button type="button" class="preset ${k === selected ? "selected" : ""}" data-size="${k}">
      <strong>${s.label}</strong><span class="hint">${s.cpus} vCPU · ${gib(s.memoryMiB)} GiB · ${s.diskGiB} GiB</span>
    </button>`)}</div>`;
}

async function viewNewCluster() {
  setNav("new");
  const [distros, defaults] = await Promise.all([api("GET", "/api/distros"), api("GET", "/api/cluster-defaults")]);
  const def = distros.find((d) => d.default) || distros[0];
  app.innerHTML = html`
    <h1>クラスタを作る</h1>
    <form class="card" id="new-cluster" autocomplete="off">
      <div class="row"><label for="f-name">名前</label>
        <div><input id="f-name" name="name" required pattern="[a-z0-9]([a-z0-9\\-]{0,18}[a-z0-9])?" placeholder="demo" maxlength="20">
        <div class="hint">英小文字・数字・ハイフン、20 文字まで。ノードは「名前-cp-1」「名前-worker-1」…になる</div></div></div>
      <div class="row"><label for="f-profile">用途</label>
        <div><select id="f-profile" name="profile">
          <option value="">普通のクラスタ</option>
          <option value="cnd-handson">CloudNative Days のハンズオン (cnd-handson)</option></select>
        <div class="hint" id="f-profile-note"></div></div></div>
      <div class="row"><label for="f-distro">ディストリ</label>
        <div class="inline"><select id="f-distro" name="distro">${distros.map((d) => html`<option value="${d.name}" ${d.name === def.name ? raw("selected") : ""}>${d.name}</option>`)}</select>
        <input id="f-version" name="version" placeholder="${def.defaultVersion}"><span class="hint">空なら既定のバージョン</span></div></div>
      <div class="row"><label for="f-workers">ワーカー</label>
        <div class="inline"><input id="f-workers" name="workers" type="number" min="0" max="50" value="2"><span class="hint">台 (コントロールプレーンは 1 台)</span></div></div>
      <div class="row"><label>コントロールプレーン</label>${presetPicker("controlPlane", "medium")}</div>
      <div class="row"><label>ワーカー 1 台</label>${presetPicker("worker", "medium")}</div>
      <div class="row"><label for="f-cni">CNI</label>
        <div class="inline"><select id="f-cni" name="cni">
          <option value="cilium" ${defaults.cni === "cilium" ? raw("selected") : ""}>Cilium (kube-proxy を置き換え)</option>
          <option value="flannel" ${defaults.cni === "flannel" ? raw("selected") : ""}>flannel</option></select>
          <label class="inline"><input type="checkbox" id="f-hubble" name="hubble" checked> Hubble</label></div></div>
      <div class="row"><label for="f-runtime">コンテナランタイム</label>
        <div class="inline"><select id="f-runtime" name="runtimeDefault">
          ${["runc", "crun", "youki"].map((r) => html`<option value="${r}" ${r === defaults.runtime.default ? raw("selected") : ""}>${r}</option>`)}</select>
          <span class="hint">追加で入れる:</span>
          ${["crun", "youki"].map((r) => html`<label class="inline"><input type="checkbox" name="install" value="${r}" ${(defaults.runtime.install || []).includes(r) ? raw("checked") : ""}> ${r}</label>`)}
        </div></div>
      <div class="row"><span></span><div class="hint" id="f-note"></div></div>
      <div class="row"><span></span><div class="actions"><button class="primary" type="submit" id="f-submit">作成する</button><a class="button" href="#/">キャンセル</a></div></div>
    </form>`;

  const form = document.getElementById("new-cluster");
  const sizes = { controlPlane: "medium", worker: "medium" };
  form.querySelectorAll(".presets").forEach((p) => p.addEventListener("click", (e) => {
    const b = e.target.closest(".preset");
    if (!b) return;
    sizes[p.dataset.preset] = b.dataset.size;
    p.querySelectorAll(".preset").forEach((x) => x.classList.toggle("selected", x === b));
  }));

  const sync = () => {
    // ハンズオン用は kind と同じ kubeadm、ハンズオンの設定の Cilium + Hubble に決まっている
    const handson = form.profile.value === "cnd-handson";
    if (handson) {
      form.distro.value = "kubeadm";
      form.cni.value = "cilium";
      form.hubble.checked = true;
    }
    form.distro.disabled = handson;
    form.cni.disabled = handson;
    document.getElementById("f-profile-note").innerHTML = handson
      ? html`<a href="https://github.com/cloudnativedaysjp/cnd-handson" target="_blank" rel="noopener">cnd-handson</a> の
        chapter_cluster-create の環境 (Kubernetes v1.36、Cilium、Envoy Gateway、Gateway handson-gateway) を作り、
        ホストの 80・8080・8443・18080・18443 番をクラスタにつなぎます (ハンズオン用のクラスタは同時に 1 つまで)。
        Prometheus や KubeVirt まで進めるなら、ワーカーは large がおすすめです。`
      : "";
    const d = distros.find((x) => x.name === form.distro.value);
    form.version.placeholder = handson ? "v1.36" : d.defaultVersion;
    const talos = d.name === "talos";
    const cilium = form.cni.value === "cilium";
    form.hubble.disabled = !cilium || handson;
    if (!cilium) form.hubble.checked = false;
    form.runtimeDefault.disabled = talos;
    form.querySelectorAll("input[name=install]").forEach((i) => { i.disabled = talos; if (talos) i.checked = false; });
    if (talos) form.runtimeDefault.value = "runc";
    document.getElementById("f-note").textContent = talos
      ? "Talos はシェルも SSH も無く、talosctl で操作します。コンテナランタイムは runc のみです。"
      : "";
  };
  form.distro.addEventListener("change", sync);
  form.cni.addEventListener("change", sync);
  form.profile.addEventListener("change", sync);
  sync();

  form.addEventListener("submit", async (e) => {
    e.preventDefault();
    const btn = document.getElementById("f-submit");
    btn.disabled = true;
    btn.textContent = "作成中…";
    const install = [...form.querySelectorAll("input[name=install]:checked")].map((i) => i.value);
    const size = (k) => { const s = SIZES[sizes[k]]; return { cpus: s.cpus, memoryMiB: s.memoryMiB, diskGiB: s.diskGiB }; };
    const body = {
      name: form.name.value.trim(),
      distro: form.distro.value,
      workers: Number(form.workers.value),
      controlPlane: size("controlPlane"),
      worker: size("worker"),
      cni: form.cni.value,
      hubble: form.hubble.checked,
      runtime: { default: form.runtimeDefault.value, install },
    };
    if (form.version.value.trim()) body.version = form.version.value.trim();
    if (form.profile.value) body.profile = form.profile.value;
    try {
      const c = await api("POST", "/api/clusters", body);
      toast(`クラスタ ${c.name} を作りました。構築は数分かかります`);
      location.hash = `#/clusters/${encodeURIComponent(c.name)}`;
    } catch (err) {
      toast(err.message, true);
      btn.disabled = false;
      btn.textContent = "作成する";
    }
  });
}

// ---------- 画面: クラスタの詳細 ----------

const serialState = { machine: "", auto: true };

async function loadSerial(name) {
  const el = document.getElementById("serial");
  if (!el || !name) return;
  try {
    const res = await fetch(`/api/vms/${encodeURIComponent(name)}/serial?bytes=65536`);
    let text = await res.text();
    if (!res.ok) {
      try { text = JSON.parse(text).error; } catch { /* そのまま */ }
      text = `(シリアルログを読めません: ${text})`;
    }
    const atBottom = el.scrollTop + el.clientHeight >= el.scrollHeight - 20;
    el.textContent = text.replace(/\x1b\[[0-9;?]*[A-Za-z]/g, "") || "(まだ出力がありません)";
    if (atBottom) el.scrollTop = el.scrollHeight;
  } catch (err) {
    el.textContent = err.message;
  }
}

// ハンズオン (cnd-handson) のクラスタで、PC の hosts と、資料の読み替えを案内する
const HANDSON_HOSTS = ["hello-world", "rollout", "blue-green", "app", "prometheus", "grafana", "jaeger", "argocd",
  "app-argocd", "app-cicd", "dev-kustomize-argocd", "prd-kustomize-argocd", "helm-argocd", "app-preview-argocd",
  "headlamp", "argocdupdate", "app-rollouts", "app-preview-rollouts", "kiali", "kiali-ambient", "app-cilium",
  "hubble-cilium", "pyroscope", "troubleshoot", "cnd-web"];

function handsonCard(c, host) {
  const ip = (host && host.lanAddress) || "<ミニPC の IP>";
  const hosts = HANDSON_HOSTS.map((h) => `${ip}    ${h}.example.com`).join("\n");
  return html`<div class="card">
    <h2>CloudNative Days のハンズオン</h2>
    <div class="actions spaced-bottom"><a class="button primary" href="#/clusters/${encodeURIComponent(c.name)}/handson">資料とターミナルを並べて開く</a></div>
    <p><a href="https://github.com/cloudnativedaysjp/cnd-handson" target="_blank" rel="noopener">cnd-handson</a> の
      chapter_cluster-create (kind・Cilium・Envoy Gateway・Gateway) までは済んでいます。
      <code>helmfile sync -f helm/helmfile.yaml</code> は実行せず、「アプリケーションのデプロイ」から進めてください。
      ほかの読み替えは <a href="https://github.com/sat0ken/niwakube/blob/main/docs/cnd-handson.md" target="_blank" rel="noopener">docs/cnd-handson.md</a> にあります。</p>
    <ul class="hint">
      <li>作業は「ターミナル」で。ホームは閉じても残り、<code>git clone https://github.com/cloudnativedaysjp/cnd-handson.git</code> から始めます。
        helmfile・cilium・hubble も入っています。sudo は使えないので、資料の <code>sudo mv ... /usr/local/bin</code> は <code>~/bin</code> に置きます</li>
      <li>ターミナルでは <code>*.example.com</code> がミニPC に向いているので、資料の <code>curl</code> はそのまま動きます</li>
      <li><code>docker exec kind-worker</code> は <code>ssh worker</code>、kind のノード名 (kind-control-plane など) は ${c.name}-cp-1 などに読み替えます</li>
    </ul>
    <p>ブラウザで開く PC の hosts (Windows は <code>C:\\Windows\\System32\\drivers\\etc\\hosts</code>、Mac / Linux は <code>/etc/hosts</code>) に足すもの:</p>
    <pre class="log">${hosts}</pre>
  </div>`;
}

async function viewCluster(name) {
  setNav("");
  const [c, forwards, snapshots, host] = await Promise.all([
    api("GET", `/api/clusters/${encodeURIComponent(name)}`),
    api("GET", `/api/clusters/${encodeURIComponent(name)}/forwards`),
    api("GET", `/api/clusters/${encodeURIComponent(name)}/snapshots`).catch(() => null), // 使えなければカードを出さない
    api("GET", "/api/host").catch(() => null),
  ]);
  if (!serialState.machine || !c.machines.some((m) => m.name === serialState.machine)) {
    serialState.machine = c.machines[0] ? c.machines[0].name : "";
  }
  const enc = encodeURIComponent(c.name);
  const clusterBusy = !!c.operation && !c.operation.startsWith("failed:");
  app.innerHTML = html`
    <h1>${c.name} ${phaseBadge(c.status.phase)}
      <span class="hint">${c.status.readyNodes} / ${c.status.nodes} ノード Ready${c.status.message ? ` · ${c.status.message}` : ""}</span>
      ${c.operation ? html`<span class="badge ${c.operation.startsWith("failed:") ? "danger" : "info"}">${operationLabel(c.operation)}</span>` : ""}</h1>
    <div class="card">
      <div class="actions">
        <a class="button primary" href="#/clusters/${enc}/terminal">ターミナル</a>
        <a class="button" href="/api/clusters/${enc}/kubeconfig" download>kubeconfig をダウンロード</a>
        <a class="button" href="/api/clusters/${enc}/kubeconfig?proxy=${encodeURIComponent(SSH_PROXY)}" download
          title="ミニPC の外 (SSH でつないでいる PC) から使う kubeconfig。proxy-url: ${SSH_PROXY} 入り">SSH 経由用の kubeconfig</a>
        ${c.distro === "talos" ? html`<a class="button" href="/api/clusters/${enc}/talosconfig" download>talosconfig をダウンロード</a>
          <a class="button" href="/api/clusters/${enc}/talosconfig?proxy=${encodeURIComponent(SSH_PROXY)}" download
            title="ミニPC の外 (SSH でつないでいる PC) から使う talosconfig。proxy-url: ${SSH_PROXY} 入り">SSH 経由用の talosconfig</a>` : ""}
        <button data-act="start" ${c.desiredState === "running" ? raw("disabled") : ""}>起動</button>
        <button data-act="stop" ${c.desiredState === "stopped" ? raw("disabled") : ""}>停止</button>
        <button class="danger" data-act="delete">削除</button>
      </div>
      <p class="hint spaced-top">「SSH 経由用の kubeconfig${c.distro === "talos" ? " / talosconfig" : ""}」は、PC で <code>ssh -N -D 1080 &lt;ユーザー&gt;@&lt;ミニPC&gt;</code> をつないだまま、
        PC の kubectl${c.distro === "talos" ? " / talosctl" : ""} で使います (クラスタがいくつあっても SSH は 1 本でよい)。ミニPC の上では普通の kubeconfig を使います。</p>
    </div>
    ${c.profile === "cnd-handson" ? handsonCard(c, host) : ""}
    <div class="card">
      <h2>概要</h2>
      <dl class="kv">
        <dt>ディストリ</dt><dd>${c.distro} ${c.version}</dd>
        <dt>CNI</dt><dd>${c.cni}${c.hubble ? " + Hubble" : ""}</dd>
        <dt>コンテナランタイム</dt><dd>${c.runtime.default}${c.runtime.install && c.runtime.install.length ? ` (RuntimeClass: runc, ${c.runtime.install.join(", ")})` : ""}</dd>
        <dt>API</dt><dd><code>${c.endpoint}</code></dd>
        <dt>サブネット</dt><dd><code>${c.subnet}</code></dd>
        ${c.loadBalancerPool ? html`<dt>LoadBalancer の IP</dt><dd><code>${c.loadBalancerPool}</code></dd>` : ""}
        ${c.storageClasses ? html`<dt>StorageClass</dt><dd>${c.storageClasses.map((s, i) => html`<span class="tag">${s}${i === 0 ? " (既定)" : ""}</span>`)}</dd>` : ""}
        ${c.addons ? html`<dt>アドオン</dt><dd>${c.addons === "applied" ? "適用済み" : c.addons === "pending" ? "クラスタの準備ができたら適用します" : c.addons}</dd>` : ""}
        <dt>作成日時</dt><dd>${new Date(c.createdAt).toLocaleString()}</dd>
      </dl>
    </div>
    <div class="card">
      <h2>ノード</h2>
      <div class="table-wrap"><table>
        <thead><tr><th>名前</th><th>役割</th><th>状態</th><th>IP</th><th class="num">vCPU</th><th class="num">メモリ</th><th class="num">ディスク</th></tr></thead>
        <tbody>${c.machines.map((m) => html`<tr>
          <td><a href="#/vms/${encodeURIComponent(m.name)}">${m.name}</a></td><td>${m.role}</td><td>${stateBadge(m.status.state)}</td>
          <td><code>${m.ip}</code></td><td class="num">${m.cpus}</td><td class="num">${gib(m.memoryMiB)} GiB</td><td class="num">${m.diskGiB} GiB</td></tr>`)}</tbody>
      </table></div>
    </div>
    <div class="card">
      <h2>ワーカーの台数を変える</h2>
      <p class="hint">増やすと、新しいワーカーを作ってクラスタに参加させます (サイズは今のワーカーのサイズ)。
        減らすと、番号の大きいワーカーから 1 台ずつ drain (Pod をほかのノードへ移す) してから Node とマシンを消します。
        消したワーカーのディスクのデータ (hostPath や local-path の PV) は残りません。</p>
      <form id="scale-form" class="inline" autocomplete="off">
        <label class="inline">ワーカー <input name="workers" type="number" min="0" max="50" value="${c.machines.filter((m) => m.role === "worker").length}" required> 台</label>
        <button class="primary" type="submit" ${(c.operation && !c.operation.startsWith("failed:")) || c.desiredState !== "running" ? raw("disabled") : ""}>適用</button>
        ${c.desiredState !== "running" ? html`<span class="hint">クラスタを起動してから変えられます</span>` : ""}
      </form>
    </div>
    <div class="card">
      <h2>ノードのサイズを変える</h2>
      <p class="hint">選んだ役割のノードを 1 台ずつ、drain (Pod をほかのノードへ移す) → 止めてサイズを変えて起動 → Ready に戻ったら次へ、の順に変えます。
        ワーカーが 2 台以上ならサービスは止まりません。コントロールプレーンは 1 台なので、再起動中の 1〜2 分は API が使えません。
        ディスクは広げるだけで、パーティションは起動時に広がります。これから足すノードも新しいサイズになります。</p>
      <form id="resize-form" class="inline" autocomplete="off">
        <select name="role">
          <option value="worker">ワーカー (${c.machines.filter((m) => m.role === "worker").length} 台)</option>
          <option value="control-plane">コントロールプレーン (${c.machines.filter((m) => m.role === "control-plane").length} 台)</option>
        </select>
        <label class="inline">vCPU <input name="cpus" type="number" min="1" max="32" required></label>
        <label class="inline">メモリ GiB <input name="mem" type="number" min="0.5" max="64" step="0.5" required></label>
        <label class="inline">ディスク GiB <input name="disk" type="number" max="1024" required></label>
        <button class="primary" type="submit" ${c.operation && !c.operation.startsWith("failed:") ? raw("disabled") : ""}>適用</button>
      </form>
      <div class="actions spaced-top">${Object.entries(SIZES).map(([k, s]) => html`<button type="button" data-size-preset="${k}">${s.label} (${s.cpus} vCPU · ${gib(s.memoryMiB)} GiB)</button>`)}</div>
    </div>
    ${snapshots ? html`<div class="card">
      <h2>スナップショット</h2>
      <p class="hint">クラスタをいったん止めて、全ノードのディスク (etcd を含む) と NFS の PV のデータを丸ごと保存し、また起動します。
        戻すと、ノードの台数・サイズ・中身がその時点に戻ります (あとで足したノードは消え、消したノードは戻ります)。
        止めている間 (1〜2 分) はクラスタを使えません。XFS なら容量はほとんど使いません。</p>
      ${snapshots.length ? html`<div class="table-wrap"><table>
        <thead><tr><th>名前</th><th>日時</th><th>ノード</th><th></th></tr></thead>
        <tbody>${snapshots.map((sn) => html`<tr><td><strong>${sn.name}</strong></td><td>${new Date(sn.createdAt).toLocaleString()}</td>
          <td>${sn.nodes.length} 台${sn.nfs ? " + NFS" : ""}</td>
          <td class="actions"><button data-restore-snap="${sn.name}" ${clusterBusy ? raw("disabled") : ""}>戻す</button>
            <button class="danger" data-del-snap="${sn.name}" ${clusterBusy ? raw("disabled") : ""}>削除</button></td></tr>`)}</tbody>
      </table></div>` : html`<p class="hint">まだありません。</p>`}
      <form id="snap-form" class="inline spaced-top" autocomplete="off">
        <input name="name" placeholder="before-upgrade (省略すると日時)" pattern="[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?" title="英小文字・数字・-">
        <button class="primary" type="submit" ${clusterBusy ? raw("disabled") : ""}>スナップショットを取る</button>
      </form>
    </div>` : ""}
    <div class="card">
      <h2>ポートの転送 (LAN からの入口)</h2>
      <p class="hint">ミニPC のポートをクラスタの IP (LoadBalancer の IP など) に転送します。LAN の PC から <code>&lt;ミニPC の IP&gt;:ポート</code> で届きます。</p>
      ${forwards.length ? html`<div class="table-wrap"><table>
        <thead><tr><th>プロトコル</th><th class="num">ホストのポート</th><th>転送先</th><th></th></tr></thead>
        <tbody>${forwards.map((f) => html`<tr><td>${f.proto}</td><td class="num">${f.hostPort}</td><td><code>${f.ip}:${f.port}</code></td>
          <td><button class="danger" data-del-forward="${f.proto}/${f.hostPort}">削除</button></td></tr>`)}</tbody></table></div>` : ""}
      <form id="forward-form" class="inline spaced-top">
        <select name="proto"><option>tcp</option><option>udp</option></select>
        <label class="inline">ホストのポート <input name="hostPort" type="number" min="1" max="65535" placeholder="8080" required></label>
        <span>→</span>
        <input name="ip" placeholder="${c.loadBalancerPool ? c.loadBalancerPool.split("-")[0] : "172.30.x.y"}" required>
        <span>:</span>
        <input name="port" type="number" min="1" max="65535" placeholder="80" required>
        <button type="submit">追加</button>
      </form>
    </div>
    <div class="card">
      <h2>シリアルコンソールのログ</h2>
      <div class="inline spaced-bottom">
        <select id="serial-machine">${c.machines.map((m) => html`<option ${m.name === serialState.machine ? raw("selected") : ""}>${m.name}</option>`)}</select>
        <label class="inline"><input type="checkbox" id="serial-auto" ${serialState.auto ? raw("checked") : ""}> 自動更新</label>
      </div>
      <pre class="log" id="serial">読み込み中…</pre>
    </div>`;

  app.querySelectorAll("[data-act]").forEach((b) => b.addEventListener("click", async () => {
    const act = b.dataset.act;
    if (act === "delete" && !confirm(`クラスタ ${c.name} を削除します。ノードと PV (NFS を含む) のデータも消えます。よろしいですか?`)) return;
    b.disabled = true;
    try {
      if (act === "delete") {
        await api("DELETE", `/api/clusters/${enc}`);
        toast(`クラスタ ${c.name} を削除しました`);
        location.hash = "#/";
        return;
      }
      await api("POST", `/api/clusters/${enc}/${act}`);
      toast(act === "start" ? "起動しました" : "停止しました");
      render();
    } catch (err) { toast(err.message, true); b.disabled = false; }
  }));
  app.querySelectorAll("[data-del-forward]").forEach((b) => b.addEventListener("click", async () => {
    try { await api("DELETE", `/api/clusters/${enc}/forwards/${b.dataset.delForward}`); toast("転送を削除しました"); render(); } catch (err) { toast(err.message, true); }
  }));
  clusterOpActive = !!c.operation && !c.operation.startsWith("failed:");
  const rf = document.getElementById("resize-form");
  const fillResize = () => {
    const sz = rf.role.value === "worker" ? c.worker : c.controlPlane;
    rf.cpus.value = sz.cpus;
    rf.mem.value = sz.memoryMiB / 1024;
    rf.disk.value = sz.diskGiB;
    rf.disk.min = sz.diskGiB; // ディスクは広げるだけ
  };
  fillResize();
  rf.role.addEventListener("change", fillResize);
  app.querySelectorAll("[data-size-preset]").forEach((b) => b.addEventListener("click", () => {
    const s = SIZES[b.dataset.sizePreset];
    rf.cpus.value = s.cpus;
    rf.mem.value = s.memoryMiB / 1024;
  }));
  rf.addEventListener("submit", async (e) => {
    e.preventDefault();
    const role = rf.role.value;
    const n = c.machines.filter((m) => m.role === role).length;
    const size = { cpus: Number(rf.cpus.value), memoryMiB: Math.round(Number(rf.mem.value) * 1024), diskGiB: Number(rf.disk.value) };
    const label = role === "worker" ? "ワーカー" : "コントロールプレーン";
    if (!confirm(`${label} ${n} 台を 1 台ずつ再起動して、${size.cpus} vCPU · ${gib(size.memoryMiB)} GiB · ディスク ${size.diskGiB} GiB にします。よろしいですか?`)) return;
    try {
      await api("POST", `/api/clusters/${enc}/resize`, { role, size });
      toast(`${label}のサイズの変更を始めました。1 台ずつ再起動します`);
      document.activeElement.blur();
      render();
    } catch (err) { toast(err.message, true); }
  });
  const snapForm = document.getElementById("snap-form");
  if (snapForm) snapForm.addEventListener("submit", async (e) => {
    e.preventDefault();
    const n = snapForm.name.value.trim();
    if (!confirm(`クラスタ ${c.name} をいったん止めてスナップショット${n ? ` ${n}` : ""} を取ります。止めている間はクラスタを使えません。よろしいですか?`)) return;
    try {
      await api("POST", `/api/clusters/${enc}/snapshots`, n ? { name: n } : {});
      toast("スナップショットを取り始めました");
      document.activeElement.blur();
      render();
    } catch (err) { toast(err.message, true); }
  });
  app.querySelectorAll("[data-restore-snap]").forEach((b) => b.addEventListener("click", async () => {
    const n = b.dataset.restoreSnap;
    if (!confirm(`クラスタ ${c.name} をスナップショット ${n} の状態に戻します。それ以降の変更 (Kubernetes の中身、ノードの台数・サイズ、NFS のデータ) はすべて消えます。よろしいですか?`)) return;
    try { await api("POST", `/api/clusters/${enc}/snapshots/${encodeURIComponent(n)}/restore`); toast(`${n} に戻し始めました`); render(); } catch (err) { toast(err.message, true); }
  }));
  app.querySelectorAll("[data-del-snap]").forEach((b) => b.addEventListener("click", async () => {
    const n = b.dataset.delSnap;
    if (!confirm(`スナップショット ${n} を削除します。よろしいですか?`)) return;
    try { await api("DELETE", `/api/clusters/${enc}/snapshots/${encodeURIComponent(n)}`); toast("削除しました"); render(); } catch (err) { toast(err.message, true); }
  }));
  const sf = document.getElementById("scale-form");
  sf.addEventListener("submit", async (e) => {
    e.preventDefault();
    const have = c.machines.filter((m) => m.role === "worker").length;
    const workers = Number(sf.workers.value);
    if (workers === have) return;
    if (workers < have && !confirm(`ワーカーを ${have} 台から ${workers} 台に減らします。番号の大きい ${have - workers} 台を drain してから削除します。よろしいですか?`)) return;
    try {
      await api("POST", `/api/clusters/${enc}/scale`, { workers });
      toast(workers > have ? `ワーカーを ${workers - have} 台追加しています` : `ワーカーを ${have - workers} 台減らしています`);
      document.activeElement.blur();
      render();
    } catch (err) { toast(err.message, true); }
  });
  document.getElementById("forward-form").addEventListener("submit", async (e) => {
    e.preventDefault();
    const f = e.target;
    try {
      await api("POST", `/api/clusters/${enc}/forwards`, { proto: f.proto.value, hostPort: Number(f.hostPort.value), ip: f.ip.value.trim(), port: Number(f.port.value) });
      toast("転送を追加しました");
      f.reset();
      render();
    } catch (err) { toast(err.message, true); }
  });
  document.getElementById("serial-machine").addEventListener("change", (e) => { serialState.machine = e.target.value; loadSerial(serialState.machine); });
  document.getElementById("serial-auto").addEventListener("change", (e) => { serialState.auto = e.target.checked; });
  loadSerial(serialState.machine);
}

// ---------- 画面: VM ----------

async function viewVMs() {
  setNav("vms");
  const vms = await api("GET", "/api/vms");
  const standalone = vms.filter((v) => !v.cluster);
  const inCluster = vms.filter((v) => v.cluster);
  const rows = (list, actions) => list.map((v) => html`<tr>
    <td><a href="#/vms/${encodeURIComponent(v.name)}">${v.name}</a></td><td>${stateBadge(v.status.state)}</td>
    ${v.cluster ? html`<td><a href="#/clusters/${encodeURIComponent(v.cluster)}">${v.cluster}</a> <span class="hint">${v.role}</span></td>` : html`<td>${v.image}</td>`}
    <td><code>${v.ip}</code></td><td class="num">${v.cpus}</td><td class="num">${gib(v.memoryMiB)} GiB</td><td class="num">${v.diskGiB} GiB</td>
    ${actions ? html`<td><div class="actions"><button data-vm-act="start" data-vm="${v.name}">起動</button><button data-vm-act="stop" data-vm="${v.name}">停止</button><button class="danger" data-vm-act="delete" data-vm="${v.name}">削除</button></div></td>` : ""}
  </tr>`);
  app.innerHTML = html`
    <h1>VM</h1>
    <div class="card">
      <h2>単体の VM</h2>
      ${standalone.length ? html`<div class="table-wrap"><table>
        <thead><tr><th>名前</th><th>状態</th><th>イメージ</th><th>IP</th><th class="num">vCPU</th><th class="num">メモリ</th><th class="num">ディスク</th><th></th></tr></thead>
        <tbody>${rows(standalone, true)}</tbody></table></div>` : html`<div class="empty">単体の VM はありません</div>`}
      <form id="vm-form" class="inline spaced-top" autocomplete="off">
        <input name="name" placeholder="名前" required pattern="[a-z0-9]([a-z0-9\\-]{0,30}[a-z0-9])?" maxlength="32">
        <label class="inline">vCPU <input name="cpus" type="number" min="1" max="32" value="2"></label>
        <label class="inline">メモリ GiB <input name="mem" type="number" min="1" max="64" value="4"></label>
        <label class="inline">ディスク GiB <input name="disk" type="number" min="5" max="1024" value="30"></label>
        <button class="primary" type="submit">VM を作る</button>
      </form>
      <p class="hint">Ubuntu 26.04 の VM を作ります。ミニPC から <code>ssh -i /var/lib/niwakube/ssh/id_ed25519 ubuntu@&lt;IP&gt;</code> でログインできます。</p>
    </div>
    <div class="card">
      <h2>クラスタのノード</h2>
      ${inCluster.length ? html`<div class="table-wrap"><table>
        <thead><tr><th>名前</th><th>状態</th><th>クラスタ</th><th>IP</th><th class="num">vCPU</th><th class="num">メモリ</th><th class="num">ディスク</th></tr></thead>
        <tbody>${rows(inCluster, false)}</tbody></table></div>` : html`<div class="empty">ありません</div>`}
    </div>`;

  app.querySelectorAll("[data-vm-act]").forEach((b) => b.addEventListener("click", async () => {
    const name = b.dataset.vm, act = b.dataset.vmAct;
    if (act === "delete" && !confirm(`VM ${name} を削除します。よろしいですか?`)) return;
    b.disabled = true;
    try {
      if (act === "delete") await api("DELETE", `/api/vms/${encodeURIComponent(name)}`);
      else await api("POST", `/api/vms/${encodeURIComponent(name)}/${act}`);
      toast(`${name}: ${{ start: "起動しました", stop: "停止しました", delete: "削除しました" }[act]}`);
      render();
    } catch (err) { toast(err.message, true); b.disabled = false; }
  }));
  document.getElementById("vm-form").addEventListener("submit", async (e) => {
    e.preventDefault();
    const f = e.target;
    const btn = f.querySelector("button[type=submit]");
    btn.disabled = true;
    try {
      const vm = await api("POST", "/api/vms", { name: f.name.value.trim(), cpus: Number(f.cpus.value), memoryMiB: Number(f.mem.value) * 1024, diskGiB: Number(f.disk.value) });
      toast(`VM ${vm.name} を作りました (${vm.ip})`);
      f.reset();
      render();
    } catch (err) { toast(err.message, true); } finally { btn.disabled = false; }
  });
}

async function viewVM(name) {
  setNav("vms");
  const v = await api("GET", `/api/vms/${encodeURIComponent(name)}`);
  serialState.machine = v.name;
  app.innerHTML = html`
    <h1>${v.name} ${stateBadge(v.status.state)}</h1>
    <div class="card">
      <dl class="kv">
        ${v.cluster ? html`<dt>クラスタ</dt><dd><a href="#/clusters/${encodeURIComponent(v.cluster)}">${v.cluster}</a> (${v.role})</dd>` : ""}
        <dt>IP / MAC</dt><dd><code>${v.ip}</code> <code>${v.mac}</code></dd>
        <dt>リソース</dt><dd>${v.cpus} vCPU · ${gib(v.memoryMiB)} GiB · ${v.diskGiB} GiB</dd>
        <dt>イメージ</dt><dd>${v.image}</dd>
        <dt>バックエンド</dt><dd>${v.backend}</dd>
        ${v.status.detail ? html`<dt>詳細</dt><dd>${v.status.detail}</dd>` : ""}
      </dl>
    </div>
    <div class="card">
      <h2>サイズを変える</h2>
      ${v.cluster
        ? html`<p class="hint">クラスタのノードは、<a href="#/clusters/${encodeURIComponent(v.cluster)}">クラスタの画面</a>の「ノードのサイズを変える」から変えます
            (drain してから 1 台ずつ再起動します)。</p>`
        : html`<p class="hint">${v.status.state === "running" ? "VM を止めてサイズを変え、起動し直します。" : "止まっている VM のサイズを変えます。"}
            ディスクは広げるだけで、パーティションは起動時に広がります。</p>
          <form id="vm-resize-form" class="inline" autocomplete="off">
            <label class="inline">vCPU <input name="cpus" type="number" min="1" max="32" value="${v.cpus}" required></label>
            <label class="inline">メモリ GiB <input name="mem" type="number" min="0.5" max="64" step="0.5" value="${v.memoryMiB / 1024}" required></label>
            <label class="inline">ディスク GiB <input name="disk" type="number" min="${v.diskGiB}" max="1024" value="${v.diskGiB}" required></label>
            <button class="primary" type="submit">適用</button>
          </form>`}
    </div>
    <div class="card">
      <h2>シリアルコンソールのログ</h2>
      <label class="inline spaced-bottom"><input type="checkbox" id="serial-auto" ${serialState.auto ? raw("checked") : ""}> 自動更新</label>
      <pre class="log" id="serial">読み込み中…</pre>
    </div>`;
  document.getElementById("serial-auto").addEventListener("change", (e) => { serialState.auto = e.target.checked; });
  const vf = document.getElementById("vm-resize-form");
  if (vf) vf.addEventListener("submit", async (e) => {
    e.preventDefault();
    const req = { cpus: Number(vf.cpus.value), memoryMiB: Math.round(Number(vf.mem.value) * 1024), diskGiB: Number(vf.disk.value) };
    if (v.status.state === "running" && !confirm(`${v.name} を再起動して、${req.cpus} vCPU · ${gib(req.memoryMiB)} GiB · ディスク ${req.diskGiB} GiB にします。よろしいですか?`)) return;
    const btn = vf.querySelector("button[type=submit]");
    btn.disabled = true;
    btn.textContent = "変更中…";
    try {
      await api("POST", `/api/vms/${encodeURIComponent(v.name)}/resize`, req);
      toast(`${v.name} のサイズを変えました`);
      document.activeElement.blur();
      render();
    } catch (err) {
      toast(err.message, true);
      btn.disabled = false;
      btn.textContent = "適用";
    }
  });
  loadSerial(v.name);
}

// ---------- 画面: ターミナル ----------

function loadScript(src) {
  return new Promise((resolve, reject) => {
    if (document.querySelector(`script[src="${src}"]`)) return resolve();
    const s = document.createElement("script");
    s.src = src;
    s.onload = resolve;
    s.onerror = () => reject(new Error(`${src} を読み込めません`));
    document.head.appendChild(s);
  });
}

function loadCSS(href) {
  if (document.querySelector(`link[href="${href}"]`)) return;
  const l = document.createElement("link");
  l.rel = "stylesheet";
  l.href = href;
  document.head.appendChild(l);
}

// loadXterm は xterm.js を読み込む (ターミナルを使う画面で 1 度だけ)。
async function loadXterm() {
  loadCSS("/ui/vendor/xterm/xterm.css");
  await loadScript("/ui/vendor/xterm/xterm.js");
  await loadScript("/ui/vendor/xterm/addon-fit.js");
}

// mountTerminal は el にクラスタ name のシェル (ミニPC の一時ユーザーのサンドボックス) をつなぐ。
// statusEl に接続の状態を出し、reconnectBtn を切断時に表示する。戻り値は後片付けの関数。
function mountTerminal(el, name, statusEl, reconnectBtn) {
  const term = new Terminal({
    cursorBlink: true,
    fontSize: 14,
    fontFamily: 'ui-monospace, SFMono-Regular, Menlo, Consolas, "Noto Sans Mono CJK JP", monospace',
    scrollback: 5000,
    theme: { background: "#111613", foreground: "#e3ebe6" },
  });
  const fit = new FitAddon.FitAddon();
  term.loadAddon(fit);
  term.open(el);
  fit.fit();
  term.focus();

  const encoder = new TextEncoder();
  let ws = null;
  const status = (text, cls) => { statusEl.textContent = text; statusEl.className = `badge ${cls}`; };
  const send = (data) => { if (ws && ws.readyState === WebSocket.OPEN) ws.send(data); };
  const sendSize = () => send(JSON.stringify({ type: "resize", cols: term.cols, rows: term.rows }));
  const connect = () => {
    const proto = location.protocol === "https:" ? "wss:" : "ws:";
    ws = new WebSocket(`${proto}//${location.host}/api/clusters/${encodeURIComponent(name)}/terminal`);
    ws.binaryType = "arraybuffer";
    status("接続中", "info");
    reconnectBtn.hidden = true;
    ws.onopen = () => { status("接続済み", "ok"); sendSize(); };
    ws.onmessage = (e) => term.write(typeof e.data === "string" ? e.data : new Uint8Array(e.data));
    ws.onclose = (e) => {
      status("切断", "");
      term.write(`\r\n\x1b[33m[切断されました${e.reason ? `: ${e.reason}` : ""}]\x1b[0m\r\n`);
      reconnectBtn.hidden = false;
    };
  };
  term.onData((d) => send(encoder.encode(d)));
  term.onBinary((d) => send(Uint8Array.from(d, (c) => c.charCodeAt(0))));
  term.onResize(sendSize);
  const onWindowResize = () => fit.fit();
  window.addEventListener("resize", onWindowResize);
  const onReconnect = () => { term.reset(); connect(); term.focus(); };
  reconnectBtn.addEventListener("click", onReconnect);
  connect();

  const cleanup = () => {
    window.removeEventListener("resize", onWindowResize);
    reconnectBtn.removeEventListener("click", onReconnect);
    if (ws) { ws.onclose = null; ws.close(); }
    term.dispose();
  };
  // paste は文字列をターミナルに貼る (実行はしない。複数行は bracketed paste で 1 度に貼る)
  cleanup.paste = (text) => { term.paste(text); term.focus(); };
  return cleanup;
}

// viewTerminal はクラスタの kubeconfig を設定したシェルを開く。
async function viewTerminal(name) {
  setNav("");
  await loadXterm();
  const enc = encodeURIComponent(name);
  app.innerHTML = html`
    <h1><a href="#/clusters/${enc}">${name}</a> のターミナル <span class="badge info" id="term-status">接続中</span></h1>
    <div class="term-wrap" id="term"></div>
    <div class="actions spaced-top">
      <button id="term-reconnect" hidden>再接続</button>
      <a class="button" href="#/clusters/${enc}">クラスタの画面に戻る</a>
    </div>
    <p class="hint">kubectl (<code>k</code>) が設定済みのシェルです (Talos のクラスタは talosctl も)。
      ミニPC の権限の無い一時ユーザーで動き、ホームはこのセッションだけのもので、閉じると消えます。</p>`;
  // ほかの画面に移るときに閉じる
  teardown = mountTerminal(document.getElementById("term"), name, document.getElementById("term-status"), document.getElementById("term-reconnect"));
}

// ---------- 練習問題 (CKA / CKAD) ----------

const PRACTICE_CLUSTER_KEY = "niwakube.practice.cluster";
const storageGet = (k) => { try { return localStorage.getItem(k); } catch { return null; } };
const storageSet = (k, v) => { try { localStorage.setItem(k, v); } catch { /* 保存できなくても動く */ } };

// viewPractice は練習問題の一覧。
async function viewPractice() {
  setNav("practice");
  const problems = await api("GET", "/api/practice/problems");
  const exams = [...new Set(problems.map((p) => p.exam))];
  app.innerHTML = html`
    <h1>練習問題 (CKA / CKAD)</h1>
    <div class="card">
      <p>本物のクラスタで CKA / CKAD の練習ができます。問題を開くと、左に問題文、右にクラスタのターミナルが出ます。
        「出題する」で問題の Namespace を作り直し、ターミナルの kubectl で解いたら「採点」を押します。</p>
      <p class="hint">問題は公開されている出題範囲に沿ったオリジナルで、本物の試験問題ではありません。
        練習用のクラスタを 1 つ作っておくと、ほかのクラスタを壊す心配がありません (試験と同じ kubeadm、ワーカー 1 台以上がおすすめ)。
        「ノードに SSH」の問題 (CKA) は、ターミナルから ssh cp / ssh worker でノードに入って解きます。
        この機能より前に作ったクラスタにはノードの鍵が無いので、新しく作ったクラスタを使ってください。</p>
    </div>
    ${exams.map((exam) => html`<div class="card">
      <h2>${exam}</h2>
      <div class="table-wrap"><table>
        <thead><tr><th>問題</th><th>分野</th><th>使えるクラスタ</th><th class="num">配点</th></tr></thead>
        <tbody>${problems.filter((p) => p.exam === exam).map((p) => html`<tr>
          <td><a href="#/practice/${encodeURIComponent(p.id)}">${p.title}</a></td><td>${p.domain}</td>
          <td>${p.requires ? html`<span class="tag">${p.requires}</span>` : html`<span class="hint">どれでも</span>`}${p.nodes ? html`<span class="tag">ノードに SSH</span>` : ""}</td>
          <td class="num">${p.weight}</td></tr>`)}</tbody>
      </table></div>
    </div>`)}`;
}

function practiceResult(r) {
  const all = r.passed === r.total;
  return html`<div class="practice-result ${all ? "all" : ""}">
    <p><strong>${r.score} / ${r.weight} 点</strong> <span class="hint">(${r.passed} / ${r.total} 項目)</span>${all ? html` <span class="badge ok">正解</span>` : ""}</p>
    <ul class="checks">${r.checks.map((c) => html`<li class="${c.passed ? "pass" : "fail"}">
      <span class="mark">${c.passed ? "✓" : "✗"}</span> ${c.description}${c.message ? html` <span class="hint">(${c.message})</span>` : ""}</li>`)}</ul>
  </div>`;
}

// viewProblem は 1 問の画面。試験と同じく、左に問題文、右にターミナルを置く。
async function viewProblem(id) {
  setNav("practice");
  const [p, clusters] = await Promise.all([api("GET", `/api/practice/problems/${encodeURIComponent(id)}`), api("GET", "/api/clusters")]);
  const ready = clusters.filter((c) => c.status.phase === "ready" && (!p.requires || c.distro === p.requires)).map((c) => c.name);
  const saved = storageGet(PRACTICE_CLUSTER_KEY);
  let chosen = ready.includes(saved) ? saved : ready[0];
  app.innerHTML = html`
    <h1><a href="#/practice">練習問題</a> / ${p.title} <span class="badge info">${p.exam}</span></h1>
    <div class="practice-grid">
      <div>
        <div class="card">
          <p class="hint">${p.domain} · 配点 ${p.weight}</p>
          <div class="task">${p.task}</div>
        </div>
        <div class="card">
          ${ready.length ? html`<div class="inline">
            <label class="inline">クラスタ <select id="pr-cluster">${ready.map((n) => html`<option ${n === chosen ? raw("selected") : ""}>${n}</option>`)}</select></label>
            <button class="primary" id="pr-setup">出題する</button>
            <button id="pr-check">採点</button>
          </div>
          <p class="hint">${(p.namespaces || []).length ? `「出題する」は Namespace ${p.namespaces.join(", ")} を消して作り直します。` : "「出題する」で出題直後の状態にします。"}
            やり直すときももう一度押します。${p.resetSnapshot ? "この問題は最初の出題でクラスタのスナップショットを取り (数分かかります)、2 回目からはそれに戻します。" : ""}</p>
          <div id="pr-avail"></div>`
          : html`<p>${p.requires ? `${p.requires} の` : ""}Ready のクラスタがありません。<a href="#/clusters/new">練習用のクラスタを作って</a>から開いてください。</p>`}
          <div id="pr-result"></div>
        </div>
        ${p.hint ? html`<details class="card"><summary>ヒント</summary><div class="task">${p.hint}</div></details>` : ""}
        ${p.solution ? html`<details class="card"><summary>解説と解き方の例</summary><div class="task">${p.solution}</div>
          ${ready.length ? html`<div class="actions spaced-top"><button id="pr-solve">模範解答をクラスタに当てる</button></div>
          <p class="hint">解き方の例と同じ状態をクラスタに作ります。当てたあとに「採点」で満点になることを確かめられます。</p>` : ""}</details>` : ""}
      </div>
      <div>
        <div class="inline spaced-bottom"><strong>ターミナル</strong> <span class="badge" id="term-status">未接続</span> <button id="term-reconnect" hidden>再接続</button></div>
        <div class="term-wrap" id="term"></div>
      </div>
    </div>`;
  if (!ready.length) return;

  app.classList.add("wide"); // 問題文とターミナルを並べるので広く使う
  await loadXterm();
  let closeTerm = null;
  const open = () => {
    if (closeTerm) closeTerm();
    document.getElementById("term").replaceChildren();
    closeTerm = mountTerminal(document.getElementById("term"), chosen, document.getElementById("term-status"), document.getElementById("term-reconnect"));
  };
  open();
  teardown = () => { if (closeTerm) closeTerm(); app.classList.remove("wide"); };

  const result = document.getElementById("pr-result");
  document.getElementById("pr-cluster").addEventListener("change", (e) => {
    chosen = e.target.value;
    storageSet(PRACTICE_CLUSTER_KEY, chosen);
    result.replaceChildren();
    open();
  });
  const run = async (btn, busy, fn) => {
    const label = btn.textContent;
    btn.disabled = true;
    btn.textContent = busy;
    try { await fn(); } catch (err) { toast(err.message, true); } finally { btn.disabled = false; btn.textContent = label; }
  };
  // 選んだクラスタでこの問題を出せるか (ディストリ、ノードの鍵、バージョン)
  const avail = document.getElementById("pr-avail");
  const checkAvail = async () => {
    avail.replaceChildren();
    let ok = true;
    try {
      const list = await api("GET", `/api/practice/problems?cluster=${encodeURIComponent(chosen)}`);
      const me = list.find((x) => x.id === p.id);
      if (me && me.available === false) {
        ok = false;
        avail.innerHTML = html`<p class="badge warn">このクラスタでは出せません</p> <span class="hint">${me.reason}</span>`;
      }
    } catch { /* 確かめられなければ出題のときにエラーが出る */ }
    ["pr-setup", "pr-check", "pr-solve"].forEach((bid) => { const b = document.getElementById(bid); if (b) b.disabled = !ok; });
  };
  checkAvail();
  document.getElementById("pr-cluster").addEventListener("change", checkAvail);
  const setupBtn = document.getElementById("pr-setup");
  setupBtn.addEventListener("click", () => run(setupBtn, "出題中…", async () => {
    await api("POST", `/api/practice/problems/${encodeURIComponent(p.id)}/setup`, { cluster: chosen });
    result.replaceChildren();
    toast(`${chosen} に出題しました。右のターミナルで解いてください`);
  }));
  const solveBtn = document.getElementById("pr-solve");
  if (solveBtn) solveBtn.addEventListener("click", () => {
    if (!confirm(`模範解答を ${chosen} に当てます。自分の解答は上書きされます。よろしいですか?`)) return;
    run(solveBtn, "当てています…", async () => {
      await api("POST", `/api/practice/problems/${encodeURIComponent(p.id)}/solve`, { cluster: chosen });
      result.replaceChildren();
      toast("模範解答を当てました。少し待ってから「採点」を押してください");
    });
  });
  const checkBtn = document.getElementById("pr-check");
  checkBtn.addEventListener("click", () => run(checkBtn, "採点中…", async () => {
    const r = await api("POST", `/api/practice/problems/${encodeURIComponent(p.id)}/check`, { cluster: chosen });
    result.innerHTML = practiceResult(r);
  }));
}

// ---------- CloudNative Days のハンズオン (資料とターミナルを並べる) ----------

const HANDSON_KEY = "niwakube.handson.doc"; // クラスタごとに、最後に開いた資料

const ALERTS = { NOTE: "Note", TIP: "Tip", IMPORTANT: "Important", WARNING: "Warning", CAUTION: "Caution" };

// decorateDoc は資料の HTML に、ターミナルに貼るボタンと GitHub の注記 ([!NOTE] など) の見た目を足す。
function decorateDoc(el, paste) {
  el.querySelectorAll("pre > code").forEach((code) => {
    const lang = (code.className.match(/language-(\S+)/) || [])[1] || "";
    if (lang && !["bash", "sh", "shell", "console", "zsh"].includes(lang)) return; // yaml や出力の例には付けない
    const text = code.textContent.replace(/\n+$/, "");
    const bar = document.createElement("div");
    bar.className = "code-actions";
    const pasteBtn = document.createElement("button");
    pasteBtn.type = "button";
    pasteBtn.textContent = "ターミナルに貼る";
    pasteBtn.title = "右のターミナルに貼ります (Enter で実行)";
    pasteBtn.addEventListener("click", () => paste(text));
    const copyBtn = document.createElement("button");
    copyBtn.type = "button";
    copyBtn.textContent = "コピー";
    copyBtn.addEventListener("click", async () => {
      try { await navigator.clipboard.writeText(text); toast("コピーしました"); } catch { toast("コピーできませんでした", true); }
    });
    bar.append(pasteBtn, copyBtn);
    code.parentElement.before(bar);
  });
  el.querySelectorAll("blockquote").forEach((q) => {
    const p = q.querySelector("p");
    const m = p && p.firstChild && p.firstChild.nodeType === Node.TEXT_NODE && p.firstChild.textContent.match(/^\s*\[!(NOTE|TIP|IMPORTANT|WARNING|CAUTION)\]\s*/);
    if (!m) return;
    p.firstChild.textContent = p.firstChild.textContent.slice(m[0].length);
    q.classList.add("alert", `alert-${m[1].toLowerCase()}`);
    const title = document.createElement("p");
    title.className = "alert-title";
    title.textContent = ALERTS[m[1]];
    q.prepend(title);
  });
}

// viewHandson は、左にハンズオンの資料 (GitHub の README)、右にクラスタのターミナルを並べる。
async function viewHandson(name, chapterId, file) {
  setNav("");
  const enc = encodeURIComponent(name);
  const chapters = await api("GET", "/api/handson/chapters");
  if (!chapterId) {
    const saved = (storageGet(`${HANDSON_KEY}.${name}`) || "").split("/");
    if (chapters.some((c) => c.id === saved[0])) [chapterId, file] = saved;
  }
  if (!chapters.some((c) => c.id === chapterId)) chapterId = chapters[0].id;
  app.innerHTML = html`
    <h1><a href="#/clusters/${enc}">${name}</a> / CloudNative Days のハンズオン</h1>
    <div class="practice-grid handson-grid">
      <div>
        <div class="card">
          <div class="inline">
            <label class="inline">章 <select id="ho-chapter">${chapters.map((c, i) => html`<option value="${c.id}" ${c.id === chapterId ? raw("selected") : ""}>${i}. ${c.title}</option>`)}</select></label>
            <span id="ho-docs" class="inline"></span>
            <a class="button" id="ho-source" target="_blank" rel="noopener">GitHub で開く</a>
          </div>
          <p class="hint">資料は <a href="https://github.com/cloudnativedaysjp/cnd-handson" target="_blank" rel="noopener">cloudnativedaysjp/cnd-handson</a> から取っています。
            コマンドの「ターミナルに貼る」で右に貼り、Enter で実行します。</p>
        </div>
        <div id="ho-notes"></div>
        <div class="card md" id="ho-doc"><p class="hint">読み込み中…</p></div>
      </div>
      <div>
        <div class="inline spaced-bottom"><strong>ターミナル</strong> <span class="badge" id="term-status">未接続</span> <button id="term-reconnect" hidden>再接続</button></div>
        <div class="term-wrap" id="term"></div>
      </div>
    </div>`;

  app.classList.add("wide");
  await loadXterm();
  const closeTerm = mountTerminal(document.getElementById("term"), name, document.getElementById("term-status"), document.getElementById("term-reconnect"));
  teardown = () => { closeTerm(); app.classList.remove("wide"); };

  const docEl = document.getElementById("ho-doc");
  // 章を変えてもターミナルはつないだまま (hashchange を起こさないように replaceState で URL だけ変える)
  const load = async (id, f, anchor) => {
    const c = chapters.find((x) => x.id === id);
    f = f && c.docs.some((d) => d.file === f) ? f : c.docs[0].file;
    document.getElementById("ho-chapter").value = id;
    history.replaceState(null, "", `#/clusters/${enc}/handson/${encodeURIComponent(id)}/${encodeURIComponent(f)}`);
    storageSet(`${HANDSON_KEY}.${name}`, `${id}/${f}`);
    document.getElementById("ho-docs").innerHTML = c.docs.length > 1
      ? c.docs.map((d) => html`<button type="button" class="${d.file === f ? "primary" : ""}" data-doc="${d.file}">${d.title}</button>`).join("")
      : "";
    document.getElementById("ho-notes").innerHTML = c.notes && c.notes.length
      ? html`<div class="card notes"><h2>niwakube での読み替え</h2><ul>${c.notes.map((n) => html`<li>${n}</li>`)}</ul>
        <p class="hint">ほかの章も含めた一覧は <a href="https://github.com/sat0ken/niwakube/blob/main/docs/cnd-handson.md" target="_blank" rel="noopener">docs/cnd-handson.md</a></p></div>`
      : "";
    docEl.innerHTML = html`<p class="hint">読み込み中…</p>`;
    try {
      const page = await api("GET", `/api/handson/chapters/${encodeURIComponent(id)}?doc=${encodeURIComponent(f)}`);
      document.getElementById("ho-source").href = page.source;
      docEl.innerHTML = page.html; // サーバーで決まったタグと属性だけにしてある
      decorateDoc(docEl, closeTerm.paste);
      const target = anchor && document.getElementById(anchor);
      if (target) target.scrollIntoView(); else window.scrollTo(0, 0);
    } catch (err) {
      docEl.innerHTML = html`<p class="error">資料を読み込めませんでした: ${err.message}</p>
        <p class="hint">ミニPC から GitHub (raw.githubusercontent.com) に届くか確かめてください。</p>`;
    }
  };
  document.getElementById("ho-chapter").addEventListener("change", (e) => load(e.target.value));
  document.getElementById("ho-docs").addEventListener("click", (e) => {
    const b = e.target.closest("[data-doc]");
    if (b) load(document.getElementById("ho-chapter").value, b.dataset.doc);
  });
  // 資料の中のリンク: ほかの章の資料はこの画面で開き、見出しへのリンクはスクロールする
  docEl.addEventListener("click", (e) => {
    const a = e.target.closest("a[href^='#']");
    if (!a) return;
    const href = a.getAttribute("href");
    e.preventDefault();
    if (href.startsWith("#doc:")) {
      const [id, f] = href.slice(5).split("/");
      load(id, f);
    } else if (href.startsWith("#md-")) {
      const target = document.getElementById(href.slice(1));
      if (target) target.scrollIntoView({ behavior: "smooth" });
    }
  });
  await load(chapterId, file);
}

// ---------- ルーター ----------

// 表示中の画面の後片付け (ターミナルの WebSocket を閉じるなど)
let teardown = null;

function route() {
  const parts = location.hash.replace(/^#\/?/, "").split("/").map(decodeURIComponent);
  if (parts[0] === "clusters" && parts[1] === "new") return { view: viewNewCluster, live: false };
  if (parts[0] === "clusters" && parts[1] && parts[2] === "terminal") return { view: () => viewTerminal(parts[1]), live: false };
  if (parts[0] === "clusters" && parts[1] && parts[2] === "handson") return { view: () => viewHandson(parts[1], parts[3], parts[4]), live: false };
  if (parts[0] === "clusters" && parts[1]) return { view: () => viewCluster(parts[1]), live: true, serial: true };
  if (parts[0] === "practice" && parts[1]) return { view: () => viewProblem(parts[1]), live: false };
  if (parts[0] === "practice") return { view: viewPractice, live: false };
  if (parts[0] === "vms" && parts[1]) return { view: () => viewVM(parts[1]), live: true, serial: true };
  if (parts[0] === "vms") return { view: viewVMs, live: true };
  return { view: viewHome, live: true };
}

let current = route();

async function render() {
  try {
    await current.view();
    // CSP で style 属性は使えないので、メーターの幅はここで設定する
    app.querySelectorAll("[data-pct]").forEach((el) => { el.style.width = `${el.dataset.pct}%`; });
  } catch (err) {
    if (loginShown) return; // ログインの画面に切り替えた
    app.innerHTML = html`<div class="card error">読み込めませんでした: ${err.message}</div>`;
  }
}

window.addEventListener("hashchange", () => {
  if (teardown) { teardown(); teardown = null; }
  clusterOpActive = false;
  current = route();
  window.scrollTo(0, 0);
  render();
});

setInterval(() => {
  if (document.hidden || !current.live) return;
  if (current.serial) {
    // 詳細画面は、表の更新でスクロール位置が飛ばないようにシリアルログだけを更新する。
    // ただしクラスタの操作 (サイズの変更) が進んでいる間は、進み具合が見えるよう画面全体を更新する
    if (clusterOpActive && !isEditing()) { render(); return; }
    if (serialState.auto) loadSerial(serialState.machine);
    return;
  }
  if (!isEditing()) render();
}, REFRESH_MS);

// ---------- ログイン (Google / GitHub。niwakubed に設定があるときだけ) ----------

let loginShown = false;

// showLogin は、ログインのボタンを並べた画面を出す (API が 401 を返したとき)。
async function showLogin() {
  if (loginShown) return;
  loginShown = true;
  if (teardown) { teardown(); teardown = null; }
  current = { view: showLogin, live: false };
  document.querySelector(".topbar nav").hidden = true;
  let providers = [];
  try { providers = await (await fetch("/auth/providers")).json(); } catch { /* 下で案内する */ }
  const next = location.pathname + location.hash;
  app.innerHTML = html`
    <div class="card login">
      <h1>niwakube にログイン</h1>
      ${providers.length
        ? html`<p class="hint">許可されたアカウントでログインしてください。</p>
          <div class="login-buttons">${providers.map((p) => html`<a class="button primary" href="/auth/login/${encodeURIComponent(p.name)}?next=${encodeURIComponent(next)}">${p.label}</a>`)}</div>`
        : html`<p>この niwakubed は API トークンでだけ使えます (ログインのプロバイダーが設定されていません)。</p>`}
    </div>`;
}

// loadUser は、ログインしている人を上のバーに出す。ログインを使っていなければ (/auth/me が無い) 何も出さない。
async function loadUser() {
  const res = await fetch("/auth/me").catch(() => null);
  if (!res || res.status === 404) return true; // ログインなし
  if (res.status === 401) { showLogin(); return false; }
  const me = await res.json();
  document.getElementById("user-name").textContent = me.name;
  document.getElementById("user").hidden = false;
  document.getElementById("logout").addEventListener("click", async () => {
    await fetch("/auth/logout", { method: "POST" });
    location.reload();
  });
  return true;
}

loadUser().then((ok) => {
  if (!ok) return;
  api("GET", "/api/host").then((h) => { document.getElementById("host-name").textContent = `${h.hostname} · ${h.version}`; }).catch(() => {});
  render();
});
