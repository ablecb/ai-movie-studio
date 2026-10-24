/* AI 电影工作室控制台 —— 前端单页应用（无构建依赖） */
'use strict';

/* ================= 基础工具 ================= */
const $ = s => document.querySelector(s);
const esc = s => String(s ?? '').replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));

async function api(path, opts) {
  const r = await fetch(path, opts);
  let j = null;
  try { j = await r.json(); } catch { /* 空响应 */ }
  if (!r.ok) throw new Error((j && j.error) || ('HTTP ' + r.status));
  return j;
}

function fmtBytes(n) {
  if (n == null) return '-';
  if (n < 1024) return n + ' B';
  if (n < 1048576) return (n / 1024).toFixed(1) + ' KB';
  if (n < 1073741824) return (n / 1048576).toFixed(1) + ' MB';
  return (n / 1073741824).toFixed(2) + ' GB';
}
function fmtMS(ms) {
  if (!ms) return '-';
  const s = ms / 1000;
  if (s < 60) return s.toFixed(1) + ' 秒';
  return Math.floor(s / 60) + ' 分 ' + Math.round(s % 60) + ' 秒';
}
function fmtSec(v) { return (v == null || v === 0) ? '-' : Number(v).toFixed(2) + 's'; }
function fmtTime(t) {
  if (!t || String(t).startsWith('0001-01-01')) return '-'; // 零值时间（Go time.Time{}）不渲染
  return new Date(t).toLocaleString('zh-CN', { hour12: false });
}
function fmtDurMs(ms) {
  if (ms == null) return '-';
  if (ms < 1000) return ms + ' ms';
  const s = ms / 1000;
  return s < 60 ? s.toFixed(1) + ' s' : Math.floor(s / 60) + ' 分 ' + Math.round(s % 60) + ' 秒';
}
const aiKindLabel = k => k === 'chat' ? '文字' : k === 'image' ? '图片' : k === 'video' ? '视频' : (k || '-');
function fmtAgo(t) {
  if (!t) return '-';
  const d = (Date.now() - new Date(t).getTime()) / 1000;
  if (d < 60) return '刚刚';
  if (d < 3600) return Math.floor(d / 60) + ' 分钟前';
  if (d < 86400) return Math.floor(d / 3600) + ' 小时前';
  return Math.floor(d / 86400) + ' 天前';
}
function trunc(s, n) { s = String(s ?? ''); return s.length > n ? s.slice(0, n) + '…' : s; }
function fileUrl(run, path, download) {
  return `/api/runs/${encodeURIComponent(run)}/file/${path.split('/').map(encodeURIComponent).join('/')}` + (download ? '?download=1' : '');
}

const badgeMap = {
  done: 'b-done', ok: 'b-ok', pass: 'b-ok', complete: 'b-done', corrected: 'b-ok',
  running: 'b-running', info: 'b-info',
  failed: 'b-failed', err: 'b-failed', blocking: 'b-failed',
  skipped: 'b-skipped', unknown: 'b-skipped', other: 'b-skipped',
  pending: 'b-pending', warn: 'b-pending', warning: 'b-pending',
  canceled: 'b-canceled', interrupted: 'b-canceled', none: 'b-muted', '': 'b-muted',
};
function badge(status, text) {
  const cls = badgeMap[String(status || '').toLowerCase()] || 'b-muted';
  return `<span class="badge ${cls}">${esc(text || status || '-')}</span>`;
}
const stageIcon = { done: '✓', failed: '✗', running: '…', skipped: '⤼', pending: '·' };

function toast(msg, type = 'info', ms = 3800) {
  const el = document.createElement('div');
  el.className = 'toast ' + type;
  el.textContent = msg;
  $('#toast-root').appendChild(el);
  setTimeout(() => el.remove(), ms);
}

/* ================= 弹窗 ================= */
let modalStack = [];
function openModal(title, bodyHTML, footHTML, width) {
  const mask = document.createElement('div');
  mask.className = 'modal-mask';
  mask.innerHTML = `<div class="modal" ${width ? `style="width:min(${width},94vw)"` : ''}>
    <div class="modal-head"><div class="modal-title">${title}</div>
      <button class="modal-close" title="关闭">✕</button></div>
    <div class="modal-body">${bodyHTML}</div>
    ${footHTML ? `<div class="modal-foot">${footHTML}</div>` : ''}</div>`;
  mask.addEventListener('mousedown', e => { if (e.target === mask) closeModal(mask); });
  mask.querySelector('.modal-close').onclick = () => closeModal(mask);
  $('#modal-root').appendChild(mask);
  modalStack.push(mask);
  return mask;
}
function closeModal(mask) {
  if (!mask) mask = modalStack[modalStack.length - 1];
  if (!mask) return;
  modalStack = modalStack.filter(m => m !== mask);
  mask.remove();
  mask._cleanup && mask._cleanup();
}

/* ================= 参考帧审核链路 ================= */
// 审计来源：RefResult.qc_review（素材索引 index.json 与渲染 manifest.refs 同结构）。
// 每轮候选：round 0=首出，1..=重出；verdict=视觉核对结论；final=最终采用的那张。
const QC_STATUS_TEXT = { pass: '核对通过', corrected: '重出后通过', failed: '不合格（重出用尽）', skipped: '未检查', unknown: '审核未定稿' };
function qcReviewSummary(ref) {
  const rows = (ref && ref.qc_review) || [];
  if (!rows.length) return '';
  const repairs = rows.filter(r => r.round > 0).length;
  const fin = rows.find(r => r.final);
  const score = fin && fin.verdict && fin.verdict.score != null ? ` · ${fin.verdict.score} 分` : '';
  return `共 ${rows.length} 轮${repairs ? ` · 重出 ${repairs} 次` : ' · 未重出'}${score}`;
}
function qcRoundBody(r) {
  const v = r.verdict || {};
  const out = [];
  if (r.gen_error) out.push(`<div style="font-size:12px;color:var(--err-text)">✗ 出图/下载失败：${esc(r.gen_error)}</div>`);
  if (r.qc_error) out.push(`<div style="font-size:12px;color:var(--err-text)">✗ 质检调用失败：${esc(r.qc_error)}</div>`);
  if (r.archive_error) out.push(`<div class="muted" style="font-size:11px">⚠ 候选图存档失败（追溯时缺图）：${esc(r.archive_error)}</div>`);
  const list = (title, arr) => (arr && arr.length)
    ? `<div style="font-size:12px;margin-top:6px"><b>${title}</b><ul class="qc-issues">${arr.map(x => `<li>${esc(x)}</li>`).join('')}</ul></div>` : '';
  out.push(list('清单有、画面缺失/不符', v.missing));
  out.push(list('画面有、清单没有（AI 私自添加）', v.drift));
  out.push(list('其他生成缺陷', v.issues));
  if (v.fix_hint) out.push(`<div style="font-size:12px;margin-top:6px"><b>审核修正建议</b>：${esc(v.fix_hint)}</div>`);
  if (r.fix_hint_en) out.push(`<div class="qc-fix"><b>已追加给出图提示词的修正指令</b>（下一轮据此重出）<div class="mono" style="margin-top:4px">${esc(r.fix_hint_en)}</div></div>`);
  if (!out.length) out.push(`<div class="muted" style="font-size:12px;margin-top:6px">${v.pass ? '本帧通过核对，无问题记录。' : '本轮没有留下核对结论。'}</div>`);
  return out.join('');
}
function qcReviewModal(run, ref) {
  const rows = (ref && ref.qc_review) || [];
  const title = `🔍 参考帧审核链路 · ${esc(ref.name || ref.token || '')}`;
  if (!rows.length) {
    openModal(title, `<div class="muted" style="line-height:1.9">
      这张帧没有审核记录（旧版生成或复用帧）。<br>现行流程：出图 → 视觉模型对照角色圣经逐项核对 → 不合格带着修正指令换种子重出；
      每轮候选图（含被否掉的）与结论都会在这里逐轮展示。</div>`, null, '760px');
    return;
  }
  const repairs = rows.filter(r => r.round > 0).length;
  const status = ref.qc_status || 'unknown';
  const head = `<div class="flex spread">
      <div class="flex" style="gap:8px">${badge(status, QC_STATUS_TEXT[status] || status)}
        <span class="tag">${repairs ? `重出 ${repairs} 次` : '首次即定稿'}</span></div>
      <div class="muted" style="font-size:12px">共 ${rows.length} 轮 · 渲染锁脸用「最终采用」那张</div>
    </div>`;
  const body = rows.map(r => {
    const v = r.verdict;
    const verdictBadge = v
      ? (v.pass ? badge('pass', `通过 · ${v.score} 分`) : badge('failed', `未通过 · ${v.score} 分`))
      : (r.qc_error || r.gen_error ? badge('failed', '核对未完成') : badge('skipped', '未质检'));
    const cls = r.final ? 'approved' : ((v && !v.pass) || r.gen_error || r.qc_error ? 'rejected' : '');
    const img = r.image
      ? `<a href="${fileUrl(run, r.image)}" target="_blank" title="点击查看原图"><img src="${fileUrl(run, r.image)}" loading="lazy" alt="第 ${r.round} 轮候选"></a>`
      : `<div class="qc-none">${r.gen_error ? '出图失败' : '无图存档'}</div>`;
    const meta = [
      r.image ? esc(r.image) : '',
      r.model ? esc(r.model) : '',
      r.seed ? `seed ${r.seed}` : '',
      r.at ? fmtAgo(r.at) : '',
    ].filter(Boolean).join(' · ');
    return `<div class="qc-round ${cls}">
      ${img}
      <div style="min-width:0">
        <div class="flex spread" style="gap:8px">
          <b>${r.round === 0 ? (r.reused ? '第 0 轮 · 复用帧补查' : '第 0 轮 · 首出') : (r.reused ? `第 ${r.round} 轮 · 复用帧补查` : `第 ${r.round} 轮 · 重出`)}</b>
          <span class="flex" style="gap:6px">${verdictBadge}${r.recovered ? '<span class="tag">调用记录回溯</span>' : ''}${r.final ? badge('done', '✅ 最终采用') : ''}</span>
        </div>
        <div class="mono muted" style="font-size:10.5px;margin-top:4px;word-break:break-all">${meta}</div>
        ${qcRoundBody(r)}
      </div></div>`;
  }).join('');
  openModal(title, head + `<div class="mt">${body}</div>`, null, '840px');
}

/* ================= 任务 ================= */
const jobTypeLabel = {
  run: '创作流水线', resume: '断点续跑', render: '渲染生成', animatic: '动态分镜',
  deliver: '交付成片', polish: '打磨闭环', takes: '多 Take 对比', verify: '复核',
  doctor: '环境自检', 'probe-ref': '参考图探针', assets: '素材库',
};

async function startJob(type, run, params, { confirmText } = {}) {
  if (confirmText && !window.confirm(confirmText)) return null;
  try {
    const job = await api('/api/jobs', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ type, run, params }),
    });
    toast(`已提交：${job.label}`, 'ok');
    pollJobs();
    return job;
  } catch (e) {
    toast('提交失败：' + e.message, 'err', 6000);
    return null;
  }
}

let prevJobStatus = {};
let jobTimer = null;async function pollJobs() {
  let jobs;
  try { jobs = await api('/api/jobs?limit=12'); } catch { return; }
  for (const j of jobs) {
    const prev = prevJobStatus[j.id];
    if (prev === 'running' && j.status !== 'running') {
      if (j.status === 'done') toast(`✅ 任务完成：${j.label}`, 'ok', 6000);
      else if (j.status === 'failed') toast(`✗ 任务失败：${j.label}（点顶栏任务查看日志）`, 'err', 8000);
      else if (j.status === 'canceled') toast(`已取消：${j.label}`, 'warn');
      // 关联项目数据可能已变化：结束后刷新当前页
      if (j.run_id && j.run_id === state.run) route();
      if (j.type === 'run' && j.run_id) loadRuns();
    }
  }
  prevJobStatus = {};
  jobs.forEach(j => prevJobStatus[j.id] = j.status);
  state.jobs = jobs;
  const running = jobs.filter(j => j.status === 'running').length;
  const pill = $('#job-pill');
  if (running > 0) {
    pill.classList.remove('hidden');
    pill.textContent = `⚙ ${running} 个任务执行中`;
  } else pill.classList.add('hidden');
}

/* 任务日志弹窗（运行中每 2 秒刷新，自动跟到底部） */
function openLogModal(jobId, label) {
  const mask = openModal(`📋 任务日志 · ${esc(label || jobId)}`,
    `<div class="muted mb" id="jm-status">加载中…</div><pre class="log" id="jm-log">加载中…</pre>`,
    `<button class="btn danger hidden" id="jm-cancel">取消任务</button><button class="btn" id="jm-close">关闭</button>`);
  const logEl = mask.querySelector('#jm-log');
  const stEl = mask.querySelector('#jm-status');
  const cancelBtn = mask.querySelector('#jm-cancel');
  let follow = true;
  logEl.addEventListener('scroll', () => {
    follow = logEl.scrollTop + logEl.clientHeight >= logEl.scrollHeight - 40;
  });
  let done = false;
  const refresh = async () => {
    try {
      const [job, log] = await Promise.all([
        api('/api/jobs/' + jobId),
        api(`/api/jobs/${jobId}/log?tail=600`),
      ]);
      if (!document.contains(logEl)) return;
      stEl.innerHTML = `${badge(job.status)} ${esc(job.label)} · 开始于 ${fmtTime(job.started_at)}${job.exit_code ? ` · 退出码 ${job.exit_code}` : ''}`;
      cancelBtn.classList.toggle('hidden', job.status !== 'running');
      logEl.textContent = log.log || '（暂无输出）';
      if (follow) logEl.scrollTop = logEl.scrollHeight;
      if (job.status !== 'running') {
        done = true;
        stEl.innerHTML += ' · 已结束';
      }
    } catch (e) {
      if (document.contains(logEl)) stEl.textContent = '刷新失败：' + e.message;
    }
  };
  refresh();
  const timer = setInterval(async () => {
    if (done || !document.contains(logEl)) { clearInterval(timer); return; }
    refresh();
  }, 2000);
  mask._cleanup = () => clearInterval(timer);
  mask.querySelector('#jm-cancel').onclick = async () => {
    if (!window.confirm('确认取消该任务？进程树（含 ffmpeg）将被终止。')) return;
    try { await api(`/api/jobs/${jobId}/cancel`, { method: 'POST' }); toast('已请求取消', 'warn'); }
    catch (e) { toast('取消失败：' + e.message, 'err'); }
  };
  mask.querySelector('#jm-close').onclick = () => closeModal(mask);
}

/* 任务监视器（顶栏入口） */
function openJobMonitor() {
  const mask = openModal('⚙ 任务监视器', `<div id="jobs-body">加载中…</div>`, null, '860px');
  const render = async () => {
    if (!document.contains(mask)) return;
    let jobs = [];
    try { jobs = await api('/api/jobs?limit=15'); } catch { }
    const el = mask.querySelector('#jobs-body');
    if (!jobs.length) { el.innerHTML = `<div class="empty"><div class="big">🌙</div>还没有任务记录</div>`; return; }
    el.innerHTML = `<table class="tbl"><tr><th>状态</th><th>任务</th><th>项目</th><th>开始</th><th>耗时</th><th>操作</th></tr>` +
      jobs.map(j => `<tr>
        <td>${badge(j.status)}</td><td>${esc(j.label)}</td>
        <td class="mono">${esc(j.run_id || '-')}</td>
        <td class="muted">${fmtAgo(j.started_at)}</td>
        <td class="muted">${j.ended_at ? fmtMS(new Date(j.ended_at) - new Date(j.started_at)) : '<span class="muted">进行中…</span>'}</td>
        <td><button class="btn small" data-log="${j.id}" data-label="${esc(j.label)}">日志</button>
            ${j.status === 'running' ? `<button class="btn small danger" data-cancel="${j.id}">取消</button>` : ''}</td>
      </tr>`).join('') + `</table>`;
    el.querySelectorAll('[data-log]').forEach(b => b.onclick = () => openLogModal(b.dataset.log, b.dataset.label));
    el.querySelectorAll('[data-cancel]').forEach(b => b.onclick = async () => {
      if (!window.confirm('确认取消该任务？')) return;
      try { await api(`/api/jobs/${b.dataset.cancel}/cancel`, { method: 'POST' }); toast('已请求取消', 'warn'); render(); }
      catch (e) { toast('取消失败：' + e.message, 'err'); }
    });
  };
  render();
  const timer = setInterval(render, 3000);
  mask._cleanup = () => clearInterval(timer);
}

