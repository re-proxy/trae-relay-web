'use strict';
const $=id=>document.getElementById(id);let attempt='';
const el=(tag,text,cls)=>{const x=document.createElement(tag);if(text!==undefined)x.textContent=text;if(cls)x.className=cls;return x};
function notice(text){$('notice').textContent=text;$('notice').hidden=false;}
async function api(path,body){const res=await fetch('/admin/'+path,{method:body===undefined?'GET':'POST',headers:body===undefined?{}:{'Content-Type':'application/json'},body:body===undefined?undefined:JSON.stringify(body),credentials:'same-origin'});const data=await res.json();if(!res.ok){if(res.status===401){$('console').hidden=true;$('login').hidden=false;$('logout').hidden=true;$('newKey').value='';$('callback').value='';$('authorization').hidden=true;attempt='';}throw Error(data.error?.message||'请求失败');}return data;}
async function action(fn){try{await fn();}catch(e){notice(e.message);}}
function button(text,fn){const b=el('button',text);b.onclick=()=>action(async()=>{b.disabled=true;try{await fn();}finally{b.disabled=false;}});return b;}
function checkinLabel(a){if(a.checkin_day!==localDay())return {text:'今日未签到',cls:''};switch(a.checkin_result){case 0:return {text:'今日已签到，积分已入账',cls:'green'};case 1:return {text:'今日已签到',cls:'green'};case -2:return {text:'账号不支持签到',cls:''};case -3:return {text:'已提交，积分确认中',cls:''};default:return {text:'签到失败，稍后重试',cls:''};}}
function localDay(){const d=new Date();return d.getFullYear()+String(d.getMonth()+1).padStart(2,'0')+String(d.getDate()).padStart(2,'0');}
function endpoint(){return location.origin+'/v1';}
async function refresh(){const s=await api('state');$('console').hidden=false;$('login').hidden=true;$('logout').hidden=false;$('accountCount').textContent=s.accounts.length;$('inflight').textContent=s.in_flight;$('modelCount').textContent=s.models.length;$('baseURL').textContent=endpoint();$('keyCount').textContent=s.keys.length+' KEYS';
const accounts=$('accountList');accounts.replaceChildren();if(!s.accounts.length)accounts.append(el('p','还没有账号。添加账号并完成官方授权后，在此启用。'));
for(const a of s.accounts){
const card=el('article',undefined,'accountCard'),head=el('div',undefined,'accountHead'),identity=el('div'),avatar=el('span',a.name.slice(0,1).toUpperCase(),'avatar'),title=el('div');title.append(el('strong',a.name),el('small',a.scope==='saas'?'企业 SaaS · '+a.tenant_name:'TRAE CN / 个人账号'));identity.className='identity';identity.append(avatar,title);head.append(identity,el('span',a.enabled?'当前启用':'未启用',a.enabled?'pill active':'pill'));card.append(head);
const metrics=el('div',undefined,'accountMetrics'),health=el('div'),balance=el('div');health.append(el('small','账号连通性'),el('strong',a.check?(a.check.ok?'连接正常':'验证失败'):'尚未验证',a.check?.ok?'green':''));if(a.check)health.append(el('small',a.check.ok?a.check.model_count+' 个模型 · '+a.check.latency_ms+' ms':a.check.error),el('small',new Date(a.check.checked_at).toLocaleString()));
const bal=a.balance;balance.append(el('small',a.scope==='saas'?'套餐额度 / CNY':'剩余积分'),el('strong',bal?.ok?(bal.kind==='personal'?Number(bal.remaining).toLocaleString():bal.plan_quota!==undefined?'¥'+Number(bal.plan_quota).toLocaleString():'控制台查看'):bal?'查询失败':'—'));if(bal)balance.append(el('small',bal.ok?new Date(bal.checked_at).toLocaleString():bal.error));if(a.scope==='saas'){const link=el('a','实际消费 ↗');link.href='https://console.enterprise.trae.cn/personal/usage';link.target='_blank';link.rel='noopener noreferrer';balance.append(link);}metrics.append(health,balance);if(a.scope!=='saas'){const ci=el('div'),label=checkinLabel(a);ci.append(el('small','每日签到'),el('strong',label.text,label.cls),el('small',a.checkin_off?'已关闭自动签到':'自动签到 · 每天 '+(s.checkin_at||'')+'（服务端时间）'));metrics.append(ci);}card.append(metrics);
const actions=el('div',undefined,'actions');if(a.scope!=='saas'){actions.append(button('立即签到',async()=>{const r=await api('account/checkin',{id:a.id});notice(r.message||('签到结果 '+r.result));await refresh();}),button(a.checkin_off?'开启自动签到':'关闭自动签到',async()=>{await api('account/checkin',{id:a.id,action:'auto',enabled:!!a.checkin_off});await refresh();}));}
actions.append(button('验证连通性',async()=>{const result=await api('account/check',{id:a.id});notice(result.ok?'身份和模型目录验证通过（未发送生成请求）':'验证失败：'+result.error);await refresh();}),button('查询余额',async()=>{const result=await api('account/balance',{id:a.id});notice(result.ok?'余额信息已更新':'查询失败：'+result.error);await refresh();}),button(a.enabled?'停用':'启用',async()=>{await api('account',{id:a.id,action:a.enabled?'disable':'enable'});await refresh();}),button('重命名',async()=>{const name=prompt('账号名称',a.name);if(name){await api('account',{id:a.id,action:'rename',name});await refresh();}}),button('删除',async()=>{if(confirm('删除此账号的加密凭据？')){await api('account',{id:a.id,action:'delete'});await refresh();}}));card.append(actions);accounts.append(card);}
const keyList=$('keyList');keyList.replaceChildren();if(!s.keys.length)keyList.append(el('p','尚无访问密钥，创建一个以连接客户端。'));for(const k of s.keys){const row=el('div',undefined,'row'),info=el('div'),actions=el('div',undefined,'actions');info.append(el('strong',k.name),el('code','••••••'+k.suffix),el('span',k.enabled?'可用':'已停用',k.enabled?'pill active':'pill'));actions.append(button(k.enabled?'停用':'启用',async()=>{await api('key',{id:k.id,action:k.enabled?'disable':'enable'});await refresh();}),button('重命名',async()=>{const name=prompt('密钥名称',k.name);if(name){await api('key',{id:k.id,action:'rename',name});await refresh();}}),button('撤销',async()=>{if(confirm('撤销该密钥？使用它的客户端将失去访问权限。')){await api('key',{id:k.id,action:'delete'});await refresh();}}));row.append(info,actions);keyList.append(row);}
renderModels(s);
const usage=$('usageList');usage.replaceChildren();for(const u of [...s.usage].reverse()){const tr=el('tr'),t=u.tokens||{};for(const v of [new Date(u.time).toLocaleString(),u.model,t.prompt_tokens??'—',t.completion_tokens??'—',t.prompt_tokens_details?.cached_tokens??'—',u.success?'成功':'失败 / 取消',u.duration_ms+' ms'])tr.append(el('td',String(v)));usage.append(tr);}}
$('loginForm').onsubmit=e=>{e.preventDefault();action(async()=>{await api('login',{password:$('password').value});$('password').value='';$('notice').hidden=true;await refresh();});};
$('logout').onclick=()=>action(async()=>{await api('logout',{});location.reload();});$('refreshState').onclick=()=>action(refresh);
$('startLogin').onclick=()=>action(async()=>{const p=await api('login/start',{});attempt=p.id;$('authLink').href=p.url;$('authorization').hidden=false;$('callback').value='';$('authorization').scrollIntoView({behavior:'smooth'});});
$('completeForm').onsubmit=e=>{e.preventDefault();action(async()=>{const callback=$('callback').value;$('callback').value='';await api('login/complete',{id:attempt,callback,name:$('accountName').value});attempt='';$('authorization').hidden=true;notice('授权已保存，请启用账号并刷新模型目录');await refresh();});};
$('refreshModels').onclick=()=>action(async()=>{notice('正在请求上游模型目录…');await api('models/refresh',{});notice('模型目录已更新');await refresh();});
$('keyForm').onsubmit=e=>{e.preventDefault();action(async()=>{const button=$('keyForm').querySelector('button');button.disabled=true;try{const result=await api('key',{action:'create',name:$('keyName').value});$('newKey').value=result.api_key;$('newKeyBox').hidden=false;$('keyName').value='';await refresh();}finally{button.disabled=false;}});};$('hideKey').onclick=()=>{$('newKey').value='';$('newKeyBox').hidden=true;};
$('footerURL').textContent=endpoint().replace(/\/v1$/,'');
$('footerURL').title=location.origin;
$('baseURL').onclick=async()=>{try{await navigator.clipboard.writeText(endpoint());notice('BASE URL 已复制：'+endpoint());}catch{notice('复制失败，请手动选择 BASE URL');}};


