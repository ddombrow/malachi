// glue.c is the whole of malachi's C: it runs one script in QuickJS-ng and
// connects it to the Go host. It replaces quickjs-libc, so the guest has no
// std or os module, no filesystem, network, environment or timers: only the
// four imports below.
//
// The script talks to Go through a prelude (codemode/prelude.js) that the
// host passes in. tools.x() calls queue in the prelude and return pending
// promises; when the script has nothing left to run, the event loop here
// asks the prelude to flush them, which hands the whole queue to Go in one
// batch. Go runs the batch concurrently and the promises resolve.

#include <stdint.h>
#include <stdlib.h>
#include <string.h>

#include "quickjs.h"

#define IMPORT(name) __attribute__((import_module("malachi"), import_name(name)))
#define EXPORT(name) __attribute__((export_name(name)))

// batch runs a JSON array of tool calls and returns the length of the JSON
// response, which take then copies into buf; -1 means the host refused the
// batch, and take copies the reason instead.
IMPORT("batch") int32_t host_batch(const char *req, int32_t len);
IMPORT("take") void host_take(char *buf);
// output delivers printed text, the script's result, or its error.
IMPORT("output") void host_output(int32_t kind, const char *p, int32_t len);
// interrupted reports whether the host wants the script stopped.
IMPORT("interrupted") int32_t host_interrupted(void);

enum { OUT_PRINT = 0, OUT_RESULT = 1, OUT_ERROR = 2 };

// cm_run's results.
enum { RUN_OK = 0, RUN_SCRIPT_ERROR = 1, RUN_INTERNAL_ERROR = 2 };

EXPORT("cm_alloc") void *cm_alloc(int32_t n) { return malloc((size_t)n); }
EXPORT("cm_free") void cm_free(void *p) { free(p); }

static int interrupt_handler(JSRuntime *rt, void *opaque) {
  (void)rt;
  (void)opaque;
  return host_interrupted();
}

static JSValue js_print(JSContext *ctx, JSValueConst this_val, int argc, JSValueConst *argv) {
  (void)this_val;
  if (argc < 1)
    return JS_UNDEFINED;
  size_t len;
  const char *s = JS_ToCStringLen(ctx, &len, argv[0]);
  if (!s)
    return JS_EXCEPTION;
  host_output(OUT_PRINT, s, (int32_t)len);
  JS_FreeCString(ctx, s);
  return JS_UNDEFINED;
}

static JSValue js_batch(JSContext *ctx, JSValueConst this_val, int argc, JSValueConst *argv) {
  (void)this_val;
  if (argc < 1)
    return JS_ThrowTypeError(ctx, "batch needs a request");
  size_t len;
  const char *req = JS_ToCStringLen(ctx, &len, argv[0]);
  if (!req)
    return JS_EXCEPTION;
  int32_t n = host_batch(req, (int32_t)len);
  JS_FreeCString(ctx, req);
  int refused = n < 0;
  if (refused)
    n = -n - 1;
  char *buf = malloc((size_t)n + 1);
  if (!buf)
    return JS_ThrowOutOfMemory(ctx);
  host_take(buf);
  JSValue v;
  if (refused)
    v = JS_ThrowInternalError(ctx, "%.*s", (int)n, buf);
  else
    v = JS_NewStringLen(ctx, buf, (size_t)n);
  free(buf);
  return v;
}

// report sends the pending exception as the script's error.
static void report_exception(JSContext *ctx) {
  JSValue exc = JS_GetException(ctx);
  const char *msg = NULL;
  JSValue stack = JS_UNDEFINED;
  if (JS_IsError(exc)) {
    stack = JS_GetPropertyStr(ctx, exc, "stack");
  }
  msg = JS_ToCString(ctx, exc);
  const char *st = JS_IsString(stack) ? JS_ToCString(ctx, stack) : NULL;
  size_t mlen = msg ? strlen(msg) : 0, slen = st ? strlen(st) : 0;
  char *out = malloc(mlen + slen + 2);
  if (out) {
    memcpy(out, msg ? msg : "", mlen);
    size_t n = mlen;
    if (slen) {
      out[n++] = '\n';
      memcpy(out + n, st, slen);
      n += slen;
    }
    host_output(OUT_ERROR, out, (int32_t)n);
    free(out);
  } else {
    host_output(OUT_ERROR, "out of memory", 13);
  }
  if (st)
    JS_FreeCString(ctx, st);
  if (msg)
    JS_FreeCString(ctx, msg);
  JS_FreeValue(ctx, stack);
  JS_FreeValue(ctx, exc);
}

// call invokes fn with one argument, or none when arg is JS_UNDEFINED and
// nargs is 0.
static JSValue call(JSContext *ctx, JSValueConst fn, int nargs, JSValueConst arg) {
  return JS_Call(ctx, fn, JS_UNDEFINED, nargs, &arg);
}

