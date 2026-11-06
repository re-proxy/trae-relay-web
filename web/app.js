'use strict';
const $=id=>document.getElementById(id);
const el=(tag,text,cls)=>{const x=document.createElement(tag);if(text!==undefined)x.textContent=text;if(cls)x.className=cls;return x};
const PAGES=[
{id:'accounts',label:'账号',path:'/',desc:'账号健康、剩余积分与每日签到'},
{id:'models',label:'模型',path:'/models',desc:'上游模型目录与客户端别名'},
{id:'keys',label:'API 接入',path:'/keys',desc:'客户端密钥与 Base URL'},
{id:'usage',label:'使用记录',path:'/usage',desc:'最近 500 次请求'}
];
const PAGE_ALIAS={'/accounts':'/','/access':'/keys'};
let S=null,attempt='',authorization=null,pinnedKey='';

function currentPage(){const p=PAGE_ALIAS[location.pathname]||location.pathname;return PAGES.find(x=>x.path===p)||PAGES[0];}
function endpoint(){return location.origin+'/v1';}
let noticeTimer;
function notice(text){const n=$('notice');n.textContent=text;n.hidden=false;clearTimeout(noticeTimer);noticeTimer=setTimeout(()=>{n.hidden=true;},5000);}
function fmt(v){return v===undefined||v===null?'—':Number(v).toLocaleString();}
function clock(iso){return iso?new Date(iso).toLocaleString():'';}
function localDay(){const d=new Date();return d.getFullYear()+String(d.getMonth()+1).padStart(2,'0')+String(d.getDate()).padStart(2,'0');}
function checkinLabel(a){if(a.checkin_day!==localDay())return {text:'今日未签到',cls:''};switch(a.checkin_result){case 0:return {text:'今日已签到，积分已入账',cls:'green'};case 1:return {text:'今日已签到',cls:'green'};case -2:return {text:'账号不支持签到',cls:''};case -3:return {text:'已提交，积分确认中',cls:''};default:return {text:'签到失败，稍后重试',cls:'warn'};}}

async function api(path,body){const res=await fetch('/admin/'+path,{method:body===undefined?'GET':'POST',headers:body===undefined?{}:{'Content-Type':'application/json'},body:body===undefined?undefined:JSON.stringify(body),credentials:'same-origin'});const data=await res.json().catch(()=>({}));if(!res.ok){if(res.status===401)showLogin();throw Error(data.error?.message||'请求失败');}return data;}
async function action(fn){try{await fn();}catch(e){notice(e.message);}}
function button(text,fn,cls){const b=el('button',text,cls);b.onclick=()=>action(async()=>{b.disabled=true;try{await fn();}finally{b.disabled=false;}});return b;}
function link(text,path){const a=el('a',text);a.href=path;a.onclick=go;return a;}

function go(event){const a=event.currentTarget;if(event.metaKey||event.ctrlKey||event.shiftKey||event.altKey)return;event.preventDefault();if(a.pathname===location.pathname){render();return;}history.pushState({},'',a.pathname);render();}

function showLogin(){S=null;attempt='';authorization=null;pinnedKey='';const p=$('password'),c=$('callback');if(p)p.value='';if(c)c.value='';render();}
async function refresh(){S=await api('state');render();}

/* ---------- page furniture ---------- */
function pageHead(page,extra,actions){const head=el('header',undefined,'pageHead'),title=el('div');title.append(el('h1',page.label),el('p',page.desc+(extra?' · '+extra:'')));const box=el('div',undefined,'pageActions');for(const a of actions)box.append(a);head.append(title,box);return head;}
function emptyBox(title,text,steps){const box=el('section',undefined,'empty');box.append(el('h2',title),el('p',text));if(steps){const ol=el('ol');for(const s of steps)ol.append(el('li',s));box.append(ol);}return box;}
function fact(label,value,note,cls){const d=el('div');d.append(el('dt',label),el('dd',value,cls));if(note)d.append(el('p',note,'factNote'));return d;}