/* ================= 项目选择 ================= */
const state = { run: localStorage.getItem('studio.run') || '', runs: [], jobs: [] };

async function loadRuns() {
  try { state.runs = (await api('/api/runs')) || []; } catch { state.runs = []; }
  const sel = $('#run-select');
  const cur = state.run;
  sel.innerHTML = `<option value="">— 选择项目 —</option>` +
    state.runs.map(r => `<option value="${esc(r.id)}">${esc(r.id.replace(/^run-/, ''))} · ${esc(trunc(r.brief, 22))}</option>`).join('');
  if (cur && state.runs.some(r => r.id === cur)) sel.value = cur;
  else if (cur && !state.runs.length) sel.innerHTML = `<option value="${esc(cur)}">${esc(cur)}</option>`, sel.value = cur;
  else state.run = '', localStorage.removeItem('studio.run');
  $('#sidebar-foot').innerHTML = state.run
    ? `当前项目<br><span class="mono">${esc(state.run)}</span>`
    : '未选择项目';
}
function pickRun() {
  if (state.run) return state.run;
  return null;
}
function needRunHTML(feature) {
  return `<div class="empty"><div class="big">🎬</div>
    请先在右上角选择一个项目，或到 <a href="#/dashboard">总览</a> 提交新的创作。<br>
    <span class="muted">${esc(feature)}</span></div>`;
}

/* ================= 通用渲染器 ================= */
function statCard(k, v, s, cls) {
  return `<div class="card ${cls || ''}"><div class="k">${k}</div><div class="v">${v}</div>${s ? `<div class="s">${s}</div>` : ''}</div>`;
}

function pipelineHTML(stages) {
  if (!stages || !stages.length) return `<div class="empty">无阶段数据</div>`;
  return `<div class="pipeline">` + stages.map((s, i) => `
    <div class="stage ${esc(s.status)}" title="${esc(s.error || s.repair_note || '')}">
      <div class="st">${stageIcon[s.status] || '·'} ${esc(s.id)}</div>
      <div class="sd">${esc(s.status)}${s.attempts > 1 ? ` · 尝试${s.attempts}` : ''}${s.duration_ms ? ` · ${fmtMS(s.duration_ms)}` : ''}</div>
    </div>${i < stages.length - 1 ? '<span class="stage-arrow">▸</span>' : ''}`).join('') + `</div>`;
}

/* ================= 工作流流程图（创作中心） =================
   节点=阶段、边=depends_on。布局取"依赖最长路径"分列，列内垂直居中。
   交互：点节点选中 → 底栏给出三种执行动作（跑到此 / 重跑此阶段 / 重跑及后续）。 */

/* 阶段中文名兜底（工作流文件读不到时用；正常以 workflows/movie.json 的 title 为准） */
const stageCnFallback = {
  script: '剧本创作', characters: '角色圣经', assets: '素材库', storyboard: '分镜设计',
  voiceover: '配音台本', shotspec: '生成规格', music: '音乐音效', subtitles: '字幕计划',
  editplan: '剪辑方案', review: '制片审片',
};

/* 节点显示名：去掉标题里的括号补充说明（节点窄），全名进 tooltip */
function stageTitle(id, wf) {
  const n = ((wf && wf.nodes) || []).find(x => x.id === id);
  const full = (n && n.title) || stageCnFallback[id] || id;
  const short = full.replace(/（[^）]*）/g, '').replace(/\([^)]*\)/g, '').trim();
  return { name: short || full, full };
}

/* flowReduceDeps 传递约简：若下游 v 经其它路径也能从上游 u 到达，(u,v) 这条线
   就不画——依赖的**可达性**由路径本身表达，直接画全量依赖只会得到一团扇出线
   （script 一个点连 6 个下游、voiceover 被 4 条长线跨节点穿过）。
   画出来的线是拓扑必要连线；完整依赖清单在节点 tooltip 里可查。 */
function flowReduceDeps(deps, ids) {
  const succ = {}; ids.forEach(id => succ[id] = []);
  ids.forEach(id => (deps[id] || []).forEach(d => { if (succ[d]) succ[d].push(id); }));
  // 从 from 出发（忽略 from→to 这条直连边）能否到达 to
  const reachable = (from, to) => {
    const seen = new Set([from]);
    const stack = [from];
    while (stack.length) {
      const cur = stack.pop();
      for (const nxt of succ[cur] || []) {
        if (cur === from && nxt === to) continue; // 忽略直连边本身
        if (nxt === to) return true;
        if (!seen.has(nxt)) { seen.add(nxt); stack.push(nxt); }
      }
    }
    return false;
  };
  const out = {};
  ids.forEach(id => { out[id] = (deps[id] || []).filter(d => !reachable(d, id)); });
  return out;
}

/* flowLayout 计算节点坐标与边：层号=约简后依赖的最长路径。
   用约简后的边分层还有一个好处：review 这类"依赖几乎全部上游"的节点不会
   被推到最右侧独自成列，而是落回它的直接上游（shotspec）之后。 */
function flowLayout(stages, wf) {
  const NW = 132, NH = 58, GX = 46, GY = 14;
  const ids = stages.map(s => s.id);
  const known = new Set(ids);
  const hasWF = !!(wf && wf.nodes && wf.nodes.length);
  const rawDeps = {};
  ids.forEach((id, i) => {
    const n = hasWF ? (wf.nodes.find(x => x.id === id)) : null;
    if (n && (n.depends_on || []).length) rawDeps[id] = n.depends_on.filter(d => known.has(d));
    else if (n) rawDeps[id] = [];
    else rawDeps[id] = i > 0 ? [ids[i - 1]] : [];
  });
  const deps = flowReduceDeps(rawDeps, ids);
  const layer = {};
  const visit = (id, guard) => {
    if (layer[id] !== undefined) return layer[id];
    if (guard.has(id)) return 0; // 防御：定义文件异常时不至于死循环
    guard.add(id);
    let l = 0;
    (deps[id] || []).forEach(d => { l = Math.max(l, visit(d, guard) + 1); });
    guard.delete(id);
    layer[id] = l;
    return l;
  };
  ids.forEach(id => visit(id, new Set()));
  const cols = {};
  ids.forEach(id => { const l = layer[id]; (cols[l] = cols[l] || []).push(id); });
  const nCol = Math.max(...Object.keys(cols).map(Number)) + 1;
  const maxRows = Math.max(...Object.values(cols).map(a => a.length));
  const H = maxRows * NH + (maxRows - 1) * GY;
  const pos = {};
  Object.keys(cols).forEach(l => {
    const list = cols[l];
    const colH = list.length * NH + (list.length - 1) * GY;
    const top = (H - colH) / 2;
    list.forEach((id, i) => { pos[id] = { x: Number(l) * (NW + GX), y: top + i * (NH + GY) }; });
  });
  const W = nCol * NW + (nCol - 1) * GX;
  return { pos, deps, rawDeps, W, H, NW, NH };
}

/* mergeFlowStages 合并"工作流定义"与"运行状态"再渲染流程图。
   动机：`run --until X` 创建的 run 其 state.json 只记录到 X（引擎在 resume 时会
   按工作流补种缺失阶段，首次创建不会）——只按状态渲染的话，这类 run 的流程图
   只剩一个节点，后面的阶段明明可续跑却看不见。定义里有、状态里没有的阶段一律
   按"待执行"显示；状态里有、定义里没有的（旧 run / 自定义工作流）附在末尾。 */
function mergeFlowStages(stages, wf) {
  const nodes = (wf && wf.nodes) || [];
  if (!nodes.length) return stages || [];
  const byId = {};
  (stages || []).forEach(s => byId[s.id] = s);
  const out = [];
  nodes.forEach(n => {
    out.push(byId[n.id] || { id: n.id, status: 'pending', attempts: 0, revisions: 0, duration_ms: 0 });
    delete byId[n.id];
  });
  (stages || []).forEach(s => { if (byId[s.id]) out.push(s); });
  return out;
}

/* 节点状态一行文字。skipped 分两种：级联跳过（上游失败）与"上游已重跑需重做" */
function flowStatusText(s) {
  switch (s.status) {
    case 'done': return '✓ 完成' + (s.duration_ms ? ' · ' + fmtMS(s.duration_ms) : '');
    case 'failed': return '✗ 失败';
    case 'running': return '… 运行中';
    case 'skipped': return (s.repair_note || '').includes('上游') ? '⟳ 需重跑' : '⤼ 已跳过';
    default: return '待执行';
  }
}
function isStale(s) { return s.status === 'skipped' && (s.repair_note || '').includes('上游'); }

function flowchartHTML(stages, wf) {
  if (!stages || !stages.length) return `<div class="empty">无阶段数据</div>`;
  const L = flowLayout(stages, wf);
  const byId = {}; stages.forEach(s => byId[s.id] = s);
  let edges = '';
  Object.keys(L.deps).forEach(id => {
    (L.deps[id] || []).forEach(d => {
      const a = L.pos[d], b = L.pos[id];
      if (!a || !b) return;
      const x1 = a.x + L.NW, y1 = a.y + L.NH / 2;
      const x2 = b.x, y2 = b.y + L.NH / 2;
      const dx = Math.max(26, (x2 - x1) * 0.55);
      const cls = byId[d] && byId[d].status === 'done' ? 'e-done' : '';
      edges += `<path class="${cls}" d="M ${x1} ${y1} C ${x1 + dx} ${y1}, ${x2 - dx} ${y2}, ${x2 - 5} ${y2}" marker-end="url(#fc-arrow)"/>`;
    });
  });
  const nodes = stages.map(s => {
    const p = L.pos[s.id];
    if (!p) return '';
    const t = stageTitle(s.id, wf);
    // 画出的线是传递约简后的（画全量会成团），完整依赖清单放 tooltip 里可查
    const deps = (L.rawDeps[s.id] || []).filter(d => d !== s.id);
    const tip = [
      t.full,
      deps.length ? '依赖：' + deps.join('、') : '',
      s.error ? '失败：' + s.error : '',
      s.repair_note || '',
    ].filter(Boolean).join(' · ');
    return `<div class="fc-node ${esc(s.status)}${isStale(s) ? ' stale' : ''}" data-fc="${esc(s.id)}"
        style="left:${p.x}px;top:${p.y}px;width:${L.NW}px;height:${L.NH}px" title="${esc(tip)}">
      <div class="fc-nm">${esc(t.name)}</div>
      <div class="fc-st">${esc(flowStatusText(s))}</div>
      <div class="fc-id">${esc(s.id)}</div>
    </div>`;
  }).join('');
  return `
    <div class="fc-legend">
      <span class="lg done">✓ 已完成</span><span class="lg running">… 运行中</span>
      <span class="lg pending">待执行</span><span class="lg stale">⟳ 需重跑</span>
      <span class="lg failed">✗ 失败</span><span class="lg skipped">⤼ 已跳过</span>
      <span class="muted">点击节点选择要执行的阶段</span>
    </div>
    <div class="fc-wrap"><div class="fc-canvas" style="width:${L.W}px;height:${L.H}px">
      <svg class="fc-svg" width="${L.W}" height="${L.H}" viewBox="0 0 ${L.W} ${L.H}">
        <defs><marker id="fc-arrow" markerWidth="8" markerHeight="8" refX="6" refY="3" orient="auto">
          <path d="M0,0 L6,3 L0,6 Z" fill="#c7c7cc"/></marker></defs>${edges}</svg>
      ${nodes}
    </div></div>`;
}

/* bindFlow 挂交互：点选节点 → 底栏执行动作。
   三个动作与 CLI 的对应关系：
     跑到此          → resume --until <id>          （跳过已完成，停在该阶段）
     重跑此阶段       → resume --from <id> --until <id>（仅该阶段重做；上游未完成的会先补齐）
     重跑此阶段及后续  → resume --from <id>            （该阶段与其下游全部重做） */
function bindFlow(view, run, stages, wf, jobs) {
  const canvas = view.querySelector('.fc-canvas');
  const bar = view.querySelector('#fc-bar');
  if (!canvas || !bar) return;
  const byId = {}; stages.forEach(s => byId[s.id] = s);
  let running = (jobs || []).find(j => j.status === 'running') || null;
  // 任务当前目标阶段（resume 的 until/from 参数）加脉冲，让"正在跑哪一步"一眼可见
  if (running && running.params) {
    const tgt = running.params.until || running.params.from;
    const el = tgt && canvas.querySelector(`.fc-node[data-fc="${tgt}"]`);
    if (el) el.classList.add('running');
  }
  let sel = null;
  const renderBar = () => {
    if (!sel) {
      bar.innerHTML = `<span class="muted">已选：无 —— 点上方节点后可选「跑到此 / 重跑此阶段 / 重跑及后续」</span>`;
      return;
    }
    const s = byId[sel];
    if (!s) { bar.innerHTML = ''; return; }
    const t = stageTitle(sel, wf);
    const busy = !!running;
    const canRun = s.status !== 'done' && s.status !== 'running' && !busy;
    const runTip = canRun ? '续跑流水线并停在该阶段：已完成阶段自动跳过，未完成的按依赖顺序补齐'
      : (busy ? '有任务执行中，请等待完成或取消' : '该阶段已完成；如需重做请用「重跑此阶段」');
    bar.innerHTML = `
      <span class="fc-sel">已选：<b>${esc(t.name)}</b> <span class="mono muted">${esc(sel)}</span>
        ${isStale(s) ? badge('warn', '需重跑') : badge(s.status)}
        ${s.duration_ms ? `<span class="muted">· ${fmtMS(s.duration_ms)}</span>` : ''}
        ${s.attempts > 1 ? `<span class="muted">· 尝试 ${s.attempts}</span>` : ''}
        ${isStale(s) ? `<span class="muted">· 上游已更新，产物可能过期</span>` : ''}
        ${s.repair_note && !isStale(s) ? `<span class="muted" title="${esc(s.repair_note)}">· 💡 ${esc(s.repair_note)}</span>` : ''}</span>
      <span class="flex" style="gap:8px;flex:0 0 auto">
        <button class="btn small" id="fc-run" ${canRun ? '' : 'disabled'} title="${esc(runTip)}">▶ 跑到此</button>
        <button class="btn small" id="fc-rerun" ${busy ? 'disabled' : ''} title="该阶段重置重做；上游未完成的阶段会先自动补齐；不连带下游（后续会标记为需重跑）">↻ 重跑此阶段</button>
        <button class="btn small" id="fc-rerun-down" ${busy ? 'disabled' : ''} title="该阶段与其后所有阶段都重置重做（费用可能较高）">↻↻ 重跑此阶段及后续</button>
      </span>`;
    const bRun = bar.querySelector('#fc-run');
    const bOne = bar.querySelector('#fc-rerun');
    const bDown = bar.querySelector('#fc-rerun-down');
    bRun.onclick = async () => {
      const job = await startJob('resume', run, { until: sel }, {
        confirmText: `续跑流水线并停在「${t.name}」之后：已完成阶段自动跳过，未完成的按依赖顺序补齐。\n产生真实调用时会消耗费用。确认开始？`,
      });
      if (job) route();
    };
    bOne.onclick = async () => {
      const job = await startJob('resume', run, { from: sel, until: sel }, {
        confirmText: `把「${t.name}」重置重做（上游未完成的阶段会先自动补齐），并停在它之后——后续阶段不会自动重做，会标记为“需重跑”。\n确认开始？`,
      });
      if (job) route();
    };
    bDown.onclick = async () => {
      const job = await startJob('resume', run, { from: sel }, {
        confirmText: `把「${t.name}」及其后续所有阶段全部重置重做（付费产物会重新生成，费用可能较高）。\n确认开始？`,
      });
      if (job) route();
    };
  };
  canvas.querySelectorAll('.fc-node').forEach(el => el.onclick = () => {
    sel = el.dataset.fc;
    canvas.querySelectorAll('.fc-node').forEach(x => x.classList.toggle('selected', x === el));
    renderBar();
  });
  renderBar();
}

/* 通用表格：从对象数组自动推断列 */
function autoTable(items, maxCols = 12) {
  if (!items || !items.length) return `<div class="empty">空</div>`;
  const keys = [];
  for (const it of items.slice(0, 5)) {
    Object.keys(it || {}).forEach(k => {
      if (!keys.includes(k) && keys.length < maxCols) keys.push(k);
    });
  }
  const cell = v => {
    if (v == null) return '<span class="muted">-</span>';
    if (typeof v === 'object') return `<span class="mono muted" title="${esc(JSON.stringify(v))}">${esc(trunc(JSON.stringify(v), 60))}</span>`;
    return esc(trunc(v, 90));
  };
  return `<div style="overflow-x:auto"><table class="tbl">
    <tr>${keys.map(k => `<th>${esc(k)}</th>`).join('')}</tr>
    ${items.map(it => `<tr>${keys.map(k => `<td>${cell(it ? it[k] : null)}</td>`).join('')}</tr>`).join('')}
  </table></div>`;
}

