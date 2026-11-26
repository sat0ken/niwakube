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

function setNav(name) {
  document.querySelectorAll("nav a").forEach((a) => a.classList.toggle("active", a.dataset.nav === name));
}

// 入力中 (フォームにフォーカスがある) は自動更新で書き換えない
const isEditing = () => document.activeElement && document.activeElement.closest && document.activeElement.closest("form");

// ---------- 画面: ダッシュボード ----------

async function viewHome() {
  setNav("home");
  const [host, clusters] = await Promise.all([api("GET", "/api/host"), api("GET", "/api/clusters")]);
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
    const d = distros.find((x) => x.name === form.distro.value);
    form.version.placeholder = d.defaultVersion;
    const talos = d.name === "talos";
    const cilium = form.cni.value === "cilium";
    form.hubble.disabled = !cilium;
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

async function viewCluster(name) {
  setNav("");
  const [c, forwards] = await Promise.all([
    api("GET", `/api/clusters/${encodeURIComponent(name)}`),
    api("GET", `/api/clusters/${encodeURIComponent(name)}/forwards`),
  ]);
  if (!serialState.machine || !c.machines.some((m) => m.name === serialState.machine)) {
    serialState.machine = c.machines[0] ? c.machines[0].name : "";
  }
  const enc = encodeURIComponent(c.name);
  app.innerHTML = html`
    <h1>${c.name} ${phaseBadge(c.status.phase)}
      <span class="hint">${c.status.readyNodes} / ${c.status.nodes} ノード Ready${c.status.message ? ` · ${c.status.message}` : ""}</span></h1>
    <div class="card">
      <div class="actions">
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
      <h2>シリアルコンソールのログ</h2>
      <label class="inline spaced-bottom"><input type="checkbox" id="serial-auto" ${serialState.auto ? raw("checked") : ""}> 自動更新</label>
      <pre class="log" id="serial">読み込み中…</pre>
    </div>`;
  document.getElementById("serial-auto").addEventListener("change", (e) => { serialState.auto = e.target.checked; });
  loadSerial(v.name);
}

// ---------- ルーター ----------

function route() {
  const parts = location.hash.replace(/^#\/?/, "").split("/").map(decodeURIComponent);
  if (parts[0] === "clusters" && parts[1] === "new") return { view: viewNewCluster, live: false };
  if (parts[0] === "clusters" && parts[1]) return { view: () => viewCluster(parts[1]), live: true, serial: true };
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
    app.innerHTML = html`<div class="card error">読み込めませんでした: ${err.message}</div>`;
  }
}

window.addEventListener("hashchange", () => { current = route(); window.scrollTo(0, 0); render(); });

setInterval(() => {
  if (document.hidden || !current.live) return;
  if (current.serial) {
    // 詳細画面は、表の更新でスクロール位置が飛ばないようにシリアルログだけを更新する
    if (serialState.auto) loadSerial(serialState.machine);
    return;
  }
  if (!isEditing()) render();
}, REFRESH_MS);

api("GET", "/api/host").then((h) => { document.getElementById("host-name").textContent = `${h.hostname} · ${h.version}`; }).catch(() => {});
render();