/* ---------- accounts ---------- */
function accountsPage(page){
const s=S,root=el('div',undefined,'page');
const add=el('button','＋ 添加账号','primary');add.onclick=()=>action(startLogin);
root.append(pageHead(page,s.accounts.length?`${s.accounts.length} 个账号 · ${s.accounts.filter(a=>a.enabled).length} 个启用`:'',[button('刷新',refresh),add]));
if(!s.accounts.length)root.append(emptyBox('还没有账号','添加账号并完成官方授权，这条中继才开始转发请求。',['点击「添加账号」，在打开的页面完成 Trae 官方登录','登录后浏览器会跳到本机回调地址，复制地址栏里的完整 URL','粘贴回来提交，凭据加密保存在服务器']));
else{const grid=el('div',undefined,'accountGrid');for(const a of s.accounts)grid.append(accountCard(a,s));root.append(grid);}
if(authorization)root.append(authPanel());
return root;
}

function accountCard(a,s){
const card=el('article',undefined,'accountCard'),head=el('div',undefined,'accountHead'),identity=el('div'),avatar=el('span',a.name.slice(0,1).toUpperCase(),'avatar'),title=el('div');
title.append(el('strong',a.name),el('small',a.scope==='saas'?'企业 SaaS · '+a.tenant_name:'TRAE CN / 个人账号'));
identity.className='identity';identity.append(avatar,title);head.append(identity,el('span',a.enabled?'当前启用':'未启用',a.enabled?'pill active':'pill'));card.append(head);
const facts=el('dl',undefined,'facts');
facts.append(fact('账号连通性',a.check?(a.check.ok?'连接正常':'验证失败'):'尚未验证',a.check?(a.check.ok?a.check.model_count+' 个模型 · '+a.check.latency_ms+' ms':a.check.error)+' · '+clock(a.check.checked_at):'点击「验证连通性」读取账号身份与模型目录',a.check?.ok?'green':''));
const bal=a.balance;
facts.append(fact(a.scope==='saas'?'套餐额度 / CNY':'剩余积分',bal?.ok?(bal.kind==='personal'?fmt(bal.remaining):bal.plan_quota!==undefined?'¥'+fmt(bal.plan_quota):'控制台查看'):bal?'查询失败':'—',bal?(bal.ok?clock(bal.checked_at):bal.error):'点击「查询余额」向官方接口读取',bal?.ok?'':bal?'warn':''));
if(a.scope!=='saas'){const label=checkinLabel(a);facts.append(fact('每日签到',label.text,a.checkin_off?'已关闭自动签到':'自动签到 · 每天 '+(s.checkin_at||'')+'（服务端时间）',label.cls));}
card.append(facts);
if(a.scope==='saas'){const usage=el('a','在官方控制台查看实际消费 ↗');usage.href='https://console.enterprise.trae.cn/personal/usage';usage.target='_blank';usage.rel='noopener noreferrer';usage.className='inlineLink';card.append(usage);}
const actions=el('div',undefined,'actions');
if(a.scope!=='saas')actions.append(button('立即签到',async()=>{const r=await api('account/checkin',{id:a.id});notice(r.message||('签到结果 '+r.result));await refresh();}),button(a.checkin_off?'开启自动签到':'关闭自动签到',async()=>{await api('account/checkin',{id:a.id,action:'auto',enabled:!!a.checkin_off});await refresh();}));
actions.append(button('验证连通性',async()=>{const r=await api('account/check',{id:a.id});notice(r.ok?'身份和模型目录验证通过（未发送生成请求）':'验证失败：'+r.error);await refresh();}),button('查询余额',async()=>{const r=await api('account/balance',{id:a.id});notice(r.ok?'余额信息已更新':'查询失败：'+r.error);await refresh();}),button(a.enabled?'停用':'启用',async()=>{await api('account',{id:a.id,action:a.enabled?'disable':'enable'});await refresh();}),button('重命名',async()=>{const name=prompt('账号名称',a.name);if(name){await api('account',{id:a.id,action:'rename',name});await refresh();}}),button('删除',async()=>{if(confirm('删除此账号的加密凭据？')){await api('account',{id:a.id,action:'delete'});await refresh();}}));
card.append(actions);return card;
}

