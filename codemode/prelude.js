// The prelude runs before every script and evaluates to the hooks glue.c's
// event loop calls: flush hands queued tool calls to the host in one batch,
// and finish turns the script's return value into text. It sees two native
// functions, __cm_print and __cm_batch, and removes them from the global
// object so scripts reach the host only through what is defined here.
//
// The host appends the call: this function is applied to the catalog of
// tools the script may call, [{name, description, parameters}].
((catalog) => {
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

  // The catalog, for scripts that look tools up rather than know them.
  const freeze = (v) => {
    if (v && typeof v === "object") {
      Object.values(v).forEach(freeze);
      Object.freeze(v);
    }
    return v;
  };
  const allTools = freeze(catalog);
  globalThis.ALL_TOOLS = allTools;
  globalThis.describeTool = (name) => allTools.find((t) => t.name === name);
  // searchTools(query) ranks tools by how many of the query's words appear
  // in their name or description.
  globalThis.searchTools = (query) => {
    const words = String(query ?? "").toLowerCase().split(/\s+/).filter(Boolean);
    return allTools
      .map((t) => {
        const hay = (t.name + " " + t.description).toLowerCase();
        return { t, score: words.filter((w) => hay.includes(w)).length };
      })
      .filter(({ score }) => score > 0 || words.length === 0)
      .sort((a, b) => b.score - a.score)
      .map(({ t }) => ({ name: t.name, description: t.description }));
  };

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
})
