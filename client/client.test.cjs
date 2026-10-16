"use strict";

const { test, before, after } = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs/promises");
const path = require("node:path");
const os = require("node:os");
const vm = require("node:vm");
const crypto = require("node:crypto");
const { spawn, spawnSync } = require("node:child_process");
const net = require("node:net");
const http = require("node:http");

let core,
  base,
  source,
  rootResource,
  binary,
  server,
  serverURL,
  sourceBytes,
  configPath;
let serverLogs = "";
const MiB = 1024 * 1024;
const sum = (b) => crypto.createHash("sha256").update(b).digest("hex");
const notFound = () => new DOMException("Not found", "NotFoundError");

// A disk-backed adapter with the same commit/abort semantics as browser file
// handles. The browser suite separately runs against actual File System APIs.
class DiskDirectory {
  constructor(p) {
    this.path = p;
    this.name = path.basename(p);
  }
  child(name) {
    assert(!/[\\/]/.test(name) && name !== "." && name !== "..");
    return path.join(this.path, name);
  }
  async getDirectoryHandle(name, { create = false } = {}) {
    const p = this.child(name);
    if (create) await fs.mkdir(p, { recursive: true });
    const stat = await fs.stat(p).catch((e) => {
      if (e.code === "ENOENT") throw notFound();
      throw e;
    });
    if (!stat.isDirectory())
      throw new DOMException("Not a directory", "TypeMismatchError");
    return new DiskDirectory(p);
  }
  async getFileHandle(name, { create = false } = {}) {
    const p = this.child(name);
    if (create) {
      const f = await fs.open(p, "a");
      await f.close();
    }
    const stat = await fs.stat(p).catch((e) => {
      if (e.code === "ENOENT") throw notFound();
      throw e;
    });
    if (!stat.isFile())
      throw new DOMException("Not a file", "TypeMismatchError");
    return new DiskFile(p);
  }
  async removeEntry(name) {
    await fs.unlink(this.child(name));
  }
}
class DiskFile {
  constructor(p) {
    this.path = p;
  }
  async getFile() {
    const p = this.path,
      stat = await fs.stat(p);
    return {
      size: stat.size,
      text: () => fs.readFile(p, "utf8"),
      arrayBuffer: async () => {
        const b = await fs.readFile(p);
        return b.buffer.slice(b.byteOffset, b.byteOffset + b.byteLength);
      },
      slice(start, end) {
        return {
          async arrayBuffer() {
            const f = await fs.open(p, "r");
            try {
              const b = Buffer.alloc(
                Math.max(0, Math.min(stat.size, end) - start),
              );
              await f.read(b, 0, b.length, start);
              return b.buffer.slice(b.byteOffset, b.byteOffset + b.length);
            } finally {
              await f.close();
            }
          },
        };
      },
    };
  }
  async createWritable() {
    const temp = this.path + ".swap-" + crypto.randomUUID(),
      file = await fs.open(temp, "wx");
    let closed = false,
      offset = 0;
    return {
      async write(data) {
        const b =
          typeof data === "string" ? Buffer.from(data) : Buffer.from(data);
        await file.write(b, 0, b.length, offset);
        offset += b.length;
      },
      close: async () => {
        await file.sync();
        await file.close();
        closed = true;
        await fs.rename(temp, this.path);
      },
      async abort() {
        if (!closed) {
          await file.close();
          closed = true;
        }
        await fs.unlink(temp).catch(() => {});
      },
    };
  }
}

async function freePort() {
  const s = net.createServer();
  await new Promise((resolve) => s.listen(0, "127.0.0.1", resolve));
  const p = s.address().port;
  await new Promise((resolve) => s.close(resolve));
  return p;
}
function command(args) {
  const result = spawnSync(binary, args, { encoding: "utf8" });
  assert.equal(result.status, 0, result.stderr);
  return result.stdout;
}
async function waitReady(url) {
  for (let i = 0; i < 100; i++) {
    if (server.exitCode !== null) throw new Error(serverLogs);
    try {
      await new Promise((resolve, reject) =>
        http
          .get(url + "/healthz", (r) => {
            r.resume();
            r.on("end", resolve);
          })
          .on("error", reject),
      );
      return;
    } catch {
      await new Promise((resolve) => setTimeout(resolve, 50));
    }
  }
  throw new Error("Server startup timed out: " + serverLogs);
}
async function rpcFor() {
  const rpc = new core.RPC(
    serverURL.replace("http:", "ws:") + "/ws",
    rootResource.id,
  );
  await rpc.connect();
  return rpc;
}
async function destination() {
  const p = await fs.mkdtemp(path.join(base, "download-"));
  return new DiskDirectory(p);
}
async function run(rpc, root, options = {}) {
  const manifest = rpc.manifest;
  const manager = new core.DownloadManager({ rpc, root, manifest, ...options });
  const result = await manager.run(new Set(manifest.entries.map((e) => e.id)));
  return { manager, result };
}