// cm_run runs prelude, which must evaluate to {flush, finish}, then code as
// the body of an async function, until the function's promise settles.
// memory_limit bounds the JS heap in bytes; the wasm page cap backs it.
EXPORT("cm_run")
int32_t cm_run(const char *prelude, int32_t prelude_len, const char *code, int32_t code_len,
               int32_t memory_limit) {
  JSRuntime *rt = JS_NewRuntime();
  if (!rt)
    return RUN_INTERNAL_ERROR;
  JS_SetMemoryLimit(rt, (size_t)memory_limit);
  JS_SetInterruptHandler(rt, interrupt_handler, NULL);
  JSContext *ctx = JS_NewContext(rt);
  if (!ctx) {
    JS_FreeRuntime(rt);
    return RUN_INTERNAL_ERROR;
  }

  int32_t status = RUN_OK;
  JSValue hooks = JS_UNDEFINED, flush = JS_UNDEFINED, finish = JS_UNDEFINED, promise = JS_UNDEFINED;
  char *wrapped = NULL;

  JSValue global = JS_GetGlobalObject(ctx);
  JS_SetPropertyStr(ctx, global, "__cm_print", JS_NewCFunction(ctx, js_print, "__cm_print", 1));
  JS_SetPropertyStr(ctx, global, "__cm_batch", JS_NewCFunction(ctx, js_batch, "__cm_batch", 1));
  JS_FreeValue(ctx, global);

  hooks = JS_Eval(ctx, prelude, (size_t)prelude_len, "prelude.js", JS_EVAL_TYPE_GLOBAL);
  if (JS_IsException(hooks)) {
    report_exception(ctx);
    status = RUN_INTERNAL_ERROR;
    goto done;
  }
  flush = JS_GetPropertyStr(ctx, hooks, "flush");
  finish = JS_GetPropertyStr(ctx, hooks, "finish");
  if (!JS_IsFunction(ctx, flush) || !JS_IsFunction(ctx, finish)) {
    host_output(OUT_ERROR, "prelude lacks flush or finish", 29);
    status = RUN_INTERNAL_ERROR;
    goto done;
  }

  // The script is an async function body, so it can await and return. The
  // opening stays on the first line to keep error line numbers right.
  static const char head[] = "(async () => {", tail[] = "\n})()";
  size_t n = sizeof(head) - 1 + (size_t)code_len + sizeof(tail) - 1;
  wrapped = malloc(n + 1);
  if (!wrapped) {
    status = RUN_INTERNAL_ERROR;
    goto done;
  }
  memcpy(wrapped, head, sizeof(head) - 1);
  memcpy(wrapped + sizeof(head) - 1, code, (size_t)code_len);
  memcpy(wrapped + sizeof(head) - 1 + code_len, tail, sizeof(tail) - 1);
  wrapped[n] = 0;

  promise = JS_Eval(ctx, wrapped, n, "script.js", JS_EVAL_TYPE_GLOBAL);
  if (JS_IsException(promise)) {
    report_exception(ctx);
    status = RUN_SCRIPT_ERROR;
    goto done;
  }

  for (;;) {
    JSContext *jctx;
    int r;
    while ((r = JS_ExecutePendingJob(rt, &jctx)) > 0) {
    }
    if (r < 0) {
      report_exception(jctx);
      status = RUN_SCRIPT_ERROR;
      goto done;
    }
    if (JS_PromiseState(ctx, promise) != JS_PROMISE_PENDING)
      break;
    JSValue more = call(ctx, flush, 0, JS_UNDEFINED);
    if (JS_IsException(more)) {
      report_exception(ctx);
      status = RUN_SCRIPT_ERROR;
      goto done;
    }
    int flushed = JS_ToBool(ctx, more);
    JS_FreeValue(ctx, more);
    if (!flushed) {
      static const char stuck[] = "Error: the script is waiting on a promise that nothing will settle";
      host_output(OUT_ERROR, stuck, sizeof(stuck) - 1);
      status = RUN_SCRIPT_ERROR;
      goto done;
    }
  }

  if (JS_PromiseState(ctx, promise) == JS_PROMISE_REJECTED) {
    JS_Throw(ctx, JS_PromiseResult(ctx, promise));
    report_exception(ctx);
    status = RUN_SCRIPT_ERROR;
    goto done;
  }
  JSValue value = JS_PromiseResult(ctx, promise);
  JSValue text = call(ctx, finish, 1, value);
  JS_FreeValue(ctx, value);
  if (JS_IsException(text)) {
    report_exception(ctx);
    status = RUN_SCRIPT_ERROR;
    goto done;
  }
  size_t tlen;
  const char *t = JS_ToCStringLen(ctx, &tlen, text);
  JS_FreeValue(ctx, text);
  if (!t) {
    report_exception(ctx);
    status = RUN_SCRIPT_ERROR;
    goto done;
  }
  host_output(OUT_RESULT, t, (int32_t)tlen);
  JS_FreeCString(ctx, t);

done:
  free(wrapped);
  JS_FreeValue(ctx, promise);
  JS_FreeValue(ctx, finish);
  JS_FreeValue(ctx, flush);
  JS_FreeValue(ctx, hooks);
  JS_FreeContext(ctx);
  JS_FreeRuntime(rt);
  return status;
}