/* JSON 语法高亮 */
function prettyJSON(v) {
  let s = JSON.stringify(v, null, 2) || 'null';
  s = esc(s);
  s = s.replace(/(&quot;[^&]*?&quot;)(\s*:)/g, '<span class="j-key">$1</span>$2')
       .replace(/: (&quot;(?:[^&]|&(?!quot;))*?&quot;)/g, ': <span class="j-str">$1</span>')
       .replace(/\b(true|false)\b/g, '<span class="j-bool">$1</span>')
       .replace(/\bnull\b/g, '<span class="j-null">null</span>')
       .replace(/(: )(-?\d+\.?\d*(e[+-]?\d+)?)/gi, '$1<span class="j-num">$2</span>');
  return `<pre class="json">${s}</pre>`;
}

/* 产物智能视图 */
function artifactView(name, data, ctx = null) {
  if (name === 'script' && data && data.scenes) {
    const head = `<div class="mb"><h3 style="margin:4px 0">${esc(data.title || '')}</h3>
      <p class="muted">${esc(data.logline || '')}</p>
      <div class="flex"><span class="tag">${esc(data.genre || '')}</span>${(data.themes || []).map(t => `<span class="tag">${esc(t)}</span>`).join('')}</div></div>`;
    const sceneHtml = (data.scenes || []).map(sc => {
      const kvRows = [
        ['目的', sc.purpose], ['冲突', sc.conflict], ['转折', sc.turn],
      ].filter(r => r[1]);
      const inner = `
        <h4>${esc(sc.id || '')} <span class="tag">${esc(sc.location || '')} · ${esc(sc.time_of_day || '')}</span>
          ${sc.act != null ? `<span class="tag">第 ${esc(sc.act)} 幕</span>` : ''}${sc.tone ? `<span class="tag">${esc(sc.tone)}</span>` : ''}</h4>
        ${kvRows.length ? `<div class="kv">${kvRows.map(([k, v]) => `<span class="k">${k}</span><span>${esc(v)}</span>`).join('')}</div>` : ''}
        ${sc.action ? `<div class="action-text mt">${esc(sc.action)}</div>` : ''}
        ${(sc.dialogue && sc.dialogue.length) ? `<div class="mt">${sc.dialogue.map(l =>
          `<div class="script-line"><span class="who">${esc(l.character || '')}</span><span class="linetext">${esc(l.line || '')}</span></div>`).join('')}</div>` : ''}`;
      // 无素材上下文（其他调用方）保持纯剧本排版；有素材时右侧挂本场景关联素材
      if (!ctx) return `<div class="scene-card">${inner}</div>`;
      const asset = ctx.sceneOf[sc.id];
      const cast = [...(ctx.sceneCast[sc.id] || [])];
      return `<div class="scene-asset-card" style="cursor:default">
        <div class="sa-desc">${inner}</div>
        <div class="sa-assets">
          <div class="sa-title">本场景素材</div>
          ${ctx.running ? `<div class="mt">${badge('running', '素材生成中…')}</div>` : ''}
          ${asset && asset.file && !asset.error
            ? `<img class="sa-scene" loading="lazy" src="${fileUrl(ctx.run, asset.file)}" alt="${esc(sc.location || sc.id)}">`
            : `<div class="sa-scene none">🏞 ${asset && asset.error ? '生成失败' : '待生成'}</div>`}
          ${asset && asset.error ? `<div class="muted" style="font-size:11px;color:var(--err-text)">✗ ${esc(trunc(asset.error, 90))}</div>` : ''}
          ${cast.length ? `<div class="sa-title mt">出场角色（${cast.length}）</div>
          <div class="sa-cast">${cast.map(nm => {
            const ref = ctx.refOf[nm];
            return `<div class="sa-char" title="${esc(nm)}">
              ${ref && ref.path && !ref.error ? `<img loading="lazy" src="${fileUrl(ctx.run, ref.path)}" alt="${esc(nm)}">` : `<div class="sa-char-img">👤</div>`}
              <span>${esc(nm)}</span></div>`;
          }).join('')}</div>`
            : `<div class="muted mt" style="font-size:11.5px">暂无出场角色信息（分镜/生成规格完成后自动补全）</div>`}
        </div>
      </div>`;
    }).join('');
    return head + sceneHtml + (data.log || data.notes ? prettyJSON({ log: data.log, notes: data.notes }) : '');
  }
  if (name === 'characters' && data && data.characters) {
    return data.characters.map(c => `<div class="char-card">
      <h4>👤 ${esc(c.name || '')} ${c.tts_voice ? `<span class="tag">音色 ${esc(c.tts_voice)}</span>` : ''}</h4>
      <div class="kv">
        <span class="k">外形</span><span>${esc(c.appearance || '')}</span>
        <span class="k">服装</span><span>${esc(c.wardrobe || '')}</span>
        <span class="k">声音</span><span>${esc(c.voice || '')}</span>
        ${c.consistency_tokens ? `<span class="k">一致性锚点</span><span class="mono">${esc(c.consistency_tokens)}</span>` : ''}
      </div></div>`).join('') || `<div class="empty">空</div>`;
  }
  if (name === 'voiceover' && data && data.narration) {
    return autoTable(data.narration);
  }
  if (name === 'music' && (data && (data.cue_sheet || data.sound_fx))) {
    let html = '';
    if (data.cue_sheet) html += `<h4 class="mt">Cue Sheet</h4>` + autoTable(data.cue_sheet);
    if (data.sound_fx) html += `<h4 class="mt">音效清单</h4>` + autoTable(data.sound_fx);
    return html + (data.notes ? prettyJSON(data.notes) : '');
  }
  if ((name === 'storyboard' || name === 'shotspec') && data) {
    const arr = data.shots || data.tasks || data.scenes || null;
    if (arr) return `<h4 class="mt">${esc(data.style ? '风格：' + (data.style.name || '') : '')}</h4>` + autoTable(arr);
  }
  return prettyJSON(data);
}

/* ================= 页面：总览 ================= */
async function pageDashboard(view) {
  const ov = await api('/api/overview');
  const sys = ov.system;
  const sysBadges = sys.ffmpeg_ok ? badge('ok', 'ffmpeg 就绪') : badge('failed', 'ffmpeg 缺失');
  const modeBadge = sys.mock ? badge('mock', '离线演示模式') : badge('ok', '真实模型');
  view.innerHTML = `
    <div class="cards">
      ${statCard('项目总数', ov.runs_total, `已完成渲染 ${ov.runs_complete} · 已交付 ${ov.runs_reported}`)}
      ${statCard('执行中任务', (state.jobs || []).filter(j => j.status === 'running').length, '顶栏可查看任务监视器')}
      ${statCard('模型模式', modeBadge, esc(trunc(sys.config_summary || sys.config_error || '', 46)))}
      ${statCard('工具链', sysBadges, sys.ffmpeg_ok ? esc(sys.version || '') : esc((sys.warnings || []).join('；')))}
    </div>
    <div class="grid2">
      <div>
        <div class="panel">
          <h3>🎬 开始一部新片</h3>
          <div class="form-item" style="width:100%">
            <label>创作意图 / 故事梗概（brief）</label>
            <textarea id="new-brief" rows="3" style="width:100%" placeholder="例：一部 2 分钟科幻短片：深空信使收到来自地球麦田的神秘信号，踏上归途。"></textarea>
          </div>
          <div class="form-row mt">
            <div class="form-item"><label>工作流</label>
              <select id="new-workflow" class="form-el"></select></div>
            <div class="form-item"><label>跑到哪一步停下（--until，省钱用法）</label>
              <select id="new-until" class="form-el">
                <option value="">全部阶段（一路跑完）</option>
                <option value="script">script · 剧本后停下</option>
                <option value="characters">characters · 角色圣经后停下</option>
                <option value="assets">assets · 素材库后停下（推荐）</option>
                <option value="storyboard">storyboard · 分镜后停下</option>
                <option value="voiceover">voiceover · 台词后停下</option>
                <option value="shotspec">shotspec · 生成规格后停下</option>
              </select></div>
            <button class="btn primary" id="new-run-btn">开始创作</button>
          </div>
          <div class="muted" style="font-size:12px">未配置 API Key 时自动进入离线演示模式：会完整走通流水线，但产出占位素材。</div>
        </div>
        <div class="panel"><h3>🗂 最近项目</h3><div id="ov-runs"></div></div>
      </div>
      <div>
        <div class="panel"><h3>⚙ 最近任务</h3><div id="ov-jobs"></div></div>
        <div class="panel"><h3>💡 提示</h3>
          <div class="muted" style="line-height:2">
            · 建议用 <b>--until</b> 分段确认剧本与分镜后再继续，最省钱<br>
            · 渲染前先 <b>成本预估（dry-run）</b>，再 <b>仅出首帧</b> 拼动态分镜看节奏<br>
            · 节奏成立后才为视频付钱；不完整可断点续跑，已付费产物会复用<br>
            · 交付后到「质检复核」做视觉复核与打磨闭环
          </div></div>
      </div>
    </div>`;

  // 工作流下拉
  try {
    const meta = await api('/api/meta');
    $('#new-workflow').innerHTML = (meta.workflows || []).map(w =>
      `<option value="${esc(w.file)}">${esc(w.name || w.file)}</option>`).join('') || '<option value="workflows/movie.json">默认工作流</option>';
  } catch { /* 忽略 */ }

  $('#new-run-btn').onclick = async () => {
    const brief = $('#new-brief').value.trim();
    if (!brief) { toast('请填写创作意图', 'warn'); return; }
    const job = await startJob('run', '', {
      brief, until: $('#new-until').value, workflow: $('#new-workflow').value,
    }, { confirmText: '开始创作会调用文本模型（产生少量费用）。确认提交？' });
    if (job) { $('#new-brief').value = ''; setTimeout(loadRuns, 1200); }
  };

  const runsHTML = ov.recent_runs && ov.recent_runs.length
    ? `<table class="tbl"><tr><th>项目</th><th>阶段</th><th>渲染</th><th>更新</th><th></th></tr>` +
      ov.recent_runs.map(r => `<tr>
        <td><div class="mono">${esc(r.id)}</div><div class="muted" style="max-width:260px">${esc(trunc(r.brief, 40))}</div></td>
        <td>${r.stage_done}/${r.stage_total}${r.stage_fail ? ` <span class="badge b-failed">失败${r.stage_fail}</span>` : ''}</td>
        <td>${r.complete === null ? badge('none', '未渲染') : r.complete ? badge('done', '完整') : badge('warn', `缺镜头 ${r.shots_done}/${r.shots_total}`)}</td>
        <td class="muted">${fmtAgo(r.updated_at)}</td>
        <td><button class="btn small" data-open="${esc(r.id)}">打开</button></td>
      </tr>`).join('') + `</table>`
    : `<div class="empty"><div class="big">🌌</div>还没有项目。写下一个 brief，开始第一部片子。</div>`;
  $('#ov-runs').innerHTML = runsHTML;
  $('#ov-runs').querySelectorAll('[data-open]').forEach(b => b.onclick = () => {
    state.run = b.dataset.open; localStorage.setItem('studio.run', state.run);
    $('#run-select').value = state.run; loadRuns(); location.hash = '#/creative'; route();
  });

  const jobs = ov.jobs || [];
  $('#ov-jobs').innerHTML = jobs.length
    ? `<table class="tbl"><tr><th>状态</th><th>任务</th><th>开始</th><th></th></tr>` +
      jobs.map(j => `<tr><td>${badge(j.status)}</td><td>${esc(j.label)}</td>
        <td class="muted">${fmtAgo(j.started_at)}</td>
        <td><button class="btn small" data-log="${j.id}" data-label="${esc(j.label)}">日志</button></td></tr>`).join('') +
      `</table>`
    : `<div class="empty"><div class="big">🛋</div>还没有任务</div>`;
  $('#ov-jobs').querySelectorAll('[data-log]').forEach(b => b.onclick = () => openLogModal(b.dataset.log, b.dataset.label));
}

/* ================= 页面：创作中心 ================= */
async function pageCreative(view) {
  const run = pickRun();
  if (!run) { view.innerHTML = needRunHTML('创作中心展示剧本 / 角色圣经 / 分镜 / 台词等创作产物'); return; }
  const [d, assetsRes, scriptRes, ksRes] = await Promise.all([
    api('/api/runs/' + encodeURIComponent(run)),
    api(`/api/runs/${encodeURIComponent(run)}/assets`).catch(() => ({ index: null })),
    api(`/api/runs/${encodeURIComponent(run)}/artifacts/script`).catch(() => null),
    api(`/api/runs/${encodeURIComponent(run)}/artifacts/shotspec`).catch(() => null),
  ]);
  const arts = (d.artifacts || []).filter(f => f.kind === 'doc' && f.name.endsWith('.json'));
  const artNames = arts.map(f => f.name.replace(/\.json$/, ''));

  // 素材索引：场景图按 scene_id、角色参考帧按名字挂到剧本场景上（只取主帧）
  const ix = assetsRes.index || { characters: [], scenes: [] };
  const sceneOf = {};
  (ix.scenes || []).forEach(s => {
    if (!s.variant && !sceneOf[s.scene_id]) sceneOf[s.scene_id] = s; // 主基准（同场地多场次共享同一文件）
  });
  const refOf = {};
  (ix.characters || []).forEach(r => {
    const k = r.name || r.token;
    if (!r.variant || !refOf[k]) refOf[k] = r; // 主锚点帧优先：渲染锁脸用的就是它
  });

  // 场景→出场角色：shotspec 逐镜角色表优先，未到 shotspec 时回落台词说话人
  const scenes = (scriptRes && scriptRes.data && scriptRes.data.scenes) || [];
  let sceneCast = {};
  ((ksRes && ksRes.data && ksRes.data.shots) || []).forEach(sh => {
    if (!sh.scene_id) return;
    if (!sceneCast[sh.scene_id]) sceneCast[sh.scene_id] = new Set();
    (sh.characters || []).forEach(c => sceneCast[sh.scene_id].add(c));
  });
  scenes.forEach(sc => {
    if (!sceneCast[sc.id]) {
      const set = new Set();
      (sc.dialogue || []).forEach(l => { if (l.character && l.character !== 'narrator') set.add(l.character); });
      if (set.size) sceneCast[sc.id] = set;
    }
  });

  const assetsRunning = (state.jobs || []).some(j => j.type === 'assets' && j.run_id === run && j.status === 'running');
  const scenesReady = Object.values(sceneOf).filter(s => s.file && !s.error).length;
  const refsReady = Object.values(refOf).filter(r => r.path && !r.error).length;
  const assetStatus = assetsRunning ? '⚙ 素材生成中…'
    : (scenesReady || refsReady)
      ? `已生成 ${scenesReady}/${scenes.length} 场景图 · ${refsReady}/${(ix.characters || []).length} 角色参考帧`
      : '尚未生成素材';
  // 流程图渲染用的阶段列表：工作流定义 ∪ 运行状态（--until 创建的 run 状态不全）
  const flowStages = mergeFlowStages(d.stages, d.workflow);

  view.innerHTML = `
    <div class="panel">
      <div class="panel-head"><span>📝 创作意图</span>
        <span class="sub">${esc(d.summary.workflow || '')} ${d.mock ? badge('mock', '离线演示') : ''} · 更新于 ${fmtAgo(d.summary.updated_at)}</span></div>
      <p style="margin:0 0 12px;font-size:15px;line-height:1.8">${esc(d.summary.brief || '（无 brief）')}</p>
      <div class="flex">
        <button class="btn" id="btn-resume">▶ 断点续跑（跳过已完成阶段）</button>
        <button class="btn primary" id="btn-assets">🖼 生成素材（整个故事）</button>
        <button class="btn" id="btn-verify-report">📋 查看交付报告</button>
        <a class="btn" href="#/render">前往渲染 →</a>
      </div>
      <div class="muted" style="font-size:12px;margin-top:8px">${assetStatus} · 一键梳理整个故事，生成全部角色参考帧与场景图，生成后剧本场景右侧直接预览${artNames.includes('characters') ? '' : ' · <b>缺角色圣经</b>：在下图点选 characters →「跑到此」（仅一条文本调用的费用），完成后再生成素材'}</div>
    </div>
    <div class="panel">
      <div class="panel-head"><span>🧭 工作流阶段</span><span class="sub">点节点选择执行；失败阶段可通过「系统设置 → 环境自检」获得 AI 诊断</span></div>
      ${flowchartHTML(flowStages, d.workflow)}
      <div class="fc-bar" id="fc-bar"></div>
      ${d.stages && d.stages.some(s => s.error) ? `<div class="mt">${d.stages.filter(s => s.error).map(s =>
        `<div class="scene-card" style="border-left-color:var(--err)"><b>${esc(s.id)}</b> <span class="badge b-failed">${esc(s.status)}</span>
         <div class="muted" style="margin-top:6px">${esc(s.error)}</div>
         ${s.repair_note ? `<div style="margin-top:6px">💡 ${esc(s.repair_note)}</div>` : ''}</div>`).join('')}</div>` : ''}
    </div>
    <div class="panel">
      <div class="panel-head"><span>📚 创作产物</span><span class="sub">点击标签查看（script=剧本 characters=角色圣经 storyboard=分镜 voiceover=台词 shotspec=生成规格）</span></div>
      ${artNames.length ? `<div class="tabs">${artNames.map((n, i) =>
        `<div class="tab ${i === 0 ? 'active' : ''}" data-art="${esc(n)}">${esc(n)}</div>`).join('')}</div>
        <div id="art-view"><div class="loading">加载中…</div></div>`
      : `<div class="empty"><div class="big">📜</div>还没有创作产物。等流水线跑完 script 阶段后回来。</div>`}
    </div>`;

  $('#btn-resume').onclick = () => {
    startJob('resume', run, {}, {
      confirmText: '断点续跑会继续未完成的阶段直到全部完成（可能产生费用）。确认？',
    });
  };
  $('#btn-assets').onclick = async () => {
    const job = await startJob('assets', run, {}, {
      confirmText: 'AI 将基于剧本规划素材清单（反复出场角色加角度变体、同地点同时段场景共用一张），再逐张出图，费用随规划浮动。确认开始？',
    });
    if (job) route(); // 立即呈现任务状态
  };
  $('#btn-verify-report').onclick = () => { location.hash = '#/delivery'; route(); };
  if (location.hash === '#/creative') { /* 保持 */ }
  // 流程图交互：点节点 → 底栏出现「跑到此 / 重跑此阶段 / 重跑及后续」
  bindFlow(view, run, flowStages, d.workflow, d.jobs);

  const showArt = async name => {
    const box = $('#art-view');
    if (!box) return;
    box.innerHTML = '<div class="loading">加载中…</div>';
    try {
      const art = await api(`/api/runs/${encodeURIComponent(run)}/artifacts/${encodeURIComponent(name)}`);
      box.innerHTML = artifactView(name, art.data, { run, sceneOf, refOf, sceneCast, running: assetsRunning });
    } catch (e) { box.innerHTML = `<div class="empty">加载失败：${esc(e.message)}</div>`; }
  };
  view.querySelectorAll('.tab[data-art]').forEach(t => t.onclick = () => {
    view.querySelectorAll('.tab[data-art]').forEach(x => x.classList.remove('active'));
    t.classList.add('active');
    showArt(t.dataset.art);
  });
  if (artNames.length) showArt(artNames[0]);
}

