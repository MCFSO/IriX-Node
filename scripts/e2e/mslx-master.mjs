// MSLX 真实 daemon（v1.7.8，主节点）→ irix-node（子节点）端到端验证 v2。
// 覆盖：登录 → 协议探测 → link（主节点主动 POST accept-link）→ 子节点回源核验
// → 节点令牌签发 → 面板式直连探活/数据访问 → 状态上报（主节点 SignalR 广播可见）
// → 解除链接。
import * as signalR from "@microsoft/signalr";

const MASTER = "http://127.0.0.1:1027";
const NODE = "http://127.0.0.1:7891";
const LINK_KEY = "e2e-mslx-linkkey-0123456789";
const ADMIN = { username: "mslx", password: process.env.MSLX_ADMIN_PASS || "HAwTkkjOW6mhjJuU" };

let jwt = "";
let failures = 0;
const log = (...a) => console.log(...a);

async function api(base, method, path, body, token) {
  const headers = { "Content-Type": "application/json" };
  const t = token ?? jwt;
  if (t) {
    headers["Authorization"] = "Bearer " + t;
    headers["x-user-token"] = t;
  }
  const res = await fetch(base + path, {
    method,
    headers,
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const text = await res.text();
  let json;
  try {
    json = text ? JSON.parse(text) : null;
  } catch {
    json = text;
  }
  return { status: res.status, body: json };
}

function step(name, ok, extra = "") {
  log(`${ok ? "✅" : "❌"} ${name}${extra ? " — " + extra : ""}`);
  if (!ok) failures++;
}

const pairingId = [...crypto.getRandomValues(new Uint8Array(16))]
  .map((b) => b.toString(16).padStart(2, "0"))
  .join("");

async function main() {
  const login = await api(MASTER, "POST", "/api/auth/login", ADMIN);
  jwt = login.body?.data?.token ?? "";
  step("MSLX 主节点登录", login.status === 200 && !!jwt, `status=${login.status}`);

  // 1) link（主节点会自己 POST 子节点 /api/node/accept-link，并回源子节点 protocol）
  // 先清掉上一次留下的关系，避免 409「此地址已登记」
  const pre = await api(MASTER, "GET", "/api/node/list");
  for (const n of pre.body?.data ?? []) {
    if (String(n.nodeUrl ?? "").includes("7891")) await api(MASTER, "POST", `/api/node/delete/${n.nodeId}`);
  }
  await new Promise((r) => setTimeout(r, 2500));

  const link = await api(MASTER, "POST", "/api/node/link", {
    nodeUrl: NODE,
    nodeName: "irix-node 子节点",
    pairingId,
    linkKey: LINK_KEY,
  });
  step("主节点发起配对 link", link.status === 200, `status=${link.status} ${link.body?.message ?? ""}`);
  const nodeId =
    link.body?.data?.nodeId ??
    (await api(MASTER, "GET", "/api/node/list")).body?.data?.find((n) => String(n.nodeUrl).includes("7891"))?.nodeId;
  step("拿到子节点 nodeId", !!nodeId, `nodeId=${nodeId}`);

  // 2) 子节点侧 linked 状态
  const st = await api(NODE, "GET", "/api/status");
  step(
    "子节点 linked=true（accept-link 回源核验成功）",
    st.body?.data?.linked === true,
    `linked=${st.body?.data?.linked} nodeId=${st.body?.data?.nodeId}`,
  );

  // 3) 主节点签发节点令牌（面板/主节点代理访问子节点用的就是它）
  const session = await api(MASTER, "POST", "/api/node/session", { nodeId });
  const nodeToken = session.body?.data?.token ?? "";
  step("主节点签发节点令牌", session.status === 200 && !!nodeToken, `status=${session.status}`);

  // 4) 面板式直连：带 x-node-id + x-user-token 访问子节点（跨源、匿名路由）
  const probe = await api(NODE, "GET", "/api/status", undefined, nodeToken);
  const probeHeadersOk = probe.status === 200;
  step("子节点探活 /api/status（面板同款请求）", probeHeadersOk, `status=${probe.status} code=${probe.body?.code}`);

  const instList = await fetch(NODE + "/api/instance/list", {
    headers: { "x-node-id": nodeId, "x-user-token": nodeToken },
  });
  const instBody = await instList.text();
  step(
    "面板数据通路 /api/instance/list",
    instList.status === 200 && instBody.includes('"code":200'),
    `status=${instList.status} body=${instBody.slice(0, 80)}`,
  );

  // 4b) 面板路径下的实例管理：创建 → 列表 → 启动（都用节点令牌，不是本节点 JWT）
  const hdr = { "Content-Type": "application/json", "x-node-id": nodeId, "x-user-token": nodeToken };
  const cwd = "D:\\irix-e2e\\servers\\mslx-panel-" + Date.now().toString(36);
  const createResp = await fetch(NODE + "/api/instance/createServer", {
    method: "POST",
    headers: hdr,
    body: JSON.stringify({ name: "面板建的MSLX服", core: "server.jar", path: cwd, java: "java", minM: 512, maxM: 1024, ignoreEula: true }),
  });
  const createBody = await createResp.json().catch(() => ({}));
  const newId = createBody?.data?.serverId;
  step("节点令牌下创建实例 createServer", createResp.status === 200 && !!newId, `serverId=${newId}`);

  if (newId) {
    // 等后台准备完成（任务置 state=2）
    let taskOk = false;
    for (let i = 0; i < 10 && !taskOk; i++) {
      await new Promise((r) => setTimeout(r, 500));
      const t = await (await fetch(NODE + "/api/tasks", { headers: hdr })).json();
      taskOk = (t.data ?? []).some((x) => x.instanceId === newId && x.state === 2);
    }
    step("创建任务完成（/api/tasks state=2）", taskOk);

    // 换成能长时间运行的命令（createServer 默认拼的是 java -jar，测试环境没有 jar）
    const setCmd = await fetch(NODE + `/api/instance/settings/general/${newId}`, {
      method: "POST",
      headers: hdr,
      body: JSON.stringify({ startCommand: "ping 127.0.0.1 -t", stopCommand: "stop" }),
    });
    step("节点令牌下更新实例启动命令（settings/general）", setCmd.status === 200, `status=${setCmd.status}`);

    const act = await fetch(NODE + "/api/instance/action", {
      method: "POST",
      headers: hdr,
      body: JSON.stringify({ id: newId, action: "start" }),
    });
    const actBody = await act.json().catch(() => ({}));
    step("节点令牌下启动实例", act.status === 200 && actBody?.code === 200, JSON.stringify(actBody).slice(0, 120));

    await new Promise((r) => setTimeout(r, 1500));
    const info = await (await fetch(NODE + `/api/instance/info?id=${newId}`, { headers: hdr })).json();
    step("实例信息 status 为 MSLX 语义的 2（运行中）", info?.data?.status === 2, `status=${info?.data?.status}`);

    // 控制台 Hub：先协商（带节点令牌），再连 WS
    const neg = await fetch(NODE + "/api/hubs/instanceControlHub/negotiate?negotiateVersion=1", {
      method: "POST",
      headers: hdr,
    });
    const negBody = await neg.json().catch(() => ({}));
    step(
      "SignalR 协商接受节点令牌（面板控制台路径）",
      neg.status === 200 && !!negBody?.connectionToken,
      `status=${neg.status}`,
    );

    await fetch(NODE + "/api/instance/action", {
      method: "POST",
      headers: hdr,
      body: JSON.stringify({ id: newId, action: "forceExit" }),
    });
  }

  // 5) 状态上报：连主节点 SignalR SystemMonitorHub，看 slaves 里是否有我们
  const conn = new signalR.HubConnectionBuilder()
    .withUrl(MASTER + "/api/hubs/system", { accessTokenFactory: () => jwt })
    .configureLogging(signalR.LogLevel.None)
    .build();
  let statsSeen = null;
  conn.on("ReceiveSystemStats", (payload) => {
    const slaves = payload?.slaves ?? {};
    if (slaves[nodeId]) statsSeen = slaves[nodeId];
  });
  await conn.start();
  await conn.invoke("JoinMonitor").catch(() => {});
  for (let i = 0; i < 12 && !statsSeen; i++) await new Promise((r) => setTimeout(r, 1000));
  step(
    "主节点收到子节点状态上报（SignalR slaves）",
    !!statsSeen,
    statsSeen ? `cpu=${statsSeen.cpu} memUsed=${statsSeen.memUsed}/${statsSeen.memTotal}` : "未在 10 秒内看到",
  );
  await conn.stop();

  // 6) 解除链接
  const del = await api(MASTER, "POST", `/api/node/delete/${nodeId}`);
  step("主节点解除链接", del.status === 200, `status=${del.status} ${del.body?.message ?? ""}`);

  // 主节点是异步清理（清理循环会回呼子节点 /api/node/remove-link），要给它时间
  let linked = true;
  for (let i = 0; i < 15; i++) {
    await new Promise((r) => setTimeout(r, 1000));
    const st2 = await api(NODE, "GET", "/api/status");
    linked = st2.body?.data?.linked;
    if (linked === false) break;
  }
  step("子节点 linked 归零（主节点异步清理回呼生效）", linked === false, `linked=${linked}`);

  log(`\n=== 失败数：${failures} ===`);
}

main().catch((e) => {
  console.error("harness error:", e);
  process.exitCode = 1;
});