let modelState=null;
const modelDrafts=new Map();
const rateValue=v=>typeof v==='number'&&Number.isFinite(v)&&v>=0?v.toLocaleString(undefined,{maximumFractionDigits:6})+'×':null;
function rateWindow(w){const clock=n=>String(Math.floor(n/60)).padStart(2,'0')+':'+String(n%60).padStart(2,'0');const days=['','一','二','三','四','五','六','日'];return (w.weekdays?.length?'周'+w.weekdays.map(d=>days[d]).join('、'):'每天')+' '+clock(w.start_minute)+'–'+clock(w.end_minute);}
function pricingPanel(m,max){
const panel=el('div',undefined,'modelPricing'),p=max?m.max_pricing:m.pricing,main=el('div',undefined,'rateMain');main.append(el('span',max?'MAX 基础倍率':'基础倍率'),el('strong',rateValue(p?.standard_rate)||'—'));panel.append(main);
const details=el('div',undefined,'rateDetails');
if(!p){details.append(el('span',max?'上游未提供 Max 计费倍率':'上游未提供计费倍率','rateUnknown'));}
else{
if(p.standard_rate===undefined)details.append(el('span','基础倍率未提供','rateUnknown'));
for(const [label,value] of [['会员',p.member_rate],[p.activity_type==='off_peak'?'闲时活动':'活动',p.activity_rate],['活动会员',p.activity_member_rate]]){const formatted=rateValue(value);if(formatted){const item=el('div');item.append(el('span',label),el('strong',formatted));details.append(item);}}
if(!details.childNodes.length)details.append(el('span','上游目录标准倍率','rateUnknown'));
if(p.off_peak_windows?.length){const conditions=el('small','闲时条件 · 北京时间 '+p.off_peak_windows.map(rateWindow).join(' / '),'rateConditions');details.append(conditions);}
else if(p.activity_rate!==undefined)details.append(el('small',p.activity_type==='off_peak'?'闲时条件未提供，生效情况以上游为准':'活动资格与有效期以上游为准','rateConditions'));
}
panel.append(details);return panel;
}
function renderModels(state){
modelState=state;const list=$('modelList');list.replaceChildren();const query=$('modelSearch').value.trim().toLowerCase(),filter=$('modelFilter').value;
const visible=state.models.filter(m=>{const st=modelDrafts.get(m.id)||state.settings[m.id]||{};return [m.display_name,m.id,m.provider].join(' ').toLowerCase().includes(query)&&(filter==='all'||filter==='enabled'&&!st.disabled||filter==='vision'&&m.vision||filter==='reasoning'&&m.efforts?.length||filter==='max'&&m.max);});
$('modelResults').textContent=visible.length+' / '+state.models.length+' 个模型';
if(!visible.length){const empty=el('div',undefined,'modelEmpty');empty.append(el('span','◇'),el('strong',state.models.length?'没有匹配的模型':'模型目录尚未加载'),el('p',state.models.length?'试试其他关键词或筛选条件。':'启用账号后刷新目录，即可查看可用模型、计费倍率和能力。'));list.append(empty);return;}
for(const m of visible){
const st=modelDrafts.get(m.id)||state.settings[m.id]||{},card=el('article',undefined,'modelCard'+(st.disabled?' isDisabled':'')),head=el('div',undefined,'modelCardHead'),title=el('div',undefined,'modelTitle');title.append(el('small',m.provider||'TRAE · 内置模型'),el('h3',m.display_name),el('code',m.id));
const toggle=el('label',undefined,'modelToggle'),enabled=el('input');enabled.type='checkbox';enabled.checked=!st.disabled;enabled.setAttribute('aria-label',m.id+' 启用');const toggleText=el('span',enabled.checked?'已启用':'已停用');toggle.append(enabled,toggleText);head.append(title,toggle);card.append(head);
const tags=el('div',undefined,'modelTags');if(m.vision)tags.append(el('span','图片输入'));if(m.efforts?.length)tags.append(el('span','思考'));if(m.max)tags.append(el('span','Max'));if(m.context)tags.append(el('span',new Intl.NumberFormat('en',{notation:'compact',maximumFractionDigits:1}).format(m.context)+' 上下文'));if(!tags.childNodes.length)tags.append(el('span','文本'));card.append(tags);
let price=pricingPanel(m,!!st.max);card.append(price);
const form=el('div',undefined,'modelFields'),aliasLabel=el('label','API 别名'),alias=el('input');alias.value=st.alias||'';alias.placeholder=m.id;alias.setAttribute('aria-label',m.id+' 别名');aliasLabel.append(alias);
const effortLabel=el('label','思考档位'),effort=el('select');effort.setAttribute('aria-label',m.id+' 思考档位');const def=el('option',m.default_effort?'默认 · '+m.default_effort:'跟随上游默认');def.value='';effort.append(def);for(const e of m.efforts||[]){const option=el('option',e);option.value=e;effort.append(option);}effort.value=st.effort||'';effort.disabled=!m.efforts?.length;effortLabel.append(effort);form.append(aliasLabel,effortLabel);card.append(form);
const bottom=el('div',undefined,'modelCardBottom'),maxLabel=el('label',undefined,'modelMax'),max=el('input');max.type='checkbox';max.checked=!!st.max;max.disabled=!m.max;max.setAttribute('aria-label',m.id+' Max 模式');maxLabel.append(max,el('span',m.max?'Max 模式':'不支持 Max'));const saveGroup=el('div',undefined,'modelSaveGroup'),dirty=el('small',modelDrafts.has(m.id)?'未保存':'');const save=button('保存偏好',async()=>{const snapshot={disabled:!enabled.checked,alias:alias.value,effort:effort.value,max:max.checked};await api('model',{id:m.id,setting:snapshot});state.settings[m.id]=snapshot;modelDrafts.delete(m.id);dirty.textContent='已保存';notice(m.display_name+' 的偏好已保存');});saveGroup.append(dirty,save);bottom.append(maxLabel,saveGroup);card.append(bottom);
const changed=()=>{modelDrafts.set(m.id,{disabled:!enabled.checked,alias:alias.value,effort:effort.value,max:max.checked});dirty.textContent='未保存';toggleText.textContent=enabled.checked?'已启用':'已停用';card.classList.toggle('isDisabled',!enabled.checked);};alias.oninput=changed;effort.onchange=changed;enabled.onchange=changed;max.onchange=()=>{changed();const next=pricingPanel(m,max.checked);price.replaceWith(next);price=next;};list.append(card);
}}
$('modelSearch').oninput=()=>{if(modelState)renderModels(modelState);};$('modelFilter').onchange=()=>{if(modelState)renderModels(modelState);};

refresh().catch(()=>{});