before(async () => {
  const html = await fs.readFile(path.join(__dirname, "index.html"), "utf8");
  const context = vm.createContext({
    crypto: crypto.webcrypto,
    TextEncoder,
    TextDecoder,
    DOMException,
    AbortController,
    WebSocket,
    URL,
    Uint8Array,
    Uint32Array,
    DataView,
    ArrayBuffer,
    console,
    setTimeout,
    clearTimeout,
  });
  vm.runInContext(html.match(/<script>([\s\S]*?)<\/script>/)[1], context, {
    filename: "client/index.html",
  });
  core = context.WSDownloader;
  binary = path.resolve(
    process.env.WSD_TEST_BINARY ||
      path.join(
        ".cache",
        process.platform === "win32" ? "ws-test.exe" : "ws-test",
      ),
  );
  await fs.access(binary);
  base = await fs.mkdtemp(path.join(os.tmpdir(), "wsdownloader-test-"));
  configPath = path.join(base, "config.yaml");
  command(["init", "--config", configPath]);
  const port = await freePort();
  let adminPort = await freePort();
  while (adminPort === port) adminPort = await freePort();
  serverURL = "http://127.0.0.1:" + port;
  let yaml = await fs.readFile(configPath, "utf8");
  yaml = yaml.replaceAll("127.0.0.1:8080", "127.0.0.1:" + port);
  yaml = yaml.replaceAll("127.0.0.1:8081", "127.0.0.1:" + adminPort);
  await fs.writeFile(configPath, yaml);
  source = path.join(base, "资料");
  await fs.mkdir(path.join(source, "空目录"), { recursive: true });
  sourceBytes = crypto.randomBytes(9 * MiB + 137);
  await fs.writeFile(path.join(source, "数据.bin"), sourceBytes);
  await fs.writeFile(path.join(source, "空文件.txt"), "");
  await fs.writeFile(path.join(source, ".hidden"), "hello");
  rootResource = JSON.parse(command(["add", source, "--config", configPath]));
  server = spawn(binary, ["serve", "--config", configPath], {
    stdio: ["ignore", "pipe", "pipe"],
  });
  server.stdout.on("data", (b) => (serverLogs += b));
  server.stderr.on("data", (b) => (serverLogs += b));
  await waitReady(serverURL);
});
after(async () => {
  if (server && server.exitCode === null) {
    server.kill();
    await new Promise((resolve) => server.once("exit", resolve));
  }
  if (base) await fs.rm(base, { recursive: true, force: true });
});

test("CLI management uses the separate admin port and the download port rejects API calls", async () => {
  const response = await fetch(serverURL + "/api/reload", { method: "POST" });
  assert.equal(response.status, 404);
  await response.body.cancel();
  assert.match(command(["reload", "--config", configPath]), /已重载/);
  const filePath = path.join(base, "management-test.txt");
  await fs.writeFile(filePath, "management test");
  const added = JSON.parse(command(["add", filePath, "--config", configPath]));
  assert(JSON.parse(command(["list", "--config", configPath])).some((r) => r.id === added.id));
  command(["remove", added.id, "--config", configPath]);
  assert.equal(JSON.parse(command(["list", "--config", configPath])).length, 1);
});

test("incremental SHA-256 agrees with native crypto across block boundaries", () => {
  for (const length of [0, 1, 55, 56, 63, 64, 65, 127, 128, 129, MiB + 9]) {
    const bytes = crypto.randomBytes(length),
      sha = new core.SHA256();
    for (let i = 0; i < length; i += 37) sha.update(bytes.subarray(i, i + 37));
    assert.equal(sha.finish(), sum(bytes));
  }
});