async function startLogin(){const p=await api('login/start',{});authorization={id:p.id,url:p.url};attempt=p.id;render();const panel=document.querySelector('.authPanel');if(panel)panel.scrollIntoView({behavior:'smooth',block:'center'});}

function authPanel(){
const box=el('section',undefined,'authPanel'),head=el('div',undefined,'authHead');head.append(el('h2','完成官方授权'),el('span','登录链接 30 分钟有效','pill'));
box.append(head);
const ol=el('ol'),one=el('li'),oneLink=el('a','打开 Trae 官方登录页面 ↗');oneLink.href=authorization.url;oneLink.target='_blank';oneLink.rel='noreferrer noopener';one.append(oneLink);ol.append(one,el('li','完成登录后，浏览器会跳转本机回调地址。若显示无法访问，复制地址栏中的完整 URL。'),el('li','将地址粘贴到下方。回调地址含一次性凭据，请仅提交到本管理页。'));
box.append(ol,el('p','容器不监听浏览器本机回调端口。此远程粘贴流程尚未使用真实账号验证。','fine'));
const form=el('form',undefined,'authForm'),nameLabel=el('label','账号名称'),name=el('input');name.maxLength=120;name.placeholder='例如：个人开发账号';
const cbLabel=el('label','完整回调 URL'),callback=el('textarea');callback.required=true;callback.placeholder='http://127.0.0.1:…/authorize?…';callback.spellcheck=false;callback.autocomplete='off';
nameLabel.append(name);cbLabel.append(callback);
const actions=el('div',undefined,'actions'),submit=el('button','提交并加密保存','primary'),cancel=el('button','取消');
submit.type='submit';cancel.type='button';cancel.onclick=()=>{authorization=null;attempt='';render();};actions.append(submit,cancel);
form.append(nameLabel,cbLabel,actions);
form.onsubmit=e=>{e.preventDefault();action(async()=>{submit.disabled=true;try{await api('login/complete',{id:authorization.id,callback:callback.value,name:name.value});authorization=null;attempt='';notice('授权已保存，启用账号后即可刷新模型目录');await refresh();}finally{submit.disabled=false;}});};
box.append(form);return box;
}