/* ================= 页面：素材库 ================= */
async function pageAssets(view) {
  const run = pickRun();
  if (!run) { view.innerHTML = needRunHTML('素材库：基于剧本生成角色参考帧与场景美术基准图，用于跨镜头一致性'); return; }
  const [d, assetsRes] = await Promise.all([
    api('/api/runs/' + encodeURIComponent(run)),
    api(`/api/runs/${encodeURIComponent(run)}/assets`).catch(() => ({ index: null })),
  ]);
  const ix = assetsRes.index || { characters: [], scenes: [] };

  // 角色圣经（characters.json）与素材索引按名字合并
  let bibleChars = [];
  try {
    const c = await api(`/api/runs/${encodeURIComponent(run)}/artifacts/characters`);
    bibleChars = (c.data && c.data.characters) || [];
  } catch { /* 还没跑到 characters 阶段 */ }
  const refOf = {};
  const charVariants = {};
  (ix.characters || []).forEach(r => {
    const k = r.name || r.token;
    if (!r.variant) {
      if (!refOf[k]) refOf[k] = r; // 主锚点帧优先：渲染锁脸用的就是它
    } else {
      (charVariants[k] = charVariants[k] || []).push(r); // 角色变体帧（侧面/换装/状态…）
    }
  });

  // 场景（script.json）与素材索引按 id 合并；保留完整文字内容（描述+台词）供阅读
  let scenes = [];
  try {
    const sc = await api(`/api/runs/${encodeURIComponent(run)}/artifacts/script`);
    scenes = ((sc.data && sc.data.scenes) || []).map(s => ({
      id: s.id, location: s.location, time_of_day: s.time_of_day, tone: s.tone,
      purpose: s.purpose || '', conflict: s.conflict || '', turn: s.turn || '',
      action: s.action || s.conflict || s.turn || '',
      dialogue: s.dialogue || [],
    }));
  } catch { /* 还没跑到 script 阶段 */ }
  const sceneOf = {};
  const sceneVariants = {};
  (ix.scenes || []).forEach(s => {
    if (!s.variant) {
      if (!sceneOf[s.scene_id]) sceneOf[s.scene_id] = s; // 主基准（同场地多场次共享同一文件）
    } else {
      (sceneVariants[s.scene_id] = sceneVariants[s.scene_id] || []).push(s); // 状态变体（雨夜/被毁…）
    }
  });

  // 场景→出场角色：shotspec 有逐镜角色表（精确）；未到 shotspec 时回落台词说话人
  let sceneCast = {};
  try {
    const ks = await api(`/api/runs/${encodeURIComponent(run)}/artifacts/shotspec`);
    ((ks.data && ks.data.shots) || []).forEach(sh => {
      if (!sh.scene_id) return;
      if (!sceneCast[sh.scene_id]) sceneCast[sh.scene_id] = new Set();
      (sh.characters || []).forEach(c => sceneCast[sh.scene_id].add(c));
    });
  } catch { /* 未到 shotspec 阶段 */ }
  scenes.forEach(sc => {
    if (!sceneCast[sc.id]) {
      const set = new Set();
      (sc.dialogue || []).forEach(l => { if (l.character && l.character !== 'narrator') set.add(l.character); });
      if (set.size) sceneCast[sc.id] = set;
    }
  });

  const refsReady = Object.values(refOf).filter(r => r.path && !r.error).length;
  const scenesReady = Object.values(sceneOf).filter(s => s.file && !s.error).length;

  view.innerHTML = `
    <div class="panel">
      <div class="panel-head"><span>🖼 素材库操作台</span>
        <span class="sub">角色参考帧与渲染管线同源（图生图锁脸）· 场景图为场次美术基准</span></div>
      <div class="cards">
        ${statCard('角色参考帧', refsReady ? `${refsReady}/${bibleChars.length || refsReady}` : '未生成',
          '渲染时自动复用，跨镜头同一张脸')}
        ${statCard('场景 establishing 图', scenesReady ? `${scenesReady}/${scenes.length || scenesReady}` : '未生成',
          '场次级环境/光线/情绪定调')}
        ${statCard('索引更新', ix.updated_at ? fmtAgo(ix.updated_at) : '-',
          `output/assets/index.json`)}
      </div>
      <div class="form-row">
        <button class="btn primary" id="a-all">🖼 生成全部素材</button>
        <button class="btn" id="a-chars">🧑‍🎨 仅角色参考帧</button>
        <button class="btn" id="a-scenes">🏞 仅场景图</button>
        <button class="btn danger" id="a-force">♻ 强制全部重出（改变角色长相）</button>
      </div>
      <div class="muted" style="font-size:12px">
        生成顺序建议：创作流水线完成 shotspec 后先建素材库，核对参考帧与场景定调，再进渲染。
        重出单个角色/场景用下方卡片上的按钮（成本最小返修）。
      </div>
    </div>

    <div class="panel">
      <div class="panel-head"><span>🧑‍🎨 角色一致性素材（${bibleChars.length}）</span>
        <span class="sub">参考帧 = 全片一致性的地基；质检结论 ${badge('pass', 'pass')} ${badge('warning', 'corrected/failed')} ${badge('unknown', 'skipped=未检查')}</span></div>
      ${!bibleChars.length ? `<div class="empty"><div class="big">👤</div>还没有角色数据。先在创作中心完成 characters 阶段。</div>`
        : `<div class="ref-grid" style="grid-template-columns:repeat(auto-fill,minmax(230px,1fr))">
        ${bibleChars.map(c => {
          const ref = refOf[c.name];
          // 结论徽标：有定稿结论用结论；有链路但未定稿（被取消/核对未完成）如实标"审核未定稿"；
          // 无链路无错误才是"未质检"——绝不把"没查完"显示成"通过"
          const qc = ref ? (ref.qc_status || (ref.error ? 'failed' : ((ref.qc_review || []).length ? 'pending' : 'unknown'))) : '';
          const qcText = { unknown: '未质检', pending: '审核未定稿' }[qc] || qc;
          return `<div class="ref-card">
            ${ref && ref.path
              ? `<img loading="lazy" src="${fileUrl(run, ref.path)}" alt="${esc(c.name)}"
                  onerror="this.onerror=null;this.outerHTML='<div class=&quot;shot-thumb none&quot; style=&quot;aspect-ratio:3/4&quot;>👤</div>'">`
              : `<div class="shot-thumb none" style="aspect-ratio:3/4">👤</div>`}
            <div class="ref-meta">
              <div class="ref-name" title="${esc(c.name)}">👤 ${esc(c.name)} ${c.tts_voice ? `<span class="tag">音色 ${esc(c.tts_voice)}</span>` : ''}</div>
              <div class="muted" style="margin:4px 0;font-size:11px;line-height:1.6">${esc(trunc(c.appearance, 60))}</div>
              ${c.consistency_tokens ? `<div class="mono muted" style="font-size:10.5px;word-break:break-all" title="${esc(c.consistency_tokens)}">${esc(trunc(c.consistency_tokens, 64))}</div>` : ''}
              <div class="flex mt" style="gap:6px;justify-content:space-between">
                ${qc ? badge(qc, qcText) : badge('none', '未生成')}
                <button class="btn small" data-char="${esc(c.name)}" ${ref && ref.path ? '' : ''}>${ref && ref.path ? '♻ 重出' : '生成'}</button>
              </div>
              ${(charVariants[c.name] || []).length ? `<div class="sa-cast" style="margin-top:8px">${charVariants[c.name].map(v =>
                `<div class="sa-char" title="${esc(v.purpose || v.variant)}">${v.path && !v.error
                  ? `<img loading="lazy" src="${fileUrl(run, v.path)}" alt="${esc(v.variant)}">`
                  : `<div class="sa-char-img">✗</div>`}<span>${esc(v.variant)}</span></div>`).join('')}</div>` : ''}
              ${ref && ref.error ? `<div class="muted mt" style="font-size:11px;color:var(--err-text)">✗ ${esc(trunc(ref.error, 80))}</div>` : ''}
              ${ref && ref.qc_issues && ref.qc_issues.length ? `<div class="muted mt" style="font-size:11px">⚠ ${esc(ref.qc_issues.join('；'))}</div>` : ''}
              ${ref && (ref.path || (ref.qc_review || []).length) ? `<div class="flex mt spread" style="gap:6px">
                <span class="muted" style="font-size:11px">🔍 ${esc((ref.qc_review || []).length ? qcReviewSummary(ref) : '暂无审核记录')}</span>
                <button class="btn small" data-qc="${esc(c.name)}">审核链路</button>
              </div>` : ''}
            </div></div>`;
        }).join('')}</div>`}
    </div>

    <div class="panel">
      <div class="panel-head"><span>🏞 场景描述 × 素材（${scenes.length}）</span>
        <span class="sub">左侧：剧本场景描述（点击看完整文字与台词）；右侧：本场景用到的素材（场景图 + 出场角色参考帧）</span></div>
      ${!scenes.length ? `<div class="empty"><div class="big">🏞</div>还没有场次数据。先在创作中心完成 script 阶段。</div>`
        : `<div class="scene-asset-grid">
        ${scenes.map((sc, si) => {
          const asset = sceneOf[sc.id];
          const cast = [...(sceneCast[sc.id] || [])];
          const dlgCount = (sc.dialogue || []).length;
          return `<div class="scene-asset-card" data-open-scene="${si}">
          <div class="sa-desc">
            <div class="flex" style="gap:6px">
              <span class="mono">${esc(sc.id)}</span><b>${esc(sc.location || '')}</b>
              ${sc.time_of_day ? `<span class="tag">${esc(sc.time_of_day)}</span>` : ''}
              ${sc.tone ? `<span class="tag">${esc(sc.tone)}</span>` : ''}
              ${dlgCount ? `<span class="tag">${dlgCount} 句台词</span>` : ''}
            </div>
            <div class="muted clamp6 action-text" style="margin-top:10px">${esc(sc.action) || '（无描述）'}</div>
            ${sc.purpose ? `<div class="muted" style="margin-top:8px;font-size:12px"><b>目的</b> ${esc(sc.purpose)}</div>` : ''}
            <div class="flex mt" style="gap:6px">
              ${asset && asset.file ? badge('done', '场景图已生成') : asset && asset.error ? badge('failed', '失败') : badge('none', '场景图未生成')}
              <button class="btn small" data-scene="${esc(sc.id)}">${asset && asset.file ? '♻ 重出场景图' : '生成场景图'}</button>
            </div>
            ${asset && asset.error ? `<div class="muted mt" style="font-size:11px;color:var(--err-text)">✗ ${esc(trunc(asset.error, 90))}</div>` : ''}
          </div>
          <div class="sa-assets">
            <div class="sa-title">本场景素材</div>
            ${asset && asset.file && !asset.error
              ? `<img class="sa-scene" loading="lazy" src="${fileUrl(run, asset.file)}" alt="${esc(sc.location)}">`
              : `<div class="sa-scene none">🏞 待生成</div>`}
            ${(sceneVariants[sc.id] || []).length ? `<div class="sa-title mt">状态变体</div>
            <div class="sa-cast">${sceneVariants[sc.id].map(v =>
              `<div class="sa-char" title="${esc(v.purpose || v.variant)}">${v.file && !v.error
                ? `<img loading="lazy" src="${fileUrl(run, v.file)}" alt="${esc(v.variant)}">`
                : `<div class="sa-char-img">✗</div>`}<span>${esc(v.variant)}</span></div>`).join('')}</div>` : ''}
            ${cast.length ? `<div class="sa-title mt">出场角色（${cast.length}）</div>
            <div class="sa-cast">${cast.map(name => {
              const ref = refOf[name];
              return `<div class="sa-char" title="${esc(name)}${ref && ref.qc_status ? ' · ' + ref.qc_status : ''}">
                ${ref && ref.path && !ref.error ? `<img loading="lazy" src="${fileUrl(run, ref.path)}" alt="${esc(name)}">` : `<div class="sa-char-img">👤</div>`}
                <span>${esc(name)}</span></div>`;
            }).join('')}</div>`
            : `<div class="muted mt" style="font-size:11.5px">暂无出场角色信息（分镜/生成规格完成后自动补全）</div>`}
          </div>
        </div>`;
        }).join('')}</div>`}
    </div>`;

  $('#a-all').onclick = () => startJob('assets', run, {}, {
    confirmText: 'AI 将基于剧本规划素材清单（反复出场角色加角度变体、同地点同时段场景共用一张），再逐张出图，费用随规划浮动。确认开始？',
  });
  $('#a-chars').onclick = () => startJob('assets', run, { no_scenes: true }, {
    confirmText: '生成全部角色参考帧（AI 按剧本规划角度变体，base 主锚点帧有质检核对）。确认？',
  });
  $('#a-scenes').onclick = () => startJob('assets', run, { no_chars: true }, {
    confirmText: '生成全部场景 establishing 图（同地点同时段归并共用一张）。确认？',
  });
  $('#a-force').onclick = () => startJob('assets', run, { force: true }, {
    confirmText: '强制重出会重新生成角色参考帧——全片角色长相将改变。确认？',
  });
  view.querySelectorAll('[data-char]').forEach(b => b.onclick = () => {
    openRefPromptModal(run, b.dataset.char, !!refOf[b.dataset.char] && refOf[b.dataset.char].path);
  });
  // 审核链路：逐轮候选图 + 核对结论（哪张被否、改了什么、几轮通过）
  view.querySelectorAll('[data-qc]').forEach(b => b.onclick = () => {
    const r = refOf[b.dataset.qc];
    if (r) qcReviewModal(run, r);
  });

  // 编辑提示词重出：展示该角色每一帧当前实际会发送的提示词，改完提交覆盖
  async function openRefPromptModal(run, name, isRegen) {
    let d;
    try {
      d = await api(`/api/runs/${encodeURIComponent(run)}/ref-prompt?char=${encodeURIComponent(name)}`);
    } catch (e) { toast('加载提示词失败：' + e.message, 'err'); return; }
    const frames = d.frames || [];
    if (!frames.length) { toast('没有可编辑的帧', 'warn'); return; }
    const body = `
      <div class="muted mb" style="font-size:12px">以下每帧提示词 = 当前实际会发送的内容（人工编辑过则以编辑版为准）。${isRegen ? '重出会改变该角色在全片的长相。' : ''}
      提示：改动若与角色圣经冲突，质检仍按圣经核对，可能仍标 failed——建议同步修正角色圣经。</div>` +
      frames.map((f, i) => `<div class="mt" ${i ? 'style="margin-top:14px"' : ''}>
        <div class="flex" style="justify-content:space-between">
          <b>${f.variant === 'base' || !f.variant ? '基准帧' : esc(f.variant)}</b>
          <span class="muted" style="font-size:11px">${esc(f.purpose || '')}</span></div>
        <textarea class="form-el mono" data-frame="${i}" rows="${f.variant === 'base' || !f.variant ? 9 : 7}" style="width:100%;font-size:12px">${esc(f.prompt || '')}</textarea></div>`).join('');
    const mask = openModal(`✏ 编辑提示词 · ${esc(name)}`, body,
      `<button class="btn primary" id="refp-go">♻ 用此提示词重出</button>`, '840px');
    mask.querySelector('#refp-go').onclick = () => {
      const edited = frames.map((f, i) => ({
        variant: f.variant || 'base',
        prompt: mask.querySelector(`textarea[data-frame="${i}"]`).value,
        purpose: f.purpose || '',
      }));
      closeModal(mask);
      startJob('assets', run, {
        chars: name,
        force: !!isRegen,
        prompt_override: { characters: [{ name, frames: edited }] },
      }, {
        confirmText: isRegen ? `用编辑后的提示词重出「${name}」的参考帧——该角色全片长相将改变。确认？` : undefined,
      });
    };
  }
  view.querySelectorAll('[data-scene]').forEach(b => b.onclick = () => {
    startJob('assets', run, { scenes: b.dataset.scene, force: true });
  });
  // 点卡片看完整场景文字（描述 + 台词 + 出图提示词）；按钮点击不触发
  view.querySelectorAll('[data-open-scene]').forEach(card => {
    card.addEventListener('click', e => {
      if (e.target.closest('button')) return;
      const sc = scenes[+card.dataset.openScene];
      openSceneModal(run, sc, sceneOf[sc.id]);
    });
  });
}

