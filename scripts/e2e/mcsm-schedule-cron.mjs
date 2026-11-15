// 完全复刻面板「Specific Time Task」提交：time 是 6 段 cron
const BASE="http://127.0.0.1:23333"; let cookie="", token="";
const api=async(m,p,b)=>{const h={"Content-Type":"application/json","X-Requested-With":"XMLHttpRequest"}; if(cookie)h.Cookie=cookie; if(token)p+=(p.includes("?")?"&":"?")+"token="+encodeURIComponent(token); const r=await fetch(BASE+p,{method:m,headers:h,body:b===undefined?undefined:JSON.stringify(b)}); const sc=r.headers.getSetCookie?.()||[]; if(sc.length)cookie=sc.map(c=>c.split(";")[0]).join("; "); const t=await r.text(); try{return JSON.parse(t)}catch{return t}};
const sleep=(ms)=>new Promise(r=>setTimeout(r,ms));
(async()=>{
  const l=await api("POST","/api/auth/login",{username:"e2eadmin",password:"E2ePassw0rd!"}); token=l.data;
  const d=(await api("GET","/api/service/remote_services_list")).data.find(x=>x.port===24444);
  const list=(await api("GET",`/api/service/remote_service_instances?daemonId=${d.uuid}&page=1&page_size=10`)).data;
  const inst=(list?.data ?? list ?? [])[0];
  if(!inst){ console.log("无实例"); return; }
  // 面板：objTime + 2s → `${s} ${m} ${h} ${dd} ${mm} *`（weekday 固定 *）
  const at=new Date(Date.now()+2000);
  const time=`${at.getSeconds()} ${at.getMinutes()} ${at.getHours()} ${at.getDate()} ${at.getMonth()+1} *`;
  const reg=await api("POST",`/api/protected_schedule?daemonId=${d.uuid}&uuid=${inst.instanceUuid}`,
    {instanceUuid:inst.instanceUuid,name:"e2e-面板cron",count:1,time,type:3,actions:[{type:"start",payload:""}]});
  console.log("注册(面板同款 cron time="+time+"):", JSON.stringify(reg).slice(0,100));
  let fired=false;
  for(let i=0;i<15;i++){
    await sleep(1000);
    const arr=(await api("GET",`/api/service/remote_service_instances?daemonId=${d.uuid}&page=1&page_size=10`)).data;
    const cur=(arr?.data ?? arr ?? []).find(x=>x.instanceUuid===inst.instanceUuid);
    if(cur && cur.status!==0){ fired=true; console.log("实例状态", cur.status, "→ 计划任务已触发"); break; }
  }
  const tasks=(await api("GET",`/api/protected_schedule?daemonId=${d.uuid}&uuid=${inst.instanceUuid}`)).data;
  console.log("剩余任务:", ((tasks?.data ?? tasks)||[]).length, "（count=1 应自动删除）");
  console.log(fired ? "✅ 面板同款 Specific Time 任务可注册并触发" : "❌ 未触发");
  await api("POST",`/api/protected_instance/stop?daemonId=${d.uuid}&uuid=${inst.instanceUuid}`);
})();