/* ---------- models ---------- */
const modelDrafts=new Map();
const modelExpanded=new Set();
function rateText(v){return typeof v==='number'&&Number.isFinite(v)&&v>=0?v.toLocaleString(undefined,{maximumFractionDigits:6})+'×':null;}
function rateWindow(w){
if(!w)return '';
const days=['一','二','三','四','五','六','日'],names=Array.isArray(w.weekdays)?w.weekdays.map(d=>days[d-1]).filter(Boolean):[];
const span=Number.isFinite(w.start_minute)&&Number.isFinite(w.end_minute)?String(Math.floor(w.start_minute/60)).padStart(2,'0')+':'+String(w.start_minute%60).padStart(2,'0')+'–'+String(Math.floor(w.end_minute/60)).padStart(2,'0')+':'+String(w.end_minute%60).padStart(2,'0'):'';
return [names.length?'周'+names.join('、'):'',span].filter(Boolean).join(' ');
}
function pricingPanel(m,max){
const box=el('div',undefined,'rates'),p=max?m.max_pricing:m.pricing;
const main=el('div',undefined,'main'),base=rateText(p&&p.catalog_rate);
main.append(el('span',max?'MAX 基础倍率':'基础倍率'),el('strong',base||'—'));
if(!base)main.append(el('small','上游未提供'));
box.append(main);
if(p){
const detail=el('div',undefined,'detail');
const add=(label,value)=>{if(!value)return;const d=el('div');d.append(el('span',label),el('strong',value));detail.append(d);};
add('会员',rateText(p.member_rate));
add('活动',rateText(p.activity_rate));
add('活动会员',rateText(p.activity_member_rate));
if(detail.children.length)box.append(detail);
const notes=[];
const offPeak=(p.off_peak_windows||[]).map(rateWindow).filter(Boolean).join(' / ');
if(p.activity_type==='off_peak'&&offPeak)notes.push('闲时活动（北京时间）：'+offPeak);
if(p.member_rate!==undefined||p.activity_rate!==undefined)notes.push('会员与活动价需满足上游条件，本页不推算实际扣费。');
if(notes.length)box.append(el('p',notes.join(' '),'cond'));
}
if(!p)box.append(el('p','该档位未返回倍率，不会按 0 或另一档位价格处理。','cond'));
return box;
}
function modelsPage(page){
const root=el('div',undefined,'page'),s=S;
const refreshCatalog=async()=>{notice('正在请求上游模型目录…');await api('models/refresh',{});notice('模型目录已更新');await refresh();};
root.append(pageHead(page,s.models.length?s.models.length+' 个可选':'',[button('刷新',refresh),button('刷新模型目录',refreshCatalog,'primary')]));
if(!s.models.length){root.append(emptyBox('模型目录还没有加载','模型清单来自启用账号的实时上游目录，未加载前不会提供虚构模型。',['确认至少一个账号已启用','点击「刷新模型目录」向上游读取','读取失败会保留上次成功的目录']));return root;}

const view={q:'',filter:'all'};
const bar=el('div',undefined,'toolbar');
const search=el('input');search.type='search';search.placeholder='搜索名称、ID 或提供方';search.setAttribute('aria-label','搜索模型');
const filter=el('select');filter.setAttribute('aria-label','筛选模型');
for(const [value,label] of [['all','全部模型'],['enabled','已启用'],['vision','图片'],['reasoning','思考'],['max','Max']]){const o=el('option',label);o.value=value;filter.append(o);}
const count=el('span','','count');
search.oninput=()=>{view.q=search.value.trim().toLowerCase();paint();};
filter.onchange=()=>{view.filter=filter.value;paint();};
bar.append(search,filter,count);
root.append(bar,el('p','倍率是上游目录给出的计费基数，不是每百万 token 的货币价格。','fine'));

const grid=el('section',undefined,'mGrid');
root.append(grid);
const matches=m=>{
const q=view.q;
if(q&&!(m.display_name+' '+m.id+' '+(m.provider||'')).toLowerCase().includes(q))return false;
switch(view.filter){case 'enabled':return !(s.settings[m.id]||{}).disabled;case 'vision':return !!m.vision;case 'reasoning':return !!(m.efforts&&m.efforts.length);case 'max':return !!m.max;default:return true;}
};
const snapshot=m=>{const st=s.settings[m.id]||{};return {disabled:!!st.disabled,alias:st.alias||'',effort:st.effort||'',max:!!st.max};};
function paint(){
grid.textContent='';
const shown=s.models.filter(matches);
count.textContent=shown.length+' / '+s.models.length+' 个';
if(!shown.length){grid.append(emptyBox('没有匹配的模型','换个关键词或切回「全部模型」继续浏览。',null));return;}
for(const m of shown)grid.append(modelCard(m));
}
function modelCard(m){
const draft=modelDrafts.get(m.id)||snapshot(m),st=draft;
const original=snapshot(m),initialDirty=['disabled','alias','effort','max'].some(k=>st[k]!==original[k]);
const card=el('article',undefined,st.disabled?'mCard off':'mCard');
const head=el('div',undefined,'mHead'),title=el('div',undefined,'mTitle');
title.append(el('small',(m.provider||'上游目录')+' · '+m.function),el('h3',m.display_name),el('code',m.id));
const enabled=el('input');enabled.type='checkbox';enabled.checked=!st.disabled;enabled.setAttribute('aria-label',m.display_name+' 启用');
const toggle=el('label',undefined,'switch');toggle.append(enabled,el('span',st.disabled?'已停用':'启用'));
head.append(title);
const tags=el('div',undefined,'tags');
tags.append(el('span',st.max&&m.max?'Max':'标准','tag'+(st.max&&m.max?' on':'')));
if(m.vision)tags.append(el('span','图片','tag'));
if(m.efforts&&m.efforts.length)tags.append(el('span','思考 · '+(st.effort||m.default_effort||'默认'),'tag'));
if(m.context)tags.append(el('span',Math.round(m.context/1000)+'K 上下文','tag'));
title.append(tags);card.append(head,pricingPanel(m,st.max&&m.max));

const fields=el('div',undefined,'mFields');
const aliasField=el('label'),alias=el('input');alias.value=st.alias;alias.placeholder='客户端请求名，例如 gpt-4o';alias.setAttribute('aria-label',m.display_name+' 别名');aliasField.append(el('span','API 别名'),alias);
const effortField=el('label'),effort=el('select');effort.setAttribute('aria-label',m.display_name+' 思考档位');const def=el('option','跟随默认');def.value='';effort.append(def);for(const e of m.efforts||[]){const o=el('option',e);o.value=e;effort.append(o);}effort.value=st.effort;effortField.append(el('span','思考档位'),effort);
fields.append(aliasField,effortField);


const maxLabel=el('label',undefined,'switch'+(m.max?'':' off')),maxBox=el('input');maxBox.type='checkbox';maxBox.checked=st.max;maxBox.disabled=!m.max;maxBox.setAttribute('aria-label',m.display_name+' Max 模式');maxLabel.append(maxBox,el('span','Max 模式'));
const bottom=el('div',undefined,'mBottom'),hint=el('span',initialDirty?'未保存':'','draft');const save=button('保存偏好',async()=>{
await api('model',{id:m.id,setting:{disabled:!enabled.checked,alias:alias.value,effort:effort.value,max:maxBox.checked}});
modelDrafts.delete(m.id);
s.settings={...s.settings,[m.id]:{disabled:!enabled.checked,alias:alias.value,effort:effort.value,max:maxBox.checked}};
notice('已保存 '+m.display_name+' 的偏好');paint();
});
save.disabled=!initialDirty;
const editor=el('div',undefined,'mEditor');editor.id='model-editor-'+encodeURIComponent(m.id);editor.hidden=!modelExpanded.has(m.id);editor.append(fields,maxLabel);
const expand=el('button',editor.hidden?'偏好 ▾':'收起 ▴');expand.type='button';expand.setAttribute('aria-label',m.display_name+' 偏好');expand.setAttribute('aria-controls',editor.id);expand.setAttribute('aria-expanded',String(!editor.hidden));expand.onclick=()=>{editor.hidden=!editor.hidden;if(editor.hidden)modelExpanded.delete(m.id);else modelExpanded.add(m.id);expand.textContent=editor.hidden?'偏好 ▾':'收起 ▴';expand.setAttribute('aria-expanded',String(!editor.hidden));};
bottom.append(toggle,hint,save,expand);

const touch=()=>{
const base=snapshot(m),next={disabled:!enabled.checked,alias:alias.value,effort:effort.value,max:maxBox.checked};
const dirty=next.disabled!==base.disabled||next.alias!==base.alias||next.effort!==base.effort||next.max!==base.max;
if(dirty)modelDrafts.set(m.id,next);else modelDrafts.delete(m.id);
toggle.lastChild.textContent=next.disabled?'已停用':'启用';
card.className=next.disabled?'mCard off':'mCard';
hint.textContent=dirty?'未保存':'';
save.disabled=!dirty;
paint2(m,next,tags,card);
};
function paint2(m,next,tagsNode,cardNode){
// Max 只影响倍率与标签，就地更新避免失焦
tagsNode.firstChild.textContent=next.max&&m.max?'Max':'标准';
const old=cardNode.querySelector('.rates');if(old)old.replaceWith(pricingPanel(m,next.max&&m.max));
}
enabled.onchange=touch;maxBox.onchange=touch;alias.oninput=touch;effort.onchange=touch;
card.append(bottom,editor);
return card;
}
paint();
return root;
}