function openSceneModal(run, sc, asset) {
  if (!sc) return;
  const kvRows = [['目的', sc.purpose], ['冲突', sc.conflict], ['转折', sc.turn]].filter(r => r[1]);
  openModal(`场次 ${esc(sc.id)} · ${esc(sc.location || '')}`,
    `<div class="flex mb" style="gap:6px">
      ${sc.time_of_day ? `<span class="tag">${esc(sc.time_of_day)}</span>` : ''}
      ${sc.tone ? `<span class="tag">${esc(sc.tone)}</span>` : ''}
      ${asset && asset.model ? `<span class="tag mono">图 ${esc(asset.model)}</span>` : ''}
      ${asset && asset.seed ? `<span class="tag mono">种子 ${esc(asset.seed)}</span>` : ''}
    </div>
    ${kvRows.length ? `<div class="kv mb">${kvRows.map(([k, v]) => `<span class="k">${k}</span><span>${esc(v)}</span>`).join('')}</div>` : ''}
    ${sc.action ? `<div class="action-text">${esc(sc.action)}</div>` : '<div class="muted">无动作描述</div>'}
    ${(sc.dialogue && sc.dialogue.length) ? `<h4 class="mt">台词（${sc.dialogue.length} 句）</h4>${sc.dialogue.map(l =>
      `<div class="script-line"><span class="who">${esc(l.character || '')}</span><span class="linetext">${esc(l.line || '')}</span></div>`).join('')}` : ''}
    ${asset && asset.prompt ? `<h4 class="mt">场景图提示词</h4><pre class="json">${esc(asset.prompt)}</pre>` : ''}`,
    `<button class="btn" id="scm-regen">♻ 重出此场景图</button>`, '760px');
  const mask = modalStack[modalStack.length - 1];
  mask.querySelector('#scm-regen').onclick = () => startJob('assets', run, { scenes: sc.id, force: true });
}

/* ================= 页面：渲染生成 ================= */
async function pageRender(view) {
  const run = pickRun();
  if (!run) { view.innerHTML = needRunHTML('渲染生成：逐镜头出图出视频、质检与定点重渲'); return; }
  const d = await api('/api/runs/' + encodeURIComponent(run));
  const man = d.manifest;
  const outputs = d.outputs || [];
  const hasAnimatic = outputs.some(f => f.path === 'output/animatic.mp4');
  const outputsHaveShots = outputs.filter(f => f.path.startsWith('output/shots/') && f.kind === 'image' && !f.path.includes('invalid'));

  // 镜头列表：优先渲染清单；否则从产物推断
  let shots = [];
  if (man && man.shots && man.shots.length) {
    shots = man.shots.map(s => ({
      id: s.id, status: s.status, dur: s.actual_duration_sec, seed: s.seed,
      model: s.model, attempts: s.attempts, video: s.video, image: s.image,
      rounds: s.rounds || [], night: s.night,
    }));
  } else {
    const ids = [...new Set(outputsHaveShots.map(f => f.path.match(/(sh\d+)/)?.[1]).filter(Boolean))];
    shots = ids.map(id => ({ id, status: 'unknown', video: `output/shots/${id}.mp4`, image: `output/shots/${id}.png` }));
  }
  // 渲染清单按调度优先级存储，直接展示会乱序；界面按镜头号自然排序
  shots.sort((a, b) => a.id.localeCompare(b.id, undefined, { numeric: true }));

  view.innerHTML = `
    <div class="panel">
      <div class="panel-head"><span>🎛 渲染操作台</span><span class="sub">顺序建议：成本预估 → 仅出首帧（拼动态分镜看节奏）→ 完整渲染</span></div>
      <div class="form-row">
        <button class="btn" id="r-dryrun">💰 成本预估（dry-run，不花钱）</button>
        <button class="btn" id="r-images">🖼 仅出首帧 + 拼动态分镜</button>
        <button class="btn primary" id="r-full">🎬 完整渲染（付费）</button>
        <button class="btn danger" id="r-forceref">♻ 重新生成参考帧（改变全片长相）</button>
      </div>
      <div class="form-row">
        <div class="form-item"><label>定点重渲（镜头 ID，逗号分隔）</label>
          <input type="text" id="r-shots" placeholder="sh003,sh007" style="width:180px"></div>
        <button class="btn" id="r-repair">🔧 定点重渲</button>
        <div class="form-item"><label>画质</label>
          <select id="r-quality" class="form-el"><option value="">默认</option><option value="quality">quality 画质优先</option><option value="speed">speed 成本优先</option></select></div>
        <div class="form-item"><label>并发 workers</label>
          <input type="number" id="r-workers" min="1" max="8" style="width:80px" placeholder="默认"></div>
        <div class="form-item"><label>单镜超时(分)</label>
          <input type="number" id="r-timeout" min="1" style="width:90px" placeholder="默认"></div>
      </div>
      <div class="form-row">
        <div class="form-item"><label>多 Take 对比（镜头 ID）</label>
          <input type="text" id="r-takeshots" placeholder="sh003" style="width:120px"></div>
        <div class="form-item"><label>选定 Take</label>
          <select id="r-pick" class="form-el"><option value="">仅对比不切换</option><option value="A">A · 恢复旧版</option><option value="B">B · 采用新版</option></select></div>
        <button class="btn" id="r-takes">🎥 生成对比 Take</button>
        ${hasAnimatic ? `<button class="btn" id="r-play-animatic">▶ 播放动态分镜</button>` : ''}
      </div>
    </div>

    <div class="cards">
      ${statCard('渲染状态', man ? (man.complete ? badge('done', '完整') : badge('warn', '不完整')) : badge('none', '未渲染'),
        man && man.missing && man.missing.length ? `缺失：${esc(man.missing.join(','))}` : (man ? '' : '先在创作中心完成 shotspec'))}
      ${statCard('镜头', `${man ? man.count_done ?? shots.filter(s => s.status === 'done').length : 0}/${shots.length}`,
        man && man.rerendered && man.rerendered.length ? `换种重渲：${esc(man.rerendered.join(','))}` : '')}
      ${statCard('时长', man ? `${fmtSec(man.actual_total_duration_sec)} <span class="muted" style="font-size:12px">/ 计划 ${fmtSec(man.plan_total_duration_sec)}</span>` : '-',
        man && man.imaged_shots ? `${man.imaged_shots} 镜仅出首帧` : '')}
      ${statCard('花费', man ? `¥${(man.cost.actual_cny || 0).toFixed(2)}` : '-', man ? `画质 ${esc(man.quality || '-')}` : '')}
    </div>

    ${d.style ? `<div class="panel"><div class="panel-head"><span>🎨 全片风格（由代码固化）</span><span class="sub">${esc(d.style.aspect || '')}</span></div>
      <div class="kv">
        <span class="k">预设</span><span>${esc(d.style.name || '')}</span>
        <span class="k">机身</span><span>${esc(d.style.camera?.name || '')} · ${esc(d.style.camera?.film_stock || '')}</span>
        <span class="k">调色</span><span>${esc(d.style.grade?.palette || '')}</span>
        <span class="k">颗粒</span><span>${esc(d.style.grade?.grain || '')}</span>
        <span class="k">时段</span><span>${esc(d.style.time_of_day || '')}</span>
      </div></div>` : ''}

    ${man && man.refs && man.refs.length ? `<div class="panel"><div class="panel-head"><span>🧑‍🎨 角色参考帧（全片一致性的地基）</span>
      <span class="sub">质检结论：pass=通过 corrected=重出后通过 failed=不合格 skipped=未检查</span></div>
      <div class="ref-grid">${man.refs.map(ref => `<div class="ref-card">
        ${ref.path ? `<img loading="lazy" src="${fileUrl(run, ref.path)}" alt="${esc(ref.name)}">` : ''}
        <div class="ref-meta"><div class="ref-name" title="${esc(ref.name)}">${esc(ref.name)}${ref.variant && ref.variant !== 'base' ? ` <span class="tag">${esc(ref.variant)}</span>` : ''}</div>
          <div class="flex mt" style="gap:6px">${badge(ref.qc_status || 'unknown', ref.qc_status || '未质检')}</div>
          ${ref.qc_issues && ref.qc_issues.length ? `<div class="muted mt" style="font-size:11px">⚠ ${esc(ref.qc_issues.join('；'))}</div>` : ''}
          ${ref.qc_review && ref.qc_review.length ? `<div class="flex mt spread" style="gap:6px">
            <span class="muted" style="font-size:11px">🔍 ${esc(qcReviewSummary(ref))}</span>
            <button class="btn small" data-qcref="${esc(ref.token || ref.name)}">审核链路</button>
          </div>` : ''}
        </div></div>`).join('')}</div></div>` : ''}

    <div class="panel">
      <div class="panel-head"><span>🎞 镜头（${shots.length}）</span>
        <span class="sub">点击镜头查看视频与重渲历史；状态 ${badge('done', 'done')} ${badge('failed', 'failed')} ${badge('unknown', '未知')}</span></div>
      ${shots.length ? `<div class="shot-grid">${shots.map(s => {
        const img = s.image || (s.video ? s.video.replace(/\.mp4$/, '.png') : '');
        return `<div class="shot-card" data-shot="${esc(s.id)}">
          ${img ? `<img class="shot-thumb" loading="lazy" src="${fileUrl(run, img)}" alt="${esc(s.id)}"
                onerror="this.onerror=null;this.outerHTML='<div class=&quot;shot-thumb none&quot;>🎬</div>'">`
               : `<div class="shot-thumb none">🎬</div>`}
          <div class="shot-meta">
            <div><div class="shot-id">${esc(s.id)} ${s.night ? '🌙' : ''}</div>
              <div class="shot-sub">${s.model ? esc(trunc(s.model, 24)) : ''}</div></div>
            <div style="text-align:right">${badge(s.status)}
              <div class="shot-sub">${fmtSec(s.dur)}${s.attempts > 1 ? ` · ${s.attempts} 次` : ''}</div></div>
          </div></div>`;
      }).join('')}</div>` : `<div class="empty"><div class="big">🎥</div>还没有镜头产物。先「仅出首帧」或「完整渲染」。</div>`}
    </div>`;

  const q = () => ({
    quality: $('#r-quality').value, workers: +$('#r-workers').value || 0, timeout_minutes: +$('#r-timeout').value || 0,
  });
  $('#r-dryrun').onclick = () => startJob('render', run, { dry_run: true, ...q() });
  $('#r-images').onclick = () => startJob('render', run, { images_only: true, ...q() }, {
    confirmText: '仅出首帧会调用出图接口（小成本）。确认？',
  });
  $('#r-full').onclick = () => startJob('render', run, { ...q() }, {
    confirmText: '完整渲染按镜头数出图+出视频，是整条流水线最贵的一步。确认开始？',
  });
  $('#r-forceref').onclick = () => startJob('render', run, { force_ref: true, ...q() }, {
    confirmText: '重新生成角色参考帧会改变全片角色长相（一致性基准重置）。确认？',
  });
  $('#r-repair').onclick = () => {
    const v = $('#r-shots').value.trim();
    if (!v) { toast('请填镜头 ID，如 sh003,sh007', 'warn'); return; }
    startJob('render', run, { shots: v, ...q() });
  };
  $('#r-takes').onclick = () => {
    const v = $('#r-takeshots').value.trim();
    if (!v) { toast('请填镜头 ID', 'warn'); return; }
    startJob('takes', run, { shots: v, pick: $('#r-pick').value }, {
      confirmText: '多 take 对比会换种重渲一个新 take（产生视频费用）。确认？',
    });
  };
  if (hasAnimatic) $('#r-play-animatic').onclick = () => {
    openModal('🎬 动态分镜（灰盒预览）',
      `<video class="player" controls autoplay src="${fileUrl(run, 'output/animatic.mp4')}"></video>`,
      `<a class="btn" href="${fileUrl(run, 'output/animatic.mp4', true)}">下载</a>`, '900px');
  };

  view.querySelectorAll('.shot-card[data-shot]').forEach(card => {
    card.onclick = () => openShotModal(run, shots.find(s => s.id === card.dataset.shot));
  });
  view.querySelectorAll('[data-qcref]').forEach(b => b.onclick = () => {
    const key = b.dataset.qcref;
    const ref = (man.refs || []).find(r => (r.token || r.name) === key);
    if (ref) qcReviewModal(run, ref);
  });

  // 渲染进行中：每 15 秒探测产物变化，新视频一落盘重绘即可见。
  // 视频播放中 / 表单输入中 / 已离开本页时跳过，不打断观看与操作。
  const sigOf = d2 => `${(d2.manifest?.shots || []).filter(s => s.video).length}:${(d2.outputs || []).length}`;
  let lastSig = sigOf(d);
  const myPoll = (window._renderPoll || 0) + 1;
  window._renderPoll = myPoll;
  if (window._renderTimer) clearInterval(window._renderTimer);
  window._renderTimer = setInterval(async () => {
    if (window._renderPoll !== myPoll || !location.hash.startsWith('#/render')) { clearInterval(window._renderTimer); return; }
    if (document.querySelector('video.player:not([paused])')) return;
    const ae = document.activeElement;
    if (ae && (ae.tagName === 'INPUT' || ae.tagName === 'SELECT' || ae.tagName === 'TEXTAREA')) return;
    try {
      const nd = await api('/api/runs/' + encodeURIComponent(run));
      if (sigOf(nd) === lastSig) return;
      lastSig = sigOf(nd);
      pageRender(view); // 重绘后由新调用接管轮询
    } catch (e) { /* 网络抖动静默，下轮再试 */ }
  }, 15000);
}

