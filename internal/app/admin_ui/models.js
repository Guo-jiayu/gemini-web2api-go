// Model administration uses the same persisted registry as all generation APIs.
let modelCatalog = null;
let discoveredModels = [];

pages.models = async () => {
  const data = await api('api/models');
  if (data.error) throw new Error(data.error);
  modelCatalog = data;
  return `<section class="panel">
    <h2>模型管理 <button class="btn" onclick="editModel(-1)">手动添加</button></h2>
    <p class="hint">保存后立即生效，无需重启。API 模型名用于客户端调用，上游名称用于严格模型校验。</p>
    <div class="controls" style="margin:18px 0;display:flex;gap:10px;flex-wrap:wrap;align-items:center">
      <label for="modelAccount">获取账号</label>
      <select id="modelAccount" aria-label="获取模型所用的账号">
        ${(data.accounts || []).map(a => `<option value="${a.id}">${escapeHtml(a.label)}</option>`).join('')}
      </select>
      <button class="btn" id="discoverModelsBtn" onclick="discoverModels()" ${data.accounts.length ? '' : 'disabled'}>获取上游模型列表</button>
    </div>
    <div id="modelDiscoveryStatus" role="status" class="hint">${data.accounts.length ? '选择账号后获取其当前可见模型。获取不会覆盖已有配置。' : '请先在 Cookie 池添加并启用账号。'}</div>
    <div id="discoveredModels"></div>
  </section>
  <section class="panel">
    <h2>当前模型 <span class="hint">${data.models.filter(m => m.enabled).length} 个启用 / ${data.models.length} 个配置</span></h2>
    <div style="overflow-x:auto">
    <table style="min-width:760px"><thead><tr><th style="width:31%">API 模型名</th><th style="width:24%">上游名称</th><th style="width:14%">类型 / 状态</th><th style="width:31%">操作</th></tr></thead>
    <tbody>${data.models.map((m,i) => `<tr>
      <td><code>${escapeHtml(m.id)}</code>${m.id===data.default_model ? '<div class="hint">默认模型</div>' : ''}<div class="hint">${escapeHtml(m.description)}</div></td>
      <td>${escapeHtml(m.upstream || '媒体工具')}<div class="hint">${m.thinking ? '扩展思考' : m.think_wire===4 ? '中等思考' : '普通'}</div></td>
      <td>${modelToolLabel(m.tool)}<div class="hint">${m.enabled ? '已启用' : '已停用'}</div></td>
      <td><div style="display:flex;gap:6px;flex-wrap:wrap">
        <button class="btn muted" onclick="editModel(${i})">编辑</button>
        <button class="btn muted" onclick="toggleModel(${i})">${m.enabled ? '停用' : '启用'}</button>
        <button class="btn danger" onclick="deleteModel(${i})">删除</button>
      </div></td></tr>`).join('')}</tbody></table></div>
  </section>`;
};

function modelToolLabel(tool) {
  return ({0:'文字',14:'生图',21:'音乐',11:'视频',2:'画布'})[tool] || '未知';
}

async function discoverModels() {
  const btn = document.getElementById('discoverModelsBtn');
  const status = document.getElementById('modelDiscoveryStatus');
  const target = document.getElementById('discoveredModels');
  btn.disabled=true; btn.textContent='正在获取…';
  status.textContent='正在读取所选账号的上游模型清单…';
  target.innerHTML='';
  try {
    const data = await api('api/models/discover', {method:'POST',body:JSON.stringify({account_id:Number(document.getElementById('modelAccount').value)})});
    if (data.error) throw new Error(data.error);
    if (!target.isConnected) return;
    discoveredModels=data.models;
    status.textContent=`获取到 ${data.models.length} 个模型 · ${new Date(data.fetched_at*1000).toLocaleTimeString()}。选择“添加”或“更新”，确认后生效。`;
    target.innerHTML=`<div style="overflow-x:auto;margin-top:16px"><table style="min-width:560px"><thead><tr><th>上游模型</th><th>建议 API 模型名</th><th style="width:120px">操作</th></tr></thead><tbody>${data.models.map((m,i)=>`<tr>
      <td>${escapeHtml(m.upstream || m.id)}<div class="hint">${escapeHtml(m.description)}</div></td>
      <td><code>${escapeHtml(m.id)}</code></td>
      <td><button class="btn muted" onclick="importDiscoveredModel(${i})">${modelCatalog.models.some(x=>x.id===m.id) ? '更新' : '添加'}</button></td>
    </tr>`).join('')}</tbody></table></div>`;
  } catch(e) { if(status.isConnected) status.textContent=e.message; }
  finally {btn.disabled=false;btn.textContent='获取上游模型列表';}
}

function importDiscoveredModel(index) {
  const source=discoveredModels[index];
  const existing=modelCatalog.models.findIndex(m=>m.id===source.id);
  const candidate={...(existing>=0 ? modelCatalog.models[existing] : {}), ...source};
  // Discovery supplies routing, not user preferences such as descriptions or enablement.
  if(existing>=0) {
    candidate.description=modelCatalog.models[existing].description;
    candidate.enabled=modelCatalog.models[existing].enabled;
  }
  return editModel(existing,candidate);
}