/* ---------- keys ---------- */
function keysPage(page){
const s=S,root=el('div',undefined,'page');
root.append(pageHead(page,s.keys.length?s.keys.length+' 把可用':'',[button('刷新',refresh)]));
const endpointBox=el('section',undefined,'endpointBox');
const copy=el('button','复制','copy');copy.onclick=()=>action(async()=>{try{await navigator.clipboard.writeText(endpoint());notice('已复制 '+endpoint());}catch{notice('复制失败，请手动选中 Base URL');}});
const urlRow=el('div',undefined,'endpointRow');urlRow.append(el('span','BASE URL'),el('code',endpoint()),copy);
endpointBox.append(urlRow,el('p','OpenAI 兼容：地址填到客户端的 Base URL，密钥填到 Authorization 的 Bearer 位置，或放进 X-API-Key 请求头。','fine'));
root.append(endpointBox);

const form=el('form',undefined,'keyForm'),label=el('label','密钥名称'),name=el('input');name.maxLength=120;name.placeholder='例如：开发工作站';name.required=true;
label.append(name);const submit=el('button','＋ 创建 API Key','primary');submit.type='submit';form.append(label,submit);
const box=el('div',undefined,'newKeyBox'),key=el('input');key.readOnly=true;key.value=pinnedKey;box.hidden=!pinnedKey;
const boxLabel=el('label','新密钥只显示这一次，现在复制保存');boxLabel.append(key);
const boxActions=el('div',undefined,'actions'),copyKey=el('button','复制密钥','primary'),hide=el('button','已保存，隐藏');
copyKey.type='button';hide.type='button';boxActions.append(copyKey,hide);box.append(boxLabel,boxActions);
copyKey.onclick=()=>action(async()=>{try{await navigator.clipboard.writeText(pinnedKey);notice('密钥已复制');}catch{notice('复制失败，请手动选中密钥');}});
hide.onclick=()=>{pinnedKey='';key.value='';box.hidden=true;};
form.onsubmit=e=>{e.preventDefault();action(async()=>{submit.disabled=true;try{const r=await api('key',{action:'create',name:name.value});pinnedKey=r.api_key;key.value=pinnedKey;box.hidden=false;name.value='';notice('已创建密钥，请复制保存');await refresh();}finally{submit.disabled=false;}});};
root.append(form,box);

const list=el('section',undefined,'list');
if(!s.keys.length)list.append(emptyBox('还没有访问密钥','创建一把密钥交给客户端，它和管理密码互相独立，可以单独停用或撤销。'));
for(const k of s.keys){const row=el('div',undefined,'row'),info=el('div',undefined,'rowInfo');info.append(el('strong',k.name),el('code','••••••'+k.suffix),el('span',k.enabled?'可用':'已停用',k.enabled?'pill active':'pill'));
const actions=el('div',undefined,'actions');actions.append(button(k.enabled?'停用':'启用',async()=>{await api('key',{id:k.id,action:k.enabled?'disable':'enable'});await refresh();}),button('重命名',async()=>{const n=prompt('密钥名称',k.name);if(n){await api('key',{id:k.id,action:'rename',name:n});await refresh();}}),button('撤销',async()=>{if(confirm('撤销该密钥？使用它的客户端将失去访问权限。')){await api('key',{id:k.id,action:'delete'});await refresh();}}));
row.append(info,actions);list.append(row);}
root.append(list,el('p','密钥加密保存，界面只显示末六位。最多保留 32 把，撤销后无法恢复。','fine'));
return root;
}

