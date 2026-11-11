// The prelude runs before every script and evaluates to the hooks glue.c's
// event loop calls: flush hands queued tool calls to the host in one batch,
// and finish turns the script's return value into text. It sees two native
// functions, __cm_print and __cm_batch, and removes them from the global
// object so scripts reach the host only through what is defined here.
(() => {
  const nativePrint = globalThis.__cm_print;
  const nativeBatch = globalThis.__cm_batch;
  delete globalThis.__cm_print;
  delete globalThis.__cm_batch;

  const queue = [];
  let nextId = 1;

  const callTool = (name, args) =>
    new Promise((resolve) => {
      queue.push({ id: nextId++, name, args: args === undefined ? {} : args, resolve });
    });

  // tools.anything(args) queues a call to the tool of that name and returns
  // a promise for its result, {text, details, isError}. A failed call is a
  // result with isError set, not a rejection.
  globalThis.tools = new Proxy(
    {},
    {
      get: (_, name) => (typeof name === "string" ? (args) => callTool(name, args) : undefined),
      has: () => false,
      ownKeys: () => [],
    },
  );

  const show = (v) => {
    if (typeof v === "string") return v;
    if (v instanceof Error) return `${v.name}: ${v.message}`;
    if (v === undefined) return "undefined";
    try {
      return JSON.stringify(v, null, 2) ?? String(v);
    } catch {
      return String(v);
    }
  };

  globalThis.print = (...args) => nativePrint(args.map(show).join(" ") + "\n");
  globalThis.console = Object.freeze({
    log: globalThis.print,
    info: globalThis.print,
    warn: globalThis.print,
    error: globalThis.print,
    debug: globalThis.print,
  });

  return {
    flush() {
      if (queue.length === 0) return false;
      const batch = queue.splice(0);
      const request = batch.map(({ id, name, args }) => ({ id, name, args }));
      const results = JSON.parse(nativeBatch(JSON.stringify(request)));
      batch.forEach((call, i) => call.resolve(results[i]));
      return true;
    },
    finish(value) {
      return value === undefined ? "" : show(value);
    },
  };
})();