function openShotModal(run, s) {
  if (!s) return;
  const lastRound = (s.rounds && s.rounds.length) ? s.rounds[s.rounds.length - 1] : null;
  const defects = lastRound && lastRound.video_verdict && lastRound.video_verdict.defects || [];
  openModal(`🎞 镜头 ${esc(s.id)}`,
    `${s.video ? `<video class="player" controls src="${fileUrl(run, s.video)}"></video>` : `<div class="empty">该镜头暂无视频${s.status === 'failed' ? '（渲染失败）' : ''}</div>`}
    <div class="grid2 mt">
      <div>
        <table class="tbl">
          <tr><th>状态</th><td>${badge(s.status)}</td></tr>
          <tr><th>实测时长</th><td>${fmtSec(s.dur)}（计划 ${fmtSec(s.plan)}）</td></tr>
          <tr><th>种子</th><td class="mono">${esc(s.seed ?? '-')}</td></tr>
          <tr><th>模型</th><td>${esc(s.model || '-')}</td></tr>
          <tr><th>参考帧</th><td>${s.used_ref ? '已用参考图锁脸' : '未用参考图'}</td></tr>
          <tr><th>尝试次数</th><td>${s.attempts}</td></tr>
          ${s.chained_from ? `<tr><th>链式衔接</th><td>承接 ${esc(s.chained_from)} 末帧</td></tr>` : ''}
        </table>
      </div>
      <div>
        <h4 class="mt" style="margin-top:0">重渲轮次</h4>
        ${s.rounds && s.rounds.length ? `<table class="tbl"><tr><th>轮</th><th>种子</th><th>耗时</th><th>结果</th></tr>
          ${s.rounds.map(r => `<tr><td>${r.round}</td><td class="mono">${esc(r.seed)}</td>
            <td>${fmtSec(r.duration_sec)}</td><td>${r.error ? badge('failed', '失败') : badge('done', '完成')}
            ${r.error ? `<div class="muted" style="font-size:11px">${esc(trunc(r.error, 80))}</div>` : ''}</td></tr>`).join('')}</table>`
        : '<div class="muted">一次通过</div>'}
        ${defects.length ? `<h4 class="mt">质检缺陷</h4>` + defects.map(dd =>
          `<div class="scene-card" style="border-left-color:var(--warn)"><b>[${esc(dd.code)}]</b> ${esc(dd.message)}</div>`).join('') : ''}
      </div>
    </div>`,
    `${s.status !== 'done' ? `<button class="btn primary" id="sm-rerender">🔧 重渲此镜</button>` : ''}
     <button class="btn" id="sm-take">🎥 生成对比 Take</button>`, '940px');
  const mask = modalStack[modalStack.length - 1];
  const rr = mask.querySelector('#sm-rerender');
  if (rr) rr.onclick = () => startJob('render', run, { shots: s.id });
  mask.querySelector('#sm-take').onclick = () => startJob('takes', run, { shots: s.id });
}

/* ================= 页面：声音字幕 ================= */
async function pageAudio(view) {
  const run = pickRun();
  if (!run) { view.innerHTML = needRunHTML('声音字幕：配音逐句试听、字幕预览、配乐 Cue Sheet'); return; }
  const d = await api('/api/runs/' + encodeURIComponent(run));
  const outputs = d.outputs || [];

  let narration = null;
  try {
    const art = await api(`/api/runs/${encodeURIComponent(run)}/artifacts/narration`);
    narration = art.data && art.data.lines || art.data && art.data.narration || null;
  } catch { /* 旧 run 无 narration.json */ }
  if (!narration) {
    try {
      const vo = await api(`/api/runs/${encodeURIComponent(run)}/artifacts/voiceover`);
      narration = (vo.data && vo.data.narration || []).map(l => ({ ...l }));
    } catch { /* 无台词 */ }
  }

  const srt = outputs.find(f => f.path === 'output/subtitles.srt');
  const mix = outputs.find(f => f.path === 'output/audio_mix.wav');
  const musicFiles = outputs.filter(f => f.path.startsWith('output/music/') && f.kind === 'audio');
  const sfxFiles = outputs.filter(f => f.path.startsWith('output/sfx/') && f.kind === 'audio');

  let music = null;
  try {
    const art = await api(`/api/runs/${encodeURIComponent(run)}/artifacts/music`);
    music = art.data;
  } catch { /* 无配乐方案 */ }

  view.innerHTML = `
    <div class="panel">
      <div class="panel-head"><span>🎙 配音台词（唯一真源 voiceover → 逐句实测）</span>
        <span class="sub">${narration && narration.length ? `${narration.length} 句` : '暂无'} · 溢出的句子会顺延而非截断</span></div>
      ${narration && narration.length ? `<div style="overflow-x:auto"><table class="tbl">
        <tr><th>#</th><th>镜头</th><th>角色</th><th>台词</th><th>情绪</th><th>语速</th><th class="num">实测</th><th class="num">入点</th><th>试听</th></tr>
        ${narration.map((l, i) => `<tr>
          <td class="muted">${i + 1}</td>
          <td class="mono">${esc(l.shot_id || '')}${l.overflow ? ' <span class="badge b-warn">顺延</span>' : ''}${l.truncated ? ' <span class="badge b-warn">压缩</span>' : ''}</td>
          <td>${esc(l.speaker || '')}${l.voice ? `<div class="muted" style="font-size:11px">音色 ${esc(l.voice)}</div>` : ''}</td>
          <td class="line-text">${esc(l.text || '')}</td>
          <td class="muted">${esc(l.emotion || '-')}</td>
          <td class="muted">${l.speed ?? '-'}</td>
          <td class="num">${fmtSec(l.measured_sec)}</td>
          <td class="num">${fmtSec(l.abs_start_sec ?? l.start_sec)}</td>
          <td>${l.file ? `<audio class="player" controls preload="none" src="${fileUrl(run, l.file)}"></audio>`
            : l.raw_file ? `<audio class="player" controls preload="none" src="${fileUrl(run, l.raw_file)}"></audio>` : '<span class="muted">未合成</span>'}</td>
        </tr>`).join('')}</table></div>`
      : `<div class="empty"><div class="big">🎙</div>还没有台词。创作流水线跑到 voiceover 阶段后这里会出现逐句台词与试听。</div>`}
    </div>

    <div class="grid2">
      <div class="panel">
        <div class="panel-head"><span>💬 字幕</span>${srt ? `<a class="btn small" href="${fileUrl(run, srt.path, true)}">下载 SRT</a>` : ''}</div>
        ${srt ? `<pre class="json" id="srt-view">加载中…</pre>` : `<div class="empty">未生成字幕（deliver 后出现）</div>`}
      </div>
      <div>
        <div class="panel">
          <div class="panel-head"><span>🎚 混音母版</span></div>
          ${mix ? `<audio style="width:100%" controls src="${fileUrl(run, mix.path)}"></audio>
            <div class="muted mt" style="font-size:12px">${esc(mix.path)} · ${fmtBytes(mix.size)}</div>`
          : `<div class="empty">未生成混音（deliver 后出现）</div>`}
        </div>
        <div class="panel">
          <div class="panel-head"><span>🎵 配乐与音效文件</span><span class="sub">缺什么在交付报告里逐条列明</span></div>
          ${musicFiles.length || sfxFiles.length ? `<table class="tbl">
            ${musicFiles.map(f => `<tr><td>🎵 ${esc(f.path)}</td><td class="muted">${fmtBytes(f.size)}</td>
              <td><audio class="player" controls preload="none" src="${fileUrl(run, f.path)}"></audio></td></tr>`).join('')}
            ${sfxFiles.map(f => `<tr><td>💥 ${esc(f.path)}</td><td class="muted">${fmtBytes(f.size)}</td>
              <td><audio class="player" controls preload="none" src="${fileUrl(run, f.path)}"></audio></td></tr>`).join('')}
          </table>` : `<div class="empty">暂无配乐/音效素材。整理 library/music、library/sfx 或在 music.json 后补齐文件重跑交付。</div>`}
        </div>
      </div>
    </div>

    ${music ? `<div class="panel"><div class="panel-head"><span>🎼 配乐方案（Cue 按场景边界精确对齐）</span></div>
      ${music.cue_sheet ? autoTable(music.cue_sheet) : ''}
      ${music.sound_fx && music.sound_fx.length ? `<h4 class="mt">音效清单</h4>` + autoTable(music.sound_fx) : ''}
    </div>` : ''}`;

  if (srt) {
    fetch(fileUrl(run, srt.path)).then(r => r.text()).then(t => {
      const el = $('#srt-view');
      if (el) el.textContent = t;
    }).catch(() => { });
  }
}

/* ================= 页面：交付成片 ================= */
async function pageDelivery(view) {
  const run = pickRun();
  if (!run) { view.innerHTML = needRunHTML('交付成片：配音→字幕→混音→合成→校验一条龙'); return; }
  const d = await api('/api/runs/' + encodeURIComponent(run));
  const rep = d.report;
  const finals = (d.outputs || []).filter(f => f.kind === 'video' && /final_/.test(f.name));
  const reviewPage = (d.outputs || []).find(f => f.path === 'output/review.html');

  view.innerHTML = `
    <div class="panel">
      <div class="panel-head"><span>📦 交付操作</span><span class="sub">缺镜头时默认拒绝出片（可显式允许内部审看）</span></div>
      <div class="form-row">
        <label class="check"><input type="checkbox" id="d-allow"> 允许缺镜头出片（内部审看，报告会标注）</label>
        <label class="check"><input type="checkbox" id="d-noburn"> 不烧录字幕（出无字幕母版 + 外挂 SRT）</label>
        <label class="check"><input type="checkbox" id="d-noaudio"> 跳过配音（只验证画面）</label>
      </div>
      <div class="form-row">
        <label class="check"><input type="checkbox" id="d-visual" ${d.mock ? '' : 'checked'}> 交付时做视觉复核（让模型看画面）</label>
        <button class="btn primary" id="d-go">🚀 开始交付</button>
      </div>
    </div>

    ${finals.length ? `<div class="panel"><div class="panel-head"><span>🎞 成片</span>
      <span class="flex" style="gap:8px;align-items:center">
        ${reviewPage ? `<a class="btn small" href="${fileUrl(run, reviewPage.path)}" target="_blank" rel="noopener"
          title="逐镜首帧 + 画面描述 + 台词节拍 + 镜间转场标记">🎬 审片页</a>` : ''}
      </span></div>
      ${finals.map(f => `<div class="mb">
        <div class="flex spread mb"><b>${esc(f.name)}</b>
          <span class="flex"><span class="muted">${fmtBytes(f.size)} · ${fmtTime(f.mod_time)}</span>
          <a class="btn small" href="${fileUrl(run, f.path, true)}">下载</a></span></div>
        <video class="player" controls preload="metadata" src="${fileUrl(run, f.path)}"></video>
      </div>`).join('')}</div>`
    : `<div class="panel"><div class="empty"><div class="big">🎬</div>还没有成片。渲染完整后点「开始交付」。</div></div>`}

    ${rep ? `<div class="cards">
      ${statCard('完整性', rep.complete ? badge('done', `完整 ${rep.shots_done}/${rep.shots_total}`) : badge('warn', `缺 ${rep.missing_shots?.length || 0} 镜`),
        rep.placeholder_shots && rep.placeholder_shots.length ? `占位镜头：${esc(rep.placeholder_shots.join(','))}` : '')}
      ${statCard('成片', `${fmtSec(rep.video.duration_sec)}`, `${rep.video.width}×${rep.video.height} · ${rep.video.fps}fps · ${esc(rep.video.video_codec)}`)}
      ${statCard('响度', rep.audio.has_audio ? `${rep.audio.loudness_lufs.toFixed(1)} LUFS` : '无声轨',
        rep.audio.has_audio ? `峰值 ${rep.audio.true_peak_db.toFixed(1)} dBFS · ${rep.audio.voice_lines} 台词 / ${rep.audio.music_tracks} 配乐 / ${rep.audio.sfx_tracks} 音效` : '')}
      ${statCard('字幕', `${rep.subtitle.cue_count} 条`, `最快 ${rep.subtitle.max_cps.toFixed(1)} 字/秒 · ${rep.subtitle.burned ? '已烧录' : '外挂 SRT'}${rep.subtitle.violation_count ? ` · ${rep.subtitle.violation_count} 处排版问题` : ''}`)}
    </div>

    <div class="panel">
      <div class="panel-head"><span>✅ 交付校验（实测值判定）</span>
        <span class="sub">报告生成于 ${fmtTime(rep.generated_at)}</span></div>
      <table class="tbl"><tr><th>结论</th><th>校验项</th><th>实测详情</th></tr>
        ${(rep.checks || []).map(c => `<tr>
          <td>${c.ok ? badge('ok', '通过') : (c.blocking ? badge('blocking', '阻断') : badge('warning', '告警'))}</td>
          <td><b>${esc(c.name)}</b></td><td class="muted">${esc(c.detail)}</td></tr>`).join('')}
      </table>
      ${rep.warnings && rep.warnings.length ? `<h4 class="mt">⚠ 告警</h4>` + rep.warnings.map(x => `<div class="muted">· ${esc(x)}</div>`).join('') : ''}
      ${rep.blockers && rep.blockers.length ? `<h4 class="mt">⛔ 阻断</h4>` + rep.blockers.map(x => `<div style="color:var(--err-text)">· ${esc(x)}</div>`).join('') : ''}
      ${(rep.outputs || []).length ? `<h4 class="mt">输出文件</h4><div class="flex">${rep.outputs.map(o =>
        `<span class="tag mono">${esc(o)}</span>`).join('')}</div>` : ''}
    </div>

    ${rep.visual_qa ? pageDeliveryVQAHTML(rep.visual_qa, run) : ''}
    ` : `<div class="panel"><div class="empty"><div class="big">📋</div>还没有交付报告。交付完成后这里会展示全部校验结果。</div></div>`}
  `;

  $('#d-go').onclick = () => startJob('deliver', run, {
    allow_incomplete: $('#d-allow').checked, no_burn: $('#d-noburn').checked,
    skip_audio: $('#d-noaudio').checked,
    visual: $('#d-visual').checked && !d.mock, no_visual: !$('#d-visual').checked,
  }, { confirmText: '交付会合成配音/字幕/配乐（配音与视觉复核产生少量费用）。确认开始？' });
}

function pageDeliveryVQAHTML(vqa, run) {
  const dims = [
    ['身份一致性', vqa.identity_avg], ['解剖与形变', vqa.anatomy_avg], ['电影感', vqa.cinematic_avg],
    ['场景一致性', vqa.scene_avg],
  ];
  return `<div class="panel">
    <div class="panel-head"><span>👁 视觉复核（模型真的在看画面）</span>
      <span class="sub">${esc(vqa.model || '')} · ${vqa.frames_total} 帧 · ${vqa.pass ? badge('ok', '通过') : badge('warn', '有镜头待处理')}</span></div>
    <div class="flex mb">
      <div><div class="score-num" style="color:${vqa.score >= 7.5 ? 'var(--ok-text)' : 'var(--warn-text)'}">${vqa.score.toFixed(1)}</div><div class="muted" style="font-size:11px">总体均分</div></div>
      ${dims.filter(x => x[1] != null).map(([k, v]) => `<div style="min-width:150px">
        <div class="flex spread" style="font-size:12px"><span class="muted">${k}</span><b>${Number(v).toFixed(1)}</b></div>
        <div class="dim-bar"><div style="width:${Number(v) * 10}%;background:${v >= 7.5 ? 'var(--ok)' : v >= 6 ? 'var(--warn)' : 'var(--err)'}"></div></div>
      </div>`).join('')}
    </div>
    ${vqa.rerender_shots && vqa.rerender_shots.length ? `<div class="scene-card" style="border-left-color:var(--err)">
      <b>建议定点重渲：${esc(vqa.rerender_shots.join(', '))}</b>
      <div class="mt"><button class="btn primary" id="vqa-rerender">🔧 一键重渲这 ${vqa.rerender_shots.length} 个镜头</button></div></div>` : ''}
    ${vqa.shots && vqa.shots.length ? `<table class="tbl"><tr><th>镜头</th><th class="num">总分</th><th class="num">身份</th><th class="num">解剖</th><th class="num">电影感</th><th class="num">场景</th><th>问题</th><th>建议</th></tr>
      ${vqa.shots.map(s => `<tr>
        <td class="mono">${esc(s.shot_id)} ${s.failed ? badge('warning', '结论不可用') : ''}</td>
        <td class="num">${s.score.toFixed(1)}</td>
        <td class="num">${s.identity != null ? Number(s.identity).toFixed(1) : '-'}</td>
        <td class="num">${s.anatomy != null ? Number(s.anatomy).toFixed(1) : '-'}</td>
        <td class="num">${s.cinematic != null ? Number(s.cinematic).toFixed(1) : '-'}</td>
        <td class="num">${s.scene != null ? Number(s.scene).toFixed(1) : '-'}</td>
        <td>${(s.issues || []).map(i => `<div>· ${esc(i)}</div>`).join('') || '<span class="muted">-</span>'}</td>
        <td class="muted">${esc(s.fix || '')}</td></tr>`).join('')}</table>` : ''}
    ${vqa.watermark ? `<h4 class="mt">水印 / 叠字检测</h4>
      ${vqa.watermark.checked
        ? (vqa.watermark.suspected
          ? `<div class="scene-card" style="border-left-color:var(--err)"><b>⚠ 疑似检出叠字角标</b><table class="tbl mt">
             ${(vqa.watermark.shots || []).map(h => `<tr><td class="mono">${esc(h.shot_id)}</td><td>${esc(h.text || '')}</td>
               <td>${esc(h.location || '')}</td><td class="num">置信 ${(h.confidence * 100 || 0).toFixed(0)}%</td></tr>`).join('')}</table></div>`
          : `<div class="muted">✓ ${esc(vqa.watermark.note || '未检出可见水印')}</div>`)
        : `<div class="muted">⚠ ${esc(vqa.watermark.note || '未检查（视觉复核未启用或模型未答）')} ——「没查」不等于「没有」</div>`}
    ${vqa.notes && vqa.notes.length ? `<div class="muted mt" style="font-size:12px">${vqa.notes.map(n => '· ' + esc(n)).join('<br>')}</div>` : ''}
    ${vqa.feedback ? `<div class="scene-card mt">${esc(vqa.feedback)}</div>` : ''}
    ` : ''}
  </div>`;
}