async function persistModels(rows, revision) {
  const data=await api('api/models',{method:'PUT',body:JSON.stringify({models:rows,revision})});
  if(data.error) throw new Error(data.error);
  modelCatalog=data;
}

async function editModel(index, candidate) {
  const initial=candidate || (index>=0 ? modelCatalog.models[index] : {id:'',hex_id:'',mode:1,upstream:'',description:'',tool:0,thinking:false,think_wire:0,requires_login:true,enabled:true});
  const snapshot=structuredClone(modelCatalog);
  const textField=(name,label,value,hint='')=>`<div class="field"><label class="label" for="model-${name}">${label}</label><input type="text" style="width:100%" id="model-${name}" name="${name}" value="${escapeHtml(String(value ?? ''))}" autocomplete="off">${hint ? `<div class="hint">${hint}</div>` : ''}</div>`;
  const saved=await buildModal({title:index>=0 ? '编辑模型' : '添加模型',wide:true,submitText:'保存并生效',allowEnter:false,
    desc:'保存到数据库，模型列表和后续调用立即使用新配置。',
    onReady:overlay=>overlay.querySelector('.modal').classList.add('model-editor'),
    bodyHTML:`${textField('id','API 模型名',initial.id,'例如 gemini-4-hydrogen；改名后客户端也需要更新。')}
      ${textField('upstream','上游模型名称',initial.upstream,'严格校验使用的名称，例如 4 Hydrogen；媒体工具可留空。')}
      ${textField('description','描述',initial.description)}
      <div class="field"><label class="label" for="model-tool">模型类型</label><select style="width:100%" id="model-tool" name="tool">${[0,14,21,11,2].map(n=>`<option value="${n}" ${initial.tool===n?'selected':''}>${modelToolLabel(n)}</option>`).join('')}</select></div>
      <div class="field"><label class="label" for="model-thinking">思考档位</label><select style="width:100%" id="model-thinking" name="reasoning">
        <option value="normal" ${!initial.thinking && initial.think_wire!==4?'selected':''}>普通</option>
        <option value="high" ${initial.thinking?'selected':''}>扩展思考（输出思考链）</option>
        <option value="medium" ${initial.think_wire===4?'selected':''}>中等思考</option>
      </select><div class="hint">思考档位需上游模型支持；选择档位不会自动更改 API 名称。</div></div>
      <details ${!initial.hex_id?'open':''}><summary style="cursor:pointer;margin:12px 0">高级路由参数</summary>
        ${textField('hex_id','上游路由 ID（16 位 hex）',initial.hex_id,'获取上游列表时自动填入；手动新增需填写有效路由。')}
        ${textField('mode','Mode',initial.mode,'上游模型选择模式，范围 1–32。')}
      </details>
      <label style="display:block;margin:12px 0"><input type="checkbox" name="requires_login" ${initial.requires_login?'checked':''}> 需要登录账号</label><div class="hint">思考档位和媒体工具始终需要登录账号。</div>
      <label style="display:block;margin:12px 0"><input type="checkbox" name="enabled" ${initial.enabled?'checked':''}> 启用此模型</label>`,
    onSubmit:async overlay=>{
      const btn=overlay.querySelector('[data-act=ok]');
      if(btn.disabled)return false;
      const val=name=>overlay.querySelector(`[name="${name}"]`).value.trim();
      const m={...initial,id:val('id'),upstream:val('upstream'),description:val('description'),hex_id:val('hex_id'),mode:Number(val('mode')),tool:Number(val('tool')),
        thinking:val('reasoning')==='high',think_wire:val('reasoning')==='medium'?4:0,
        requires_login:overlay.querySelector('[name=requires_login]').checked,enabled:overlay.querySelector('[name=enabled]').checked};
      if(!Number.isInteger(m.mode))return {error:'Mode 必须是整数'};
      if(snapshot.models.some((x,i)=>i!==index && x.id===m.id))return {error:'API 模型名已存在'};
      const rows=structuredClone(snapshot.models);
      if(index>=0)rows[index]=m;else rows.push(m);
      btn.disabled=true;btn.textContent='正在保存…';
      try {await persistModels(rows,snapshot.revision);return true;}
      catch(e){return {error:e.message};}
      finally{btn.disabled=false;btn.textContent='保存并生效';}
    }});
  if(saved){flash('模型已保存，立即生效');if(currentPage==='models')await refreshPage();}
}

async function toggleModel(index) {
  const snapshot=structuredClone(modelCatalog);
  snapshot.models[index].enabled=!snapshot.models[index].enabled;
  try {await persistModels(snapshot.models,snapshot.revision);flash('模型状态已更新');await refreshPage();}
  catch(e){flash(e.message);}
}

async function deleteModel(index) {
  const snapshot=structuredClone(modelCatalog);
  const id=snapshot.models[index].id;
  if(!await modalConfirm({title:'删除模型',desc:`删除 <code>${escapeHtml(id)}</code> 后，该名称将无法调用。`,confirmText:'删除',danger:true}))return;
  snapshot.models.splice(index,1);
  try{await persistModels(snapshot.models,snapshot.revision);flash('模型已删除');await refreshPage();}
  catch(e){flash(e.message);}
}
