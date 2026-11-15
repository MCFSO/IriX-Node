// 用真实面板 API 走一遍「计划任务」：建 → 列表 → 删
const BASE="http://127.0.0.1:23333"; let cookie="", token="";
const api=async(m,p,b)=>{const h={"Content-Type":"application/json","X-Requested-With":"XMLHttpRequest"}; if(cookie)h.Cookie=cookie; if(token)p+=(p.includes("?")?"&":"?")+"token="+encodeURIComponent(token); const r=await fetch(BASE+p,{method:m,headers:h,body:b===undefined?undefined:JSON.stringify(b)}); const sc=r.headers.getSetCookie?.()||[]; if(sc.length)cookie=sc.map(c=>c.split(";")[0]).join("; "); const t=await r.text(); try{return JSON.parse(t)}catch{return t}};
(async()=>{
  const l=await api("POST","/api/auth/login",{username:"e2eadmin",password:"E2ePassw0rd!"}); token=l.data;
  const ds=await api("GET","/api/service/remote_services_list"); const d=(ds.data||[]).find(x=>x.port===24444);
  const insts=await api("GET",`/api/service/remote_service_instances?daemonId=${d.uuid}&page=1&page_size=10`);
  const list=insts.data?.data ?? insts.data ?? [];
  const inst=list[0]; if(!inst){ console.log("没有实例，先在面板建一个"); return; }
  console.log("目标实例:", inst.instanceUuid, inst.nickname, "status", inst.status);

  // 面板「Add Scheduled Task」提交体（Specific Time Task → type=3）
  const body={instanceUuid:inst.instanceUuid,name:"e2e-定时任务",count:1,time:"2026-11-15 08:13:50",type:3,actions:[{type:"stop",payload:""}]};
  const reg=await api("POST",`/api/protected_schedule?daemonId=${d.uuid}&uuid=${inst.instanceUuid}`, body);
  console.log("register:", JSON.stringify(reg).slice(0,160));

  const ls=await api("GET",`/api/protected_schedule?daemonId=${d.uuid}&uuid=${inst.instanceUuid}`);
  const tasks = ls.data?.data ?? ls.data ?? [];
  console.log("list:", JSON.stringify(tasks).slice(0,240));

  const del=await api("DELETE",`/api/protected_schedule?daemonId=${d.uuid}&uuid=${inst.instanceUuid}&task_name=${encodeURIComponent("e2e-定时任务")}`);
  console.log("delete:", JSON.stringify(del).slice(0,120));
})();