/* ================= 页面：质检复核 ================= */
async function pageQA(view) {
  const run = pickRun();
  if (!run) { view.innerHTML = needRunHTML('质检复核：视觉复核评分、抽帧人工核对、打磨闭环与多 Take'); return; }
  const d = await api('/api/runs/' + encodeURIComponent(run));
  const rep = d.report;
  const vqa = rep && rep.visual_qa;
  const reviewFrames = (d.outputs || []).filter(f => f.path.startsWith('output/review_frames/') && f.kind === 'image');
  const takesFiles = (d.outputs || []).filter(f => f.path.startsWith('output/takes/') && f.kind === 'video');
  const invalidFiles = (d.outputs || []).filter(f => f.path.includes('invalid') && f.kind === 'video');
  const reviewPage = (d.outputs || []).find(f => f.path === 'output/review.html');

  view.innerHTML = `
    <div class="panel">
      <div class="panel-head"><span>🪞 打磨闭环（复核 → 定点重渲 → 重交付 → 再复核）</span>
        <span class="flex" style="gap:8px;align-items:center">
          ${reviewPage ? `<a class="btn small" href="${fileUrl(run, reviewPage.path)}" target="_blank" rel="noopener"
            title="逐镜首帧 + 画面描述 + 台词节拍 + 镜间转场标记，新标签页打开">🎬 审片页（逐镜台词/转场）</a>` : ''}
          <span class="sub">每轮账目透明；预算熔断全程生效</span>
        </span></div>
      <div class="form-row">
        <div class="form-item"><label>最多轮数</label><input type="number" id="p-rounds" value="2" min="1" max="5" style="width:90px"></div>
        <div class="form-item"><label>达标分（0=沿用配置）</label><input type="number" id="p-score" step="0.1" min="0" max="10" style="width:110px" placeholder="默认"></div>
        <label class="check"><input type="checkbox" id="p-noburn"> 不烧录字幕</label>
        <button class="btn primary" id="p-go">💎 开始打磨</button>
        <button class="btn" id="p-verify">📋 复核交付报告 + 镜头客观质检复跑</button>
      </div>
    </div>

    ${vqa ? pageDeliveryVQAHTML(vqa, run) : `<div class="panel"><div class="empty"><div class="big">👁</div>
      还没有视觉复核结论。交付时勾选「视觉复核」，或直接运行上面的打磨闭环。</div></div>`}

    ${reviewFrames.length ? `<div class="panel"><div class="panel-head"><span>🖼 复核抽帧（人工核对模型判断）</span>
      <span class="sub">${reviewFrames.length} 张 · 来自每镜头抽帧</span></div>
      <div class="ref-grid">${reviewFrames.map(f => `<div class="ref-card">
        <img loading="lazy" src="${fileUrl(run, f.path)}" alt="${esc(f.name)}" style="aspect-ratio:16/9">
        <div class="ref-meta"><div class="ref-name" title="${esc(f.name)}">${esc(f.name)}</div></div></div>`).join('')}</div></div>` : ''}

    ${takesFiles.length ? `<div class="panel"><div class="panel-head"><span>🎥 多 Take 对比</span><span class="sub">compare=并排对比片；默认恢复 takeA，--pick 才切换</span></div>
      <div class="grid2">${takesFiles.map(f => `<div>
        <div class="flex spread mb"><span class="mono">${esc(f.name)}</span>
          <a class="btn small" href="${fileUrl(run, f.path, true)}">下载</a></div>
        <video class="player" controls preload="metadata" src="${fileUrl(run, f.path)}"></video></div>`).join('')}</div></div>` : ''}

    ${invalidFiles.length ? `<div class="panel"><div class="panel-head"><span>🗑 隔离区（不合格产物留证）</span></div>
      <table class="tbl">${invalidFiles.map(f => `<tr><td class="mono">${esc(f.path)}</td><td class="muted">${fmtBytes(f.size)}</td>
        <td><button class="btn small" data-play="${esc(f.path)}" data-name="${esc(f.name)}">查看</button></td></tr>`).join('')}</table></div>` : ''}
  `;

  $('#p-go').onclick = () => startJob('polish', run, {
    rounds: +$('#p-rounds').value || 2, pass_score: +$('#p-score').value || 0, no_burn: $('#p-noburn').checked,
  }, { confirmText: '打磨会循环执行 视觉复核→定点重渲→重交付（重渲产生视频费用）。确认开始？' });
  $('#p-verify').onclick = () => startJob('verify', run, { render: true });
  const vr = $('#vqa-rerender');
  if (vr) vr.onclick = () => startJob('render', run, { shots: (vqa.rerender_shots || []).join(',') });
  view.querySelectorAll('[data-play]').forEach(b => b.onclick = () => {
    openModal(`隔离产物 · ${esc(b.dataset.name)}`,
      `<video class="player" controls src="${fileUrl(run, b.dataset.play)}"></video>`, null, '760px');
  });
}

/* ================= 页面：运行管理 ================= */
let runsTab = 'active';

async function pageRuns(view) {
  view.innerHTML = `<div class="panel">
    <div class="panel-head"><span>🗂 项目管理</span><span class="sub">断点续跑不会重买已付费产物；删除为逻辑删除，文件保留可恢复</span></div>
    <div class="tabs">
      <div class="tab ${runsTab === 'active' ? 'active' : ''}" data-tab="active">📁 项目列表</div>
      <div class="tab ${runsTab === 'trash' ? 'active' : ''}" data-tab="trash">🗑 回收站</div>
    </div>
    <div id="runs-body"><div class="loading">加载中…</div></div>
  </div>`;
  view.querySelectorAll('.tab[data-tab]').forEach(t => t.onclick = () => {
    runsTab = t.dataset.tab;
    view.querySelectorAll('.tab[data-tab]').forEach(x => x.classList.toggle('active', x.dataset.tab === runsTab));
    renderRunsBody();
  });
  await renderRunsBody();
}

async function renderRunsBody() {
  const body = $('#runs-body');
  if (runsTab === 'trash') {
    let items = [];
    try { items = await api('/api/runs?trash=1'); } catch { /* 忽略 */ }
    if (!items || !items.length) {
      body.innerHTML = `<div class="empty"><div class="big">🗑</div>回收站是空的。删除的项目会在这里保留全部文件，可随时恢复。</div>`;
      return;
    }
    body.innerHTML = `<div style="overflow-x:auto"><table class="tbl">
      <tr><th>项目</th><th>阶段</th><th>删除时间</th><th>操作</th></tr>
      ${items.map(it => {
        const r = it.summary, d = it.deleted || {};
        return `<tr>
          <td><div class="mono">${esc(r.id)}</div><div class="muted" style="max-width:320px">${esc(trunc(r.brief, 52))}</div></td>
          <td>${r.stage_done}/${r.stage_total}</td>
          <td class="muted">${fmtTime(d.deleted_at)}</td>
          <td><button class="btn small" data-restore="${esc(r.id)}">♻ 恢复</button>
              <button class="btn small danger" data-purge="${esc(r.id)}">✕ 彻底删除</button></td>
        </tr>`;
      }).join('')}
    </table></div>
    <div class="muted mt" style="font-size:12px">恢复：项目回到列表并可在右上角重新选择；彻底删除：连文件一起清除（成片/素材/任务记录），不可恢复。</div>`;
    body.querySelectorAll('[data-restore]').forEach(b => b.onclick = async () => {
      try {
        await api(`/api/runs/${encodeURIComponent(b.dataset.restore)}/restore`, { method: 'POST' });
        toast('已恢复：' + b.dataset.restore, 'ok');
        await loadRuns(); renderRunsBody();
      } catch (e) { toast('恢复失败：' + e.message, 'err'); }
    });
    body.querySelectorAll('[data-purge]').forEach(b => b.onclick = async () => {
      const id = b.dataset.purge;
      if (!window.confirm(`彻底删除 ${id} 的全部文件（含成片、素材、任务记录）？\n此操作不可恢复！`)) return;
      try {
        await api(`/api/runs/${encodeURIComponent(id)}?confirm=1`, { method: 'DELETE' });
        toast('已彻底删除：' + id, 'ok');
        await loadRuns(); renderRunsBody();
      } catch (e) { toast('删除失败：' + e.message, 'err'); }
    });
    return;
  }

  const runs = (await api('/api/runs')) || [];
  if (!runs.length) {
    body.innerHTML = `<div class="empty"><div class="big">🌌</div>还没有项目。去 <a href="#/dashboard">总览</a> 提交第一部片子的 brief。</div>`;
    return;
  }
  body.innerHTML = `<div style="overflow-x:auto"><table class="tbl">
      <tr><th>项目</th><th>阶段</th><th>渲染</th><th class="num">花费</th><th>模式</th><th>更新</th><th>操作</th></tr>
      ${runs.map(r => `<tr>
        <td><div class="mono">${esc(r.id)}</div><div class="muted" style="max-width:320px">${esc(trunc(r.brief, 52))}</div></td>
        <td>${r.stage_done}/${r.stage_total}${r.stage_fail ? ` <span class="badge b-failed">失败${r.stage_fail}</span>` : ''}</td>
        <td>${r.complete === null ? badge('none', '未渲染') : r.complete ? badge('done', `完整 ${r.shots_total}`) : badge('warn', `缺 ${r.shots_total - r.shots_done}`)}</td>
        <td class="num">${r.cost_cny ? '¥' + r.cost_cny.toFixed(2) : '-'}</td>
        <td>${r.mock ? badge('mock', '离线') : badge('ok', '真实')}</td>
        <td class="muted">${fmtAgo(r.updated_at)}</td>
        <td><button class="btn small" data-use="${esc(r.id)}">设为当前</button>
            <button class="btn small" data-detail="${esc(r.id)}">详情</button>
            <button class="btn small danger" data-del="${esc(r.id)}">🗑 删除</button></td>
      </tr>`).join('')}
    </table></div>`;

  body.querySelectorAll('[data-use]').forEach(b => b.onclick = () => {
    state.run = b.dataset.use; localStorage.setItem('studio.run', state.run);
    $('#run-select').value = state.run; loadRuns(); toast('已设为当前项目：' + state.run, 'ok');
  });
  body.querySelectorAll('[data-detail]').forEach(b => b.onclick = () => openRunDetail(b.dataset.detail));
  body.querySelectorAll('[data-del]').forEach(b => b.onclick = async () => {
    const id = b.dataset.del;
    if (!window.confirm(`删除项目 ${id}？\n逻辑删除：文件全部保留，随时可在回收站恢复。`)) return;
    try {
      await api(`/api/runs/${encodeURIComponent(id)}/delete`, { method: 'POST' });
      toast('已移入回收站：' + id, 'ok');
      await loadRuns(); renderRunsBody();
    } catch (e) { toast('删除失败：' + e.message, 'err'); }
  });
}

async function openRunDetail(run) {
  const d = await api('/api/runs/' + encodeURIComponent(run));
  const tp = d.tokens.prompt + d.tokens.completion;
  const mask = openModal(`🗂 ${esc(run)}`,
    `<div class="mb">${pipelineHTML(d.stages)}</div>
     <div class="cards">
       ${statCard('阶段', `${d.summary.stage_done}/${d.summary.stage_total}`, d.summary.stage_fail ? `失败 ${d.summary.stage_fail}` : '')}
       ${statCard('Token 用量', (tp / 1000).toFixed(1) + 'K', `调用 ${d.tokens.calls} 次 · prompt ${(d.tokens.prompt / 1000).toFixed(1)}K / completion ${(d.tokens.completion / 1000).toFixed(1)}K`)}
       ${statCard('渲染花费', d.summary.cost_cny ? '¥' + d.summary.cost_cny.toFixed(2) : '-', '')}
     </div>
     ${Object.keys(d.usage || {}).length ? `<h4>分阶段用量</h4>${autoTable(Object.entries(d.usage).map(([k, v]) => ({ 阶段: k, ...v })))}` : ''}
     <h4 class="mt">本项目任务历史</h4><div id="rd-jobs">加载中…</div>`,
    `<button class="btn" id="rd-resume">▶ 断点续跑</button>
     <button class="btn" id="rd-goto">打开项目面板</button>`, '960px');
  const jb = mask.querySelector('#rd-jobs');
  const jobs = d.jobs || [];
  jb.innerHTML = jobs.length ? `<table class="tbl"><tr><th>状态</th><th>任务</th><th>开始</th><th></th></tr>
    ${jobs.map(j => `<tr><td>${badge(j.status)}</td><td>${esc(j.label)}</td><td class="muted">${fmtAgo(j.started_at)}</td>
      <td><button class="btn small" data-log="${j.id}" data-label="${esc(j.label)}">日志</button></td></tr>`).join('')}</table>`
    : '<div class="muted">暂无任务记录</div>';
  jb.querySelectorAll('[data-log]').forEach(b => b.onclick = () => openLogModal(b.dataset.log, b.dataset.label));
  mask.querySelector('#rd-resume').onclick = () => startJob('resume', run, {}, { confirmText: '继续未完成的阶段（可能产生费用）。确认？' });
  mask.querySelector('#rd-goto').onclick = () => {
    state.run = run; localStorage.setItem('studio.run', run);
    $('#run-select').value = run; loadRuns(); closeModal(mask); location.hash = '#/creative'; route();
  };
}

