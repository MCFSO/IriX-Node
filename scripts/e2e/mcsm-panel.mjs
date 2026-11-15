// MCSM 真实面板 v10.22.1 → irix-node 兼容层的端到端验证。
// 全程走面板自己的 HTTP API，面板后端再用 socket.io 与 daemon（我们的兼容层）
// 通信，因此这一步同时验证：engine.io 握手/心跳、socket.io 认证、事件回包、
// 实例增删启停、文件事件。
const BASE = "http://127.0.0.1:23333";
const NODE_IP = "127.0.0.1";
const NODE_PORT = 24444;
const NODE_KEY = "e2e-mcsm-key-0123456789abcdef";
const E2E_CWD = process.env.E2E_CWD || "D:\\irix-e2e\\servers\\e2e-panel";

let cookie = "";
let token = "";
const log = (...a) => console.log(...a);
let failures = 0;

async function api(method, path, body) {
  const headers = { "Content-Type": "application/json", "X-Requested-With": "XMLHttpRequest" };
  if (cookie) headers["Cookie"] = cookie;
  // 面板 v10 的鉴权是「会话 cookie + ?token=」，缺一个都 403
  if (token) path += (path.includes("?") ? "&" : "?") + "token=" + encodeURIComponent(token);
  const res = await fetch(BASE + path, {
    method: method === "ALL" ? "POST" : method,
    headers,
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const setCookie = res.headers.getSetCookie?.() ?? [];
  if (setCookie.length) cookie = setCookie.map((c) => c.split(";")[0]).join("; ");
  const text = await res.text();
  let json;
  try {
    json = text ? JSON.parse(text) : null;
  } catch {
    json = text;
  }
  return { status: res.status, body: json, text };
}

function step(name, ok, extra = "") {
  log(`${ok ? "✅" : "❌"} ${name}${extra ? " — " + extra : ""}`);
  if (!ok) failures++;
}

// 面板把 daemon 回包包在 {status, data} 里；取 data 里的列表
function listOf(resp) {
  const d = resp.body?.data;
  if (Array.isArray(d)) return d;
  if (Array.isArray(d?.data)) return d.data;
  return [];
}

async function main() {
  await api("POST", "/api/auth/install", { username: "e2eadmin", password: "E2ePassw0rd!" });
  const login = await api("POST", "/api/auth/login", { username: "e2eadmin", password: "E2ePassw0rd!" });
  if (typeof login.body?.data === "string") token = login.body.data;
  step("面板登录", login.status === 200 && !!cookie && !!token, `token=${token.slice(0, 8)}…`);

  // 1) 注册我们的节点（真实 daemon 注册流程）
  let daemons = listOf(await api("GET", "/api/service/remote_services_list"));
  let daemonId = daemons.find((d) => Number(d.port) === NODE_PORT)?.uuid;
  if (!daemonId) {
    const reg = await api("POST", "/api/service/remote_service", {
      ip: NODE_IP,
      port: NODE_PORT,
      apiKey: NODE_KEY,
      remarks: "irix-node compat",
      prefix: "",
    });
    daemonId = typeof reg.body === "string" ? reg.body : reg.body?.data;
    await new Promise((r) => setTimeout(r, 3000));
  }
  step("节点注册", !!daemonId, `daemonId=${daemonId}`);

  daemons = listOf(await api("GET", "/api/service/remote_services_list"));
  const node = daemons.find((d) => d.uuid === daemonId);
  step("面板显示节点在线（socket.io 认证通过）", !!node?.available, `available=${node?.available}`);

  // 2) 节点系统信息（daemon info/overview 事件）
  const sys = await api("GET", "/api/service/remote_services_system");
  const sysList = Array.isArray(sys.body?.data) ? sys.body.data : [];
  const one = sysList.find((x) => x && typeof x === "object" && x.system);
  step(
    "节点系统信息 info/overview",
    !!one,
    one ? `cpu=${one.system.cpuUsage ?? "?"}% mem=${one.system.totalmem ?? "?"} 实例=${JSON.stringify(one.instance)}` : `count=${sysList.length}`,
  );

  // 3) 创建实例（面板 POST /api/instance → daemon instance/new）
  const cfg = {
    nickname: "e2e面板建的服",
    startCommand: process.platform === "win32" ? "ping 127.0.0.1 -t" : "sh -c 'while true; do echo hi; sleep 1; done'",
    stopCommand: "stop",
    cwd: E2E_CWD,
    ie: "utf-8",
    oe: "utf-8",
    type: "universal",
    tag: [],
    endTime: 0,
    fileCode: "",
    processType: "general",
    updateCommand: "",
    crlf: 2,
    baseCode: "",
    terminalOption: { haveColor: true, pty: false },
    eventTask: { autoStart: false, autoRestart: false, ignore: false },
    docker: { containerName: "", image: "", ports: [], extraVolumes: [], memory: 0, maxSpace: 0, network: 0, io: 0, cpusetCpus: "", cpuUsage: 0 },
  };
  const created = await api("POST", `/api/instance?daemonId=${daemonId}`, cfg);
  let uuid = created.body?.data?.instanceUuid;
  if (!uuid) {
    // 面板重跑时同名目录可能已被占用，换个 cwd 再来一次
    cfg.cwd = E2E_CWD + "-" + Date.now().toString(36);
    const retry = await api("POST", `/api/instance?daemonId=${daemonId}`, cfg);
    uuid = retry.body?.data?.instanceUuid;
    if (!uuid) log("  create 回包:", JSON.stringify(retry.body).slice(0, 200));
  }
  step("面板创建实例", !!uuid, `instanceUuid=${uuid}`);

  // 4) 实例列表
  let insts = listOf(await api("GET", `/api/service/remote_service_instances?daemonId=${daemonId}&page=1&page_size=10`));
  step("面板实例列表", insts.some((i) => i.instanceUuid === uuid), `count=${insts.length}`);

  // 5) 启动
  const start = await api("POST", `/api/protected_instance/open?daemonId=${daemonId}&uuid=${uuid}`);
  step("面板启动实例", !!start.body?.data, `instanceUuid=${start.body?.data?.instanceUuid}`);
  await new Promise((r) => setTimeout(r, 2500));
  insts = listOf(await api("GET", `/api/service/remote_service_instances?daemonId=${daemonId}&page=1&page_size=10`));
  let cur = insts.find((i) => i.instanceUuid === uuid);
  step("实例进入运行态", cur?.status === 3, `status=${cur?.status}（3=运行中）`);

  // 6) 文件列表（daemon file/list 事件）
  const files = await api("GET", `/api/files/list?daemonId=${daemonId}&uuid=${uuid}&target=/&page=0&page_size=100`);
  const items = files.body?.data?.items ?? files.body?.data ?? [];
  step("面板读文件列表 file/list", files.status === 200 && Array.isArray(items), `items=${items?.length}`);

  // 7) 控制台输出（面板 outputlog → daemon 日志缓冲）
  const out = await api("GET", `/api/protected_instance/outputlog?daemonId=${daemonId}&uuid=${uuid}`);
  const outLen = (out.body?.data ?? "").length;
  step("面板读取控制台输出", out.status === 200 && outLen > 0, `len=${outLen}`);

  // 8) 停止 + 删除
  const stop = await api("POST", `/api/protected_instance/stop?daemonId=${daemonId}&uuid=${uuid}`);
  step("面板停止实例", !!stop.body?.data, `instanceUuid=${stop.body?.data?.instanceUuid}`);
  // 停止是异步的（先软关停等待，再强杀）：等它真正进入「已停止」再删
  let stopStatus;
  for (let i = 0; i < 60; i++) {
    await new Promise((r) => setTimeout(r, 1000));
    insts = listOf(await api("GET", `/api/service/remote_service_instances?daemonId=${daemonId}&page=1&page_size=10`));
    cur = insts.find((i) => i.instanceUuid === uuid);
    stopStatus = cur?.status;
    if (stopStatus === 0) break;
  }
  step("实例已停止（异步关停完成）", stopStatus === 0, `status=${stopStatus}`);

  const del = await api("DELETE", `/api/instance?daemonId=${daemonId}`, { uuids: [uuid], deleteFile: false });
  step("面板删除实例", del.status === 200, `status=${del.status}`);

  log(`\n=== 失败数：${failures} ===`);
}

main().catch((e) => {
  console.error("harness error:", e);
  process.exitCode = 1;
});