test("CRC32C matches standard vectors, including leading zeros and byte slices", () => {
  for (const [bytes, expected] of [
    [Buffer.alloc(0), "00000000"],
    [Buffer.from("123456789"), "e3069283"],
    [Buffer.alloc(32), "8a9136aa"],
    [Buffer.alloc(32, 0xff), "62a8ab43"],
    [Buffer.from("x123456789y").subarray(1, 10), "e3069283"],
  ])
    assert.equal(core.crc32c(bytes), expected);
});

test("a corrupted downloaded chunk is rejected and retried using CRC32C", async () => {
  const rpc = await rpcFor(),
    file = rpc.manifest.entries.find((e) => e.size > MiB),
    request = rpc.request.bind(rpc);
  let requests = 0;
  rpc.request = async (payload) => {
    const result = await request(payload);
    if (payload.type === "block" && ++requests === 1) result.bytes[0] ^= 1;
    return result;
  };
  try {
    const result = await rpc.block(file, 0);
    assert.equal(requests, 2);
    assert.match(result.crc32c, /^[a-f0-9]{8}$/);
    assert.equal(result.sha256, undefined);
    assert.deepEqual(Buffer.from(result.bytes), sourceBytes.subarray(0, 4 * MiB));
  } finally {
    rpc.close();
  }
});

test("page WS address uses config, falls back to its origin, and permits query overrides", async () => {
  const html = await fs.readFile(path.join(__dirname, "index.html"), "utf8"),
    script = html.match(/<script>([\s\S]*?)<\/script>/)[1];
  for (const [configured, protocol, search, expected, wsPath = "/ws", secure = true, picker = secure] of [
    ["", "http:", "", "ws://page.example.com:8080/ws"],
    ["", "http:", "?id=example", "ws://page.example.com:8080/ws", "/ws", false],
    ["", "http:", "", "ws://page.example.com:8080/ws", "/ws", true, false],
    ["", "https:", "", "wss://page.example.com:8080/ws"],
    ["", "https:", "", "wss://page.example.com:8080/download/socket", "/download/socket"],
    ["", "http:", "", "ws://page.example.com:8080/transfer/", "/transfer/"],
    ["wss://downloads.example.com/custom/ws", "https:", "", "wss://downloads.example.com/custom/ws"],
    ["ws://127.0.0.1:9000", "http:", "", "ws://127.0.0.1:9000"],
    ["wss://configured.example.com/ws", "https:", "?server=wss%3A%2F%2Foverride.example.com%2Fws", "wss://override.example.com/ws"],
  ]) {
    const elements = new Map();
    const context = vm.createContext({
      TextEncoder,
      TextDecoder,
      URL,
      URLSearchParams,
      location: {
        protocol,
        host: "page.example.com:8080",
        origin: protocol + "//page.example.com:8080",
        search,
      },
      window: {
        isSecureContext: secure,
        ...(picker ? { showDirectoryPicker() {} } : {}),
        addEventListener() {},
      },
      document: {
        querySelector: (selector) => ({
          content: selector.includes("ws-path") ? wsPath : configured,
        }),
        getElementById(id) {
          if (!elements.has(id))
            elements.set(id, { value: "", addEventListener() {} });
          return elements.get(id);
        },
      },
    });
    vm.runInContext(script, context);
    assert.equal(elements.get("server").value, expected);
    assert.equal(
      elements.get("http-help").hidden,
      protocol !== "http:" || (secure && picker),
    );
    assert.equal(elements.get("choose").disabled, !secure || !picker);
    if (protocol === "http:")
      assert.equal(
        elements.get("http-origin").textContent,
        "http://page.example.com:8080",
      );
    if (!secure) {
      assert.match(elements.get("compatibility").textContent, /下方步骤/);
      assert.equal(elements.get("choose").disabled, true);
    }
    assert.equal(
      context.WSDownloader.normalizeServer("ws://backend.example.com"),
      "ws://backend.example.com" + wsPath,
    );
  }
});