/* ---------- usage ---------- */
function usagePage(page){
const s=S,root=el('div',undefined,'page');
root.append(pageHead(page,s.usage.length?`${s.usage.length} 条`:'',[button('刷新',refresh)]));
if(!s.usage.length){root.append(emptyBox('还没有请求记录','客户端每调用一次接口，这里就会多一行，包含模型、token 与耗时，便于排查失败。',['在客户端填写 Base URL 与 API Key','发出一次 chat/completions 或 responses 请求','回到这里确认状态与耗时']));return root;}
const wrap=el('div',undefined,'scroll'),table=el('table'),thead=el('thead'),tr=el('tr');
for(const h of ['时间','模型','输入','输出','缓存','状态','耗时'])tr.append(el('th',h));
thead.append(tr);const tbody=el('tbody');
for(const u of [...s.usage].reverse()){const row=el('tr'),t=u.tokens||{};row.append(el('td',new Date(u.time).toLocaleString()),el('td',u.model),el('td',fmt(t.prompt_tokens),'num'),el('td',fmt(t.completion_tokens),'num'),el('td',fmt(t.prompt_tokens_details?.cached_tokens),'num'));
const status=el('td');status.append(el('span',u.success?'成功':'失败 / 取消',u.success?'pill active':'pill bad'));row.append(status,el('td',u.duration_ms+' ms','num'));tbody.append(row);}
table.append(thead,tbody);wrap.append(table);root.append(wrap,el('p','Token 为上游报告值，不推算积分或费用。超过 500 条时最旧的记录会被丢弃。','fine'));
return root;
}

