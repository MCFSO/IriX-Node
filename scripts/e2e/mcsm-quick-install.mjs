// 真实面板「模板市场 Install」→ 公网模板包
const BASE="http://127.0.0.1:23333"; let cookie="", token="";
const api=async(m,p,b)=>{const h={"Content-Type":"application/json","X-Requested-With":"XMLHttpRequest"}; if(cookie)h.Cookie=cookie; if(token)p+=(p.includes("?")?"&":"?")+"token="+encodeURIComponent(token); const r=await fetch(BASE+p,{method:m,headers:h,body:b===undefined?undefined:JSON.stringify(b)}); const sc=r.headers.getSetCookie?.()||[]; if(sc.length)cookie=sc.map(c=>c.split(";")[0]).join("; "); const t=await r.text(); try{return JSON.parse(t)}catch{return t}};
const sleep=(ms)=>new Promise(r=>setTimeout(r,ms));
const NIL="00000000-0000-4000-8000-000000000000";
(async()=>{
  const l=await api("POST","/api/auth/login",{username:"e2eadmin",password:"E2ePassw0rd!"}); token=l.data;
  const d=(await api("GET","/api/service/remote_services_list")).data.find(x=>x.port===24444);
  const link="https://codeload.github.com/MCFSO/IriX-Node/zip/refs/heads/main";
  const name="市场模板服"+Date.now().toString(36);
  const created=await api("POST",`/api/protected_instance/asynchronous?daemonId=${d.uuid}&uuid=${NIL}&task_name=quick_install`,
    {time:0,newInstanceName:name,targetLink:link,setupInfo:{startCommand:"ping 127.0.0.1 -t",stopCommand:"stop"}});
  const taskId=created?.data?.taskId;
  console.log("发起安装:", taskId ? "taskId="+taskId.slice(0,32)+"…" : JSON.stringify(created).slice(0,200), "| 目录 cwd=", created?.data?.instanceConfig?.cwd ?? created?.instanceConfig?.cwd);
  if(!taskId) return;
  let final=null;
  for(let i=0;i<90;i++){
    await sleep(1000);
    const q=await api("POST",`/api/protected_instance/query_asynchronous?daemonId=${d.uuid}&uuid=${NIL}&task_name=quick_install`,{taskId});
    const body=q?.data ?? q;
    const st=body?.status;
    if(i%8===0) console.log("  轮询 status=",st,"下载", JSON.stringify(body?.detail?.downloadProgress?.percentage), "%");
    if(st!==1){ final=body; break; }
  }
  console.log("最终 status:", final?.status, "error:", final?.detail?.error ?? "无");
  const arr=(await api("GET",`/api/service/remote_service_instances?daemonId=${d.uuid}&page=1&page_size=50`)).data;
  const list=arr?.data ?? arr ?? [];
  const made=list.find(x=>x.nickname===name);
  console.log(made ? `✅ 面板列表出现「${made.nickname}」，uuid=${made.instanceUuid}，cwd=${made.cwd ?? "(列表不带)"}` : "❌ 列表里没有新实例");
  if(made){
    const fl=await api("GET",`/api/files/list?daemonId=${d.uuid}&uuid=${made.instanceUuid}&target=/&page=0&page_size=50`);
    const items=(fl?.data?.items ?? []).map(i=>i.name).slice(0,6);
    console.log("实例目录文件:", items.join(", ") || "(空)");
    await api("DELETE",`/api/instance?daemonId=${d.uuid}`,{uuids:[made.instanceUuid],deleteFile:true});
    console.log("已清理测试实例");
  }
})();