test("WS backend and WSS proxy endpoints are accepted without client certificates", () => {
  assert.equal(
    core.normalizeServer("ws://127.0.0.1:8080"),
    "ws://127.0.0.1:8080/ws",
  );
  assert.equal(
    core.normalizeServer("wss://downloads.example.com"),
    "wss://downloads.example.com/ws",
  );
  for (const value of [
    "https://example.com",
    "wss://user:pass@example.com",
    "wss://example.com?query=1",
  ])
    assert.throws(() => core.normalizeServer(value));
});

test("directory download, unicode, hidden files, empty directories, cleanup and identical-file skip", async () => {
  const root = await destination(),
    rpc = await rpcFor();
  try {
    const { result } = await run(rpc, root);
    assert.equal(result.errors.length, 0);
    assert.equal(result.finished, 3);
    assert.deepEqual(
      await fs.readFile(path.join(root.path, "资料", "数据.bin")),
      sourceBytes,
    );
    assert.equal(
      (await fs.stat(path.join(root.path, "资料", "空目录"))).isDirectory(),
      true,
    );
    assert.deepEqual(
      (await fs.readdir(path.join(root.path, "资料"))).sort(),
      [".hidden", "数据.bin", "空文件.txt", "空目录"].sort(),
    );
    let requests = 0;
    const block = rpc.block.bind(rpc);
    rpc.block = (...args) => {
      requests++;
      return block(...args);
    };
    const second = await run(rpc, root);
    assert.equal(second.result.finished, 3);
    assert.equal(second.result.errors.length, 0);
    assert.equal(requests, 0);
  } finally {
    rpc.close();
  }
});

test("pause persists sibling chunks and reuses them after the WS endpoint changes", async () => {
  const root = await destination(),
    rpc = await rpcFor(),
    file = rpc.manifest.entries.find((e) => e.size > MiB);
  let manager;
  manager = new core.DownloadManager({
    rpc,
    root,
    manifest: rpc.manifest,
    concurrency: 1,
    onEvent: (event) => {
      if (event.type === "progress") manager.pause();
    },
  });
  await assert.rejects(() => manager.run(new Set([file.id])), {
    name: "AbortError",
  });
  const recordPath = path.join(root.path, "资料", "数据.bin._part.json"),
    record = JSON.parse(await fs.readFile(recordPath, "utf8"));
  assert.equal(Object.keys(record.completed).length, 1);
  assert.equal(
    record.completed[0].crc32c,
    core.crc32c(sourceBytes.subarray(0, 4 * MiB)),
  );
  assert.equal(record.completed[0].sha256, undefined);
  record.server = "wss://previous.example.com/old/socket";
  await fs.writeFile(recordPath, JSON.stringify(record));
  assert.equal(
    (await fs.stat(path.join(root.path, "资料", "数据.bin.0._part"))).isFile(),
    true,
  );
  const resumed = await rpcFor();
  let requests = 0;
  const block = resumed.block.bind(resumed);
  resumed.block = (...args) => {
    requests++;
    return block(...args);
  };
  try {
    const result = await run(resumed, root, { concurrency: 1 });
    assert.equal(result.result.errors.length, 0);
    assert.equal(requests, 3);
    assert.deepEqual(
      await fs.readFile(path.join(root.path, "资料", "数据.bin")),
      sourceBytes,
    );
  } finally {
    resumed.close();
  }
});