/* ---------- shell ---------- */
function renderService(){
const box=$('service');box.replaceChildren();
const live=el('div',undefined,S?'liveRow':'liveRow off');live.append(el('span',undefined,'dot'),el('strong',S?'服务在线':'未登录'));box.append(live);
if(!S){box.append(el('p','登录后显示进行中请求与账号数量。','svcHint'));return;}
const dl=el('dl',undefined,'svc');
for(const [k,v] of [['进行中',S.in_flight],['启用账号',S.accounts.filter(a=>a.enabled).length],['可选模型',S.models.length],['API Key',S.keys.length]]){const d=el('div');d.append(el('dt',k),el('dd',String(v)));dl.append(d);}
box.append(dl);
}

function render(){
const page=currentPage();
document.title=page.label+' · Trae Relay Web';
const nav=$('nav');nav.replaceChildren();
for(const p of PAGES){const a=el('a',p.label);a.href=p.path;if(p.id===page.id){a.className='on';a.setAttribute('aria-current','page');}a.onclick=go;nav.append(a);}
$('logout').hidden=!S;$('login').hidden=!!S;
const content=$('content');
if(!S){content.hidden=true;content.replaceChildren();renderService();return;}
content.hidden=false;
content.replaceChildren(({accounts:accountsPage,models:modelsPage,keys:keysPage,usage:usagePage})[page.id](page));
content.classList.remove('swap');void content.offsetWidth;content.classList.add('swap');
renderService();
}

$('loginForm').onsubmit=e=>{e.preventDefault();action(async()=>{await api('login',{password:$('password').value});$('password').value='';notice('已进入控制台',false);await refresh();});};
$('logout').onclick=()=>action(async()=>{await api('logout',{});showLogin();notice('已退出管理');});
$('footerURL').textContent=location.origin;$('footerURL').title=location.origin;
window.addEventListener('popstate',()=>render());
render();
$('login').hidden=false;
api('state').then(s=>{S=s;render();}).catch(()=>{});