/* ================= 页面：系统设置 ================= */
async function pageSystem(view) {
  const meta = await api('/api/meta');
  const sys = meta.system;

  view.innerHTML = `
    <div class="cards">
      ${statCard('工具链', sys.ffmpeg_ok ? badge('ok', 'ffmpeg 就绪') : badge('failed', 'ffmpeg 缺失'),
        `${esc(sys.ffmpeg_path || '')} <br> ${esc(sys.ffprobe_path || '')}`)}
      ${statCard('能力', `${sys.loudnorm ? '响度✓' : '响度✗'} ${sys.subtitles ? '字幕✓' : '字幕✗'} ${sys.sidechain ? '闪避✓' : '闪避✗'}`, esc((sys.warnings || []).join('；') || 'ffmpeg 滤镜能力完整'))}
      ${statCard('模型模式', sys.mock ? badge('mock', '离线演示') : badge('ok', '真实模型'), esc(trunc(sys.config_summary || '', 60)))}
      ${statCard('预算', sys.budget_cny > 0 ? `¥${sys.budget_cny.toFixed(2)}` : '不限制', '超限立即熔断并保留断点')}
    </div>
    ${sys.config_error ? `<div class="panel"><div class="scene-card" style="border-left-color:var(--err)"><b>配置自检未通过</b><div class="mt">${esc(sys.config_error)}</div></div></div>` : ''}

    <div class="panel">
      <div class="panel-head"><span>🩺 环境自检 + AI 诊断</span><span class="sub">付费调用之前先确认工具链与配置；可附带当前项目诊断失败阶段</span></div>
      <div class="flex">
        <button class="btn primary" id="s-doctor">🩺 运行自检</button>
        <label class="check"><input type="checkbox" id="s-doctor-run" ${state.run ? 'checked' : ''}> 附带诊断当前项目（${esc(state.run || '未选择')}）</label>
      </div>
    </div>

    <div class="panel">
      <div class="panel-head"><span>🧩 内置视觉风格预设</span><span class="sub">由代码固化保证整片统一；brief 风格关键词自动命中</span></div>
      <div class="cards">${(meta.presets || []).map(p => `<div class="card">
        <div class="k">${esc(p.name)}</div>
        <div class="v" style="font-size:15px">${esc(p.style_name || '')}</div>
        <div class="s">画幅 ${esc(p.aspect || '-')} · ${esc(p.camera || '')}<br>${esc(p.film_stock || '')}</div>
      </div>`).join('') || '<div class="empty">无预设</div>'}</div>
    </div>

    <div class="grid2">
      <div class="panel">
        <div class="panel-head"><span>🧭 工作流定义</span></div>
        ${(meta.workflows || []).map(w => `<div class="scene-card">
          <h4>${esc(w.name || w.file)} <span class="tag mono">${esc(w.file)}</span></h4>
          ${w.brief ? `<div class="muted mb">${esc(trunc(w.brief, 80))}</div>` : ''}
          <div class="pipeline">${(w.stages || []).map(s => `<div class="stage"><div class="st" style="font-size:12px">${esc(s)}</div></div>`).join('')}</div>
        </div>`).join('') || '<div class="empty">workflows/ 下无定义</div>'}
      </div>
      <div class="panel">
        <div class="panel-head"><span>🧠 经验记忆库（${(meta.lessons || []).length} 条）</span><span class="sub">引擎踩过的坑，自愈时自动召回</span></div>
        ${(meta.lessons || []).length ? `<div style="max-height:400px;overflow-y:auto">${meta.lessons.map(l =>
          `<div class="scene-card" style="border-left-color:var(--accent2)">
            <div class="flex spread"><b>${esc(l.skill)}</b><span class="muted" style="font-size:11px">${fmtTime(l.time)} · ${esc(l.source || '')}</span></div>
            <div class="mt" style="line-height:1.7">${esc(l.lesson)}</div></div>`).join('')}</div>`
        : '<div class="empty">经验库为空 —— 引擎还没踩过坑</div>'}
      </div>
    </div>

    <div class="panel">
      <div class="panel-head"><span>🔐 配置文件（api_key 已隐藏）</span><span class="sub">config.local.json 覆盖层不入库；密钥只显示是否已配置</span></div>
      ${(meta.configs || []).length ? `<div class="tabs">${meta.configs.map((c, i) =>
        `<div class="tab ${i === 0 ? 'active' : ''}" data-cfg="${i}">${esc(c.file)}</div>`).join('')}</div>
      <div id="cfg-view">${prettyJSON(meta.configs[0].content)}</div>`
      : '<div class="empty">未找到配置文件</div>'}
    </div>`;

  $('#s-doctor').onclick = () => startJob('doctor', $('#s-doctor-run').checked ? state.run : '', {});
  const tabs = view.querySelectorAll('.tab[data-cfg]');
  tabs.forEach(t => t.onclick = () => {
    tabs.forEach(x => x.classList.remove('active'));
    t.classList.add('active');
    $('#cfg-view').innerHTML = prettyJSON(meta.configs[+t.dataset.cfg].content);
  });
}

/* ================= 页面：调用记录 ================= */
async function pageCalls(view) {
  let page = 1;
  const pageSize = 50;
  view.innerHTML = `
    <div class="panel">
      <div class="panel-head"><span>🧾 AI 调用记录</span>
        <span class="sub">每次模型调用的请求/响应内容、首次响应与完成时间（logs/ai_calls/）</span></div>
      <div class="form-row" style="flex-wrap:wrap;gap:8px;align-items:flex-end">
        <div class="form-item"><label>类型</label>
          <select id="ac-kind" class="form-el">
            <option value="">全部</option>
            <option value="chat">文字</option>
            <option value="image">图片</option>
            <option value="video">视频</option>
          </select></div>
        <div class="form-item"><label>模型</label>
          <input id="ac-model" class="form-el" style="width:160px" placeholder="如 glm-5.3-flash"></div>
        <div class="form-item"><label>开始日期</label>
          <input id="ac-start" class="form-el" type="date"></div>
        <div class="form-item"><label>结束日期</label>
          <input id="ac-end" class="form-el" type="date"></div>
        <div class="form-item" style="flex:1;min-width:200px"><label>操作 / 项目（模糊）</label>
          <input id="ac-q" class="form-el" placeholder="如 assets、render.shot_image、run-2026…"></div>
        <button class="btn" id="ac-refresh">↻ 刷新</button>
      </div>
      <div id="ac-list"><div class="loading">加载中…</div></div>
    </div>`;
  const load = async () => {
    const box = $('#ac-list');
    box.innerHTML = '<div class="loading">加载中…</div>';
    const p = new URLSearchParams({ page: String(page), page_size: String(pageSize) });
    const val = id => $(id).value.trim();
    if (val('#ac-kind')) p.set('kind', val('#ac-kind'));
    if (val('#ac-model')) p.set('model', val('#ac-model'));
    if (val('#ac-q')) p.set('q', val('#ac-q'));
    if (val('#ac-start')) p.set('start', val('#ac-start'));
    if (val('#ac-end')) p.set('end', val('#ac-end'));
    try {
      const res = await api('/api/ai-calls?' + p.toString());
      const items = res.items || [];
      const total = res.total || 0;
      const pages = Math.max(1, Math.ceil(total / pageSize));
      if (!items.length) {
        box.innerHTML = `<div class="empty"><div class="big">🧾</div>没有符合条件的调用记录</div>`;
        return;
      }
      box.innerHTML = `<div style="overflow-x:auto"><table class="tbl"><tr>
          <th>时间</th><th>类型</th><th>操作</th><th>模型</th><th>首次响应</th><th>总耗时</th><th>状态</th><th>项目</th></tr>
        ${items.map(r => `<tr class="ac-row" data-id="${esc(r.id)}" style="cursor:pointer">
          <td class="mono">${fmtTime(r.start_at)}</td>
          <td>${badge('info', aiKindLabel(r.kind))}${r.mock ? ' ' + badge('mock', '离线') : ''}</td>
          <td class="mono">${esc(r.call || '')}</td>
          <td class="mono" title="${esc(r.model || '')}">${esc(trunc(r.model || '', 26))}</td>
          <td>${fmtDurMs(r.first_response_ms)}</td>
          <td>${fmtDurMs(r.total_ms)}</td>
          <td>${r.status === 'ok' ? badge('ok', 'ok') : badge('failed', '失败')}</td>
          <td class="mono" title="${esc(r.run || '')}">${esc(trunc(r.run || '', 22))}</td>
        </tr>`).join('')}</table></div>
      <div class="flex mt" style="justify-content:space-between;align-items:center">
        <span class="muted" style="font-size:12px">共 ${total} 条 · 第 ${res.page}/${pages} 页 · 点击行查看请求/响应详情</span>
        <span class="flex" style="gap:6px">
          <button class="btn small" id="ac-prev" ${res.page <= 1 ? 'disabled' : ''}>← 上一页</button>
          <button class="btn small" id="ac-next" ${res.page >= pages ? 'disabled' : ''}>下一页 →</button>
        </span></div>`;
      box.querySelectorAll('.ac-row').forEach(tr => tr.onclick = () => openCallDetail(tr.dataset.id));
      $('#ac-prev').onclick = () => { if (page > 1) { page--; load(); } };
      $('#ac-next').onclick = () => { if (page < pages) { page++; load(); } };
    } catch (e) { box.innerHTML = `<div class="empty">加载失败：${esc(e.message)}</div>`; }
  };
  $('#ac-refresh').onclick = load;
  ['#ac-kind', '#ac-model', '#ac-start', '#ac-end'].forEach(id => {
    $(id).onchange = () => { page = 1; load(); };
  });
  $('#ac-q').addEventListener('keydown', e => { if (e.key === 'Enter') { page = 1; load(); } });
  await load();
}

async function openCallDetail(id) {
  let r;
  try { r = await api('/api/ai-calls/' + encodeURIComponent(id)); }
  catch (e) { toast('加载失败：' + e.message, 'err'); return; }
  const req = r.request || {}, resp = r.response || {};
  // 响应内容里的媒体：优先本地文件（永不过期）→ 记录层存档副本 → 平台 URL（会过期）
  let mediaHTML = '';
  if (r.kind === 'image' || r.kind === 'video') {
    let src = '';
    if (resp.local_path && r.run) src = fileUrl(r.run, resp.local_path);
    else if (resp.archive_path) src = `/api/ai-calls/${encodeURIComponent(r.id)}/artifact`;
    else if (resp.url && resp.url.startsWith('http')) src = resp.url;
    if (src) {
      mediaHTML = r.kind === 'image'
        ? `<img src="${esc(src)}" style="max-width:100%;border-radius:8px" loading="lazy" alt="生成图片">`
        : `<video controls preload="metadata" style="max-width:100%;border-radius:8px" src="${esc(src)}"></video>`;
      const notes = [];
      if (resp.local_path) notes.push(`本地文件：<span class="mono">${esc(resp.local_path)}</span>`);
      if (resp.archive_path) notes.push(`记录存档：<span class="mono">${esc(resp.archive_path)}</span>`);
      if (notes.length) mediaHTML += `<div class="muted" style="font-size:11.5px;margin-top:4px">${notes.join(' · ')}</div>`;
    } else {
      mediaHTML = `<div class="muted">产物 URL：<span class="mono">${esc(resp.url || '-')}</span>${(resp.url || '').startsWith('mock://') ? '（离线演示占位，无真实画面）' : '（URL 可能已过期，成片请看对应项目产物）'}</div>`;
    }
  }
  // 请求侧图片：记录器把当时随请求发出的图（首帧/尾帧/参考图）归档了副本，这里回放。
  // 旧记录没有存档字段 → 不渲染该区（参数行已有"？未记录"标记）
  const reqImgs = [];
  if (req.first_frame_file) reqImgs.push(['首帧（图生视频的画面起点）', `first_frame`, -1]);
  if (req.end_frame_file) reqImgs.push(['尾帧锚点（运动终点约束）', `end_frame`, -1]);
  (req.ref_image_files || []).forEach((p, i) => reqImgs.push([`参考图 ${i + 1}`, `ref_image`, i]));
  const reqImgsHTML = reqImgs.length
    ? `<h4 class="mt">请求图片（当时喂给模型的）</h4>
       <div style="display:flex;gap:10px;flex-wrap:wrap">${reqImgs.map(([label, role, idx]) => `
         <figure style="margin:0;max-width:250px;flex:0 0 auto">
           <img src="/api/ai-calls/${encodeURIComponent(r.id)}/reqimg?role=${role}${idx >= 0 ? '&idx=' + idx : ''}" style="width:100%;border-radius:8px;background:#111" loading="lazy" alt="${esc(label)}">
           <figcaption class="muted" style="font-size:11px;margin-top:3px">${esc(label)}</figcaption>
         </figure>`).join('')}</div>`
    : '';
  const body = `
    <div class="kv">
      <span class="k">类型</span><span>${badge('info', aiKindLabel(r.kind))}${r.mock ? ' ' + badge('mock', '离线演示') : ''}</span>
      <span class="k">操作</span><span class="mono">${esc(r.call || '-')}</span>
      <span class="k">模型</span><span class="mono">${esc(r.model || '-')}</span>
      <span class="k">调用时间</span><span>${fmtTime(r.start_at)}</span>
      <span class="k">首次响应</span><span>${r.first_response_at ? fmtTime(r.first_response_at) : '-'}（${fmtDurMs(r.first_response_ms)}）</span>
      <span class="k">响应完成</span><span>${fmtTime(r.end_at)}（总耗时 ${fmtDurMs(r.total_ms)}）</span>
      ${r.usage ? `<span class="k">Tokens</span><span>输入 ${r.usage.prompt_tokens ?? '-'} / 输出 ${r.usage.completion_tokens ?? '-'}</span>` : ''}
      ${r.run ? `<span class="k">项目</span><span class="mono">${esc(r.run)}</span>` : ''}
      ${r.error ? `<span class="k">错误</span><span style="color:var(--err-text)">${esc(r.error)}</span>` : ''}
    </div>
    <h4 class="mt">请求内容</h4>
    ${req.system ? `<div class="muted" style="font-size:11px">System</div><pre class="log" style="max-height:220px">${esc(req.system)}</pre>` : ''}
    <div class="muted" style="font-size:11px">Prompt / 用户内容</div><pre class="log" style="max-height:300px">${esc(req.prompt || '（空）')}</pre>
    <div class="muted" style="font-size:12px">参数：温度 ${req.temperature ?? '-'} · max_tokens ${req.max_tokens ?? '-'} · 尺寸 ${esc(req.size || '-')} · 质量 ${esc(req.quality || '-')} · 参考图 ${req.ref_images ?? 0} 张${req.first_frame === 'none' ? ' · 首帧 ✗ 未传（纯文生视频）' : req.first_frame === 'data-url' ? ' · 首帧 ✓（内联 base64）' : req.first_frame ? ` · 首帧 ✓（<span class="mono">${esc(trunc(req.first_frame, 96))}</span>）` : (r.kind === 'video' ? ' · 首帧 ？（该记录早于此字段，未记录）' : '')}${req.end_image ? ' · 含尾帧' : ''}${req.duration_sec ? ' · 时长 ' + req.duration_sec + 's' : ''}</div>
    ${reqImgsHTML}
    <h4 class="mt">响应内容</h4>
    ${r.kind === 'chat'
      ? `<pre class="log" style="max-height:300px">${esc(resp.text || r.error || '（空）')}</pre>`
      : (mediaHTML || `<pre class="log">${esc(resp.url || r.error || '（空）')}</pre>`)
        + (resp.url && resp.url.startsWith('http') ? `<div class="muted" style="font-size:11px;margin-top:4px">来源 URL（平台签名，会过期）：<span class="mono">${esc(trunc(resp.url, 160))}</span></div>` : '')}`;
  openModal(`🧾 ${aiKindLabel(r.kind)}调用 · ${esc(r.call || '')}`, body, null, '880px');
}

/* ================= 路由 ================= */
const pages = {
  dashboard: { title: '总览', fn: pageDashboard },
  creative: { title: '创作中心', fn: pageCreative },
  assets: { title: '素材库', fn: pageAssets },
  render: { title: '渲染生成', fn: pageRender },
  audio: { title: '声音字幕', fn: pageAudio },
  delivery: { title: '交付成片', fn: pageDelivery },
  qa: { title: '质检复核', fn: pageQA },
  runs: { title: '运行管理', fn: pageRuns },
  calls: { title: '调用记录', fn: pageCalls },
  system: { title: '系统设置', fn: pageSystem },
};

async function route() {
  // 切页清掉残留弹窗：镜头详情/任务日志不跨页存留
  [...modalStack].forEach(m => closeModal(m));
  const key = (location.hash || '#/dashboard').replace(/^#\//, '') || 'dashboard';
  const page = pages[key] || pages.dashboard;
  document.querySelectorAll('#menu a').forEach(a => a.classList.toggle('active', a.dataset.page === key));
  $('#page-title').textContent = page.title;
  const view = $('#view');
  view.innerHTML = '<div class="loading">加载中…</div>';
  view.scrollTop = 0;
  try {
    await page.fn(view);
  } catch (e) {
    view.innerHTML = `<div class="panel"><div class="scene-card" style="border-left-color:var(--err)">
      <b>页面加载失败</b><div class="mt muted">${esc(e.message)}</div>
      <button class="btn mt" onclick="location.reload()">重新加载</button></div></div>`;
  }
}

window.addEventListener('hashchange', route);
$('#run-select').addEventListener('change', e => {
  state.run = e.target.value;
  if (state.run) localStorage.setItem('studio.run', state.run);
  else localStorage.removeItem('studio.run');
  loadRuns();
  route();
});
$('#job-pill').addEventListener('click', openJobMonitor);

(async function boot() {
  await loadRuns();
  pollJobs();
  jobTimer = setInterval(pollJobs, 3000);
  route();
})();