test("incompatible resume records report the conflicting fields and preserve all artifacts", async () => {
  const root = await destination(),
    rpc = await rpcFor(),
    file = rpc.manifest.entries.find((e) => e.size > MiB),
    directory = path.join(root.path, "资料"),
    partPath = path.join(directory, "数据.bin.0._part"),
    recordPath = path.join(directory, "数据.bin._part.json"),
    part = sourceBytes.subarray(0, 4 * MiB);
  await fs.mkdir(directory);
  await fs.writeFile(partPath, part);
  const original = {
    format: "wsdownloader-v1",
    server: rpc.server,
    resource_id: rpc.manifest.resource_id,
    file_id: file.id,
    path: file.path,
    size: file.size,
    chunk_bytes: rpc.manifest.chunk_bytes,
    completed: { 0: { crc32c: core.crc32c(part), length: part.length } },
  };
  let requests = 0;
  rpc.block = () => {
    requests++;
    throw new Error("unexpected block request");
  };
  try {
    for (const [field, value, reason] of [
      ["format", "unknown", "记录格式"],
      ["resource_id", "different-resource", "资源 ID"],
      ["file_id", "different-file", "文件 ID"],
      ["path", "different/path", "文件路径"],
      ["size", file.size + 1, "文件大小"],
      ["chunk_bytes", 2 * MiB, "分块大小"],
      ["completed", null, "已完成分块记录格式无效"],
    ]) {
      const recordText = JSON.stringify({ ...original, [field]: value });
      await fs.writeFile(recordPath, recordText);
      const manager = new core.DownloadManager({
        rpc, root, manifest: rpc.manifest,
      });
      const result = await manager.run(new Set([file.id]));
      assert.equal(result.errors.length, 1);
      assert(result.errors[0].message.includes(reason), result.errors[0].message);
      if (field === "chunk_bytes") {
        assert(result.errors[0].message.includes(String(2 * MiB)));
        assert(result.errors[0].message.includes(String(4 * MiB)));
      }
      assert.equal(await fs.readFile(recordPath, "utf8"), recordText);
    }
    assert.equal(requests, 0);
    assert.deepEqual(await fs.readFile(partPath), part);
    await assert.rejects(fs.access(path.join(directory, "数据.bin")), {
      code: "ENOENT",
    });
  } finally {
    rpc.close();
  }
});

test("browser-rejected DLL names report a ZIP workaround before any chunk is downloaded", async () => {
  const root = await destination(),
    rpc = await rpcFor(),
    file = rpc.manifest.entries.find((e) => e.size > MiB),
    entry = { ...file, id: "资料/steam_api64.dll", path: "资料/steam_api64.dll" },
    real = DiskDirectory.prototype.getFileHandle;
  DiskDirectory.prototype.getFileHandle = function (name, options) {
    if (name.endsWith(".dll"))
      throw new TypeError(
        "Failed to execute 'getFileHandle': Name is not allowed.",
      );
    return real.call(this, name, options);
  };
  let requests = 0;
  rpc.block = () => {
    requests++;
    throw new Error("unexpected block request");
  };
  try {
    const manager = new core.DownloadManager({
      rpc,
      root,
      manifest: { ...rpc.manifest, entries: [entry] },
    });
    const result = await manager.run(new Set([entry.id]));
    assert.equal(result.errors.length, 1);
    assert.match(result.errors[0].message, /浏览器禁止.*ZIP/);
    assert.equal(requests, 0);
  } finally {
    DiskDirectory.prototype.getFileHandle = real;
    rpc.close();
  }
});

test("legacy SHA-256 chunk records resume without downloading valid chunks again", async () => {
  const root = await destination(),
    rpc = await rpcFor(),
    file = rpc.manifest.entries.find((e) => e.size > MiB);
  let manager;
  manager = new core.DownloadManager({
    rpc,
    root,
    manifest: rpc.manifest,
    concurrency: 1,
    onEvent: (event) => {
      if (event.type === "progress") manager.pause();
    },
  });
  await assert.rejects(() => manager.run(new Set([file.id])), {
    name: "AbortError",
  });
  const recordPath = path.join(root.path, "资料", "数据.bin._part.json"),
    record = JSON.parse(await fs.readFile(recordPath, "utf8"));
  record.completed[0] = {
    sha256: sum(sourceBytes.subarray(0, 4 * MiB)),
    length: 4 * MiB,
  };
  await fs.writeFile(recordPath, JSON.stringify(record));
  const resumed = await rpcFor(),
    block = resumed.block.bind(resumed);
  let requests = 0;
  resumed.block = (...args) => {
    requests++;
    return block(...args);
  };
  try {
    const { result } = await run(resumed, root, { concurrency: 1 });
    assert.equal(result.errors.length, 0);
    assert.equal(requests, 3);
    assert.deepEqual(
      await fs.readFile(path.join(root.path, "资料", "数据.bin")),
      sourceBytes,
    );
  } finally {
    resumed.close();
  }
});

test("corrupted persisted chunks are detected and requested again", async () => {
  const root = await destination(),
    rpc = await rpcFor(),
    file = rpc.manifest.entries.find((e) => e.size > MiB);
  let manager;
  manager = new core.DownloadManager({
    rpc,
    root,
    manifest: rpc.manifest,
    concurrency: 1,
    onEvent: (event) => {
      if (event.type === "progress") manager.pause();
    },
  });
  await assert.rejects(() => manager.run(new Set([file.id])), {
    name: "AbortError",
  });
  await fs.writeFile(
    path.join(root.path, "资料", "数据.bin.0._part"),
    Buffer.alloc(4 * MiB, 17),
  );
  const resumed = await rpcFor();
  let requests = 0;
  const block = resumed.block.bind(resumed);
  resumed.block = (...args) => {
    requests++;
    return block(...args);
  };
  try {
    const result = await run(resumed, root);
    assert.equal(result.result.errors.length, 0);
    assert.equal(requests, 4);
    assert.deepEqual(
      await fs.readFile(path.join(root.path, "资料", "数据.bin")),
      sourceBytes,
    );
  } finally {
    resumed.close();
  }
});

test("an interrupted first merge can resume past its own empty target without overwrite", async () => {
  const root = await destination(),
    rpc = await rpcFor(),
    file = rpc.manifest.entries.find((e) => e.size > MiB);
  let manager;
  manager = new core.DownloadManager({
    rpc,
    root,
    manifest: rpc.manifest,
    concurrency: 1,
    onEvent: (event) => {
      if (event.type === "progress") manager.pause();
    },
  });
  await assert.rejects(() => manager.run(new Set([file.id])), {
    name: "AbortError",
  });
  const target = path.join(root.path, "资料", "数据.bin"),
    recordPath = target + "._part.json";
  const record = JSON.parse(await fs.readFile(recordPath, "utf8"));
  record.target_created = true;
  await fs.writeFile(recordPath, JSON.stringify(record));
  await fs.writeFile(target, "");
  const resumed = await rpcFor();
  try {
    const result = await run(resumed, root);
    assert.equal(result.result.errors.length, 0);
    assert.deepEqual(await fs.readFile(target), sourceBytes);
  } finally {
    resumed.close();
  }
});

test("a committed final file cleans up owned remnants after an interrupted cleanup", async () => {
  const root = await destination(),
    rpc = await rpcFor(),
    file = rpc.manifest.entries.find((e) => e.size > MiB);
  let manager;
  manager = new core.DownloadManager({
    rpc,
    root,
    manifest: rpc.manifest,
    concurrency: 1,
    onEvent: (event) => {
      if (event.type === "progress") manager.pause();
    },
  });
  await assert.rejects(() => manager.run(new Set([file.id])), {
    name: "AbortError",
  });
  const target = path.join(root.path, "资料", "数据.bin");
  await fs.writeFile(target, sourceBytes);
  const recordPath = target + "._part.json",
    record = JSON.parse(await fs.readFile(recordPath, "utf8"));
  record.server = "wss://previous.example.com/old/socket";
  await fs.writeFile(recordPath, JSON.stringify(record));
  const resumed = await rpcFor();
  try {
    const result = await run(resumed, root);
    assert.equal(result.result.errors.length, 0);
    assert.equal(
      (await fs.readdir(path.dirname(target))).some((name) =>
        name.includes("._part"),
      ),
      false,
    );
  } finally {
    resumed.close();
  }
});

test("different existing target is preserved unless overwrite is selected", async () => {
  const root = await destination();
  await fs.mkdir(path.join(root.path, "资料"));
  const target = path.join(root.path, "资料", "数据.bin");
  await fs.writeFile(target, "original");
  const rpc = await rpcFor();
  try {
    const first = await run(rpc, root);
    assert.equal(first.result.errors.length, 1);
    assert.equal(await fs.readFile(target, "utf8"), "original");
    const second = await run(rpc, root, { overwrite: true });
    assert.equal(second.result.errors.length, 0);
    assert.deepEqual(await fs.readFile(target), sourceBytes);
  } finally {
    rpc.close();
  }
});

test("failure during merge preserves original target and committed parts", async () => {
  const root = await destination();
  await fs.mkdir(path.join(root.path, "资料"));
  const target = path.join(root.path, "资料", "数据.bin");
  await fs.writeFile(target, "original");
  const rpc = await rpcFor(),
    file = rpc.manifest.entries.find((e) => e.size > MiB);
  const originalHash = file.sha256;
  file.sha256 = "0".repeat(64);
  try {
    const first = await run(rpc, root, { overwrite: true });
    assert.equal(first.result.errors.length, 1);
    assert.equal(await fs.readFile(target, "utf8"), "original");
    assert.equal((await fs.stat(target + ".0._part")).isFile(), true);
    file.sha256 = originalHash;
    let requests = 0;
    const block = rpc.block.bind(rpc);
    rpc.block = (...args) => {
      requests++;
      return block(...args);
    };
    const second = await run(rpc, root, { overwrite: true });
    assert.equal(second.result.errors.length, 0);
    assert.equal(requests, 0);
    assert.deepEqual(await fs.readFile(target), sourceBytes);
  } finally {
    rpc.close();
  }
});

test("CRC32C catches chunk corruption during merge and preserves the original target", async () => {
  const root = await destination(),
    rpc = await rpcFor(),
    file = rpc.manifest.entries.find((e) => e.size > MiB);
  await fs.mkdir(path.join(root.path, "资料"));
  const target = path.join(root.path, "资料", "数据.bin");
  await fs.writeFile(target, "original");
  const manager = new core.DownloadManager({
    rpc,
    root,
    manifest: rpc.manifest,
    overwrite: true,
    onEvent: (event) => {
      if (event.type === "merging")
        require("node:fs").writeFileSync(
          target + ".0._part",
          Buffer.alloc(4 * MiB, 17),
        );
    },
  });
  try {
    const result = await manager.run(new Set([file.id]));
    assert.equal(result.errors.length, 1);
    assert.match(result.errors[0].message, /合并时分块校验失败/);
    assert.equal(await fs.readFile(target, "utf8"), "original");
    await fs.access(target + ".0._part");
    await fs.access(target + "._part.json");
  } finally {
    rpc.close();
  }
});

test("connection loss is retried without restarting the whole file", async () => {
  const root = await destination(),
    rpc = await rpcFor();
  let closed = false;
  const request = rpc.request.bind(rpc);
  rpc.request = async (payload, ...args) => {
    if (payload.type === "block" && !closed) {
      closed = true;
      rpc.socket.close();
      await new Promise((resolve) => setTimeout(resolve, 20));
      throw new Error("test disconnect");
    }
    return request(payload, ...args);
  };
  try {
    const result = await run(rpc, root);
    assert.equal(closed, true);
    assert.equal(result.result.errors.length, 0);
    assert.deepEqual(
      await fs.readFile(path.join(root.path, "资料", "数据.bin")),
      sourceBytes,
    );
  } finally {
    rpc.close();
  }
});

test("a failed disk commit is never marked as a completed chunk", async () => {
  const root = await destination(),
    rpc = await rpcFor(),
    real = DiskFile.prototype.createWritable;
  DiskFile.prototype.createWritable = async function () {
    const writer = await real.call(this);
    if (this.path.endsWith(".0._part"))
      writer.close = async () => {
        await writer.abort();
        throw new Error("disk full");
      };
    return writer;
  };
  try {
    const result = await run(rpc, root, { concurrency: 1 });
    assert.equal(result.result.errors.length, 2);
    const record = JSON.parse(
      await fs.readFile(
        path.join(root.path, "资料", "数据.bin._part.json"),
        "utf8",
      ),
    );
    assert.equal(record.completed["0"], undefined);
  } finally {
    DiskFile.prototype.createWritable = real;
    rpc.close();
  }
});

test("malicious manifest paths and reserved artifact collisions are rejected", () => {
  const base = { chunk_bytes: 4 * MiB, client_concurrency: 8, entries: [] };
  for (const p of [
    "../escape",
    "root/../escape",
    "/absolute",
    "root\\escape",
    "root//x",
  ])
    assert.throws(() =>
      core.validateManifest({
        ...base,
        entries: [{ id: p, path: p, kind: "file", size: 0, sha256: sum("") }],
      }),
    );
  assert.throws(() =>
    core.validateManifest({
      ...base,
      entries: [
        { id: "a", path: "a", kind: "file", size: 1, sha256: sum("x") },
        { id: "p", path: "a.0._part", kind: "file", size: 0, sha256: sum("") },
      ],
    }),
  );
});
