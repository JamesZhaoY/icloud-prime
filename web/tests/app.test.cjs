const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");
const { test } = require("node:test");
const ts = require("typescript");
const vue = require("vue");

const source = fs.readFileSync(path.join(__dirname, "../src/App.vue"), "utf8");
const script = source.slice(source.indexOf("type ApiResponse"), source.indexOf("</script>"));
const js = ts.transpile(script, { target: ts.ScriptTarget.ES2022 });
const names = [
  "accounts", "aliases", "messages", "createJobs", "selectedAccountId", "selectedAlias", "selectedFolder",
  "selectedMessageId", "mailLimit", "onlyUnread", "onlyHideMyEmail", "visibleMessages", "extractedCodes",
  "activeTab", "notice", "error", "busy", "accountForm", "accountFormOpen", "cookieValues", "passwordForm",
  "loginForm", "loginAwaitingOtp", "requestLoginSms", "apiToken", "tokenDraft", "authRequired", "cookieStatusLabel", "inboxStatusLabel",
  "loadAccounts", "loadInbox", "loadAliases", "loadCreateJobs", "handleAccountChange", "prefetchMessageBodies",
  "ensureCodeBodies", "addAccount", "removeAccount", "saveCredentials", "saveApiToken", "changeAlias",
  "createAlias", "createAliasBatch", "updateCreateJobStatus", "formatMessageTime", "formatMessageDateTime",
];
const reply = (data, status = 200, message = "request failed") => ({
  status, ok: status >= 200 && status < 300,
  json: async () => ({ success: status < 400, data, message }),
});
const mail = (uid, extra = {}) => ({
  id: `INBOX:${uid}`, uid: String(uid), folder: "INBOX", from: "service@example.com",
  to: "alias@icloud.com", subject: "Mail", date: "2026-09-06T00:00:00Z", preview: "", ...extra,
});
function harness(t, route = () => undefined) {
  const calls = [];
  const storage = new Map();
  const scope = vue.effectScope();
  let remoteAccounts = [{ id: "A", name: "Account A", has_cookies: true, has_app_password: true }, { id: "B", name: "Account B" }];
  const context = {
    ref: vue.ref, computed: vue.computed, watch: vue.watch, onMounted: () => {},
    URLSearchParams, Headers, console,
    localStorage: { getItem: (key) => storage.get(key), setItem: (key, value) => storage.set(key, value), removeItem: (key) => storage.delete(key) },
    window: { confirm: () => true },
    navigator: { clipboard: { writeText: async () => {} } },
    fetch: async (url, init) => {
      const call = { url, init, body: init.body ? JSON.parse(init.body) : undefined };
      calls.push(call);
      const routed = route(call);
      if (routed !== undefined) return await routed;
      if (url === "/api/accounts") return reply(remoteAccounts);
      if (url.startsWith("/api/aliases?")) return reply({ aliases: [] });
      if (url.startsWith("/api/create/jobs?")) return reply({ jobs: [], remaining_this_hour: 5 });
      if (url.startsWith("/api/mailboxes?")) return reply({ folders: [] });
      if (url.startsWith("/api/inbox?")) return reply({ messages: [], count: 0, method: "imap" });
      return reply({});
    },
  };
  const app = scope.run(() => vm.runInNewContext(`(function() { ${js}; return { ${names.join(",")} }; })()`, context));
  t.after(() => scope.stop());
  return { app, calls, storage, setAccounts: (value) => { remoteAccounts = value; } };
}

test("login explains a proxy HTML error and requests JSON", async (t) => {
  const { app, calls } = harness(t, ({ url }) => url.endsWith("/login") ? {
    status: 502, ok: false,
    json: async () => { throw new SyntaxError("Unexpected token '<'"); },
  } : undefined);
  app.selectedAccountId.value = "A";
  app.loginForm.value = { password: "test input", otp_code: "" };
  await app.saveCredentials("login");
  assert.match(app.error.value, /无法完成 Apple ID 登录.*HTTP 502/);
  assert.equal(calls[0].init.headers.get("Accept"), "application/json");
});

test("empty installation opens account creation, then creates and selects an account", async (t) => {
  const { app, calls, setAccounts } = harness(t, ({ url, init }) => url === "/api/accounts" && init.method === "POST"
    ? reply({ id: "new", name: "New account", has_cookies: true }) : undefined);
  setAccounts([]);
  await app.loadAccounts();
  assert.equal(app.activeTab.value, "account");
  assert.equal(app.accountFormOpen.value, true);
  const cookies = { "X-APPLE-WEBAUTH-USER": "v=1:s=1:d=42" };
  app.accountForm.value = { name: " New account ", real_email: "apple@example.com", host: "icloud.com", proxy: "", cookies };
  await app.addAccount();
  assert.equal(app.selectedAccountId.value, "new");
  assert.equal(app.accountFormOpen.value, false);
  const create = calls.find((call) => call.init.method === "POST");
  assert.equal(create.body.name, "New account");
  assert.equal(create.body.real_email, "apple@example.com");
  assert.deepEqual(JSON.parse(create.body.cookies), cookies);
  assert.equal(Object.keys(app.accountForm.value.cookies).length, 0);
});

test("account and inbox responses cannot replace data after account switching", async (t) => {
  const pending = [];
  const { app, calls } = harness(t, ({ url }) => url.startsWith("/api/inbox?")
    ? new Promise((resolve) => pending.push(resolve)) : undefined);
  app.selectedAccountId.value = "A";
  const a = app.loadInbox();
  app.selectedAccountId.value = "B";
  const b = app.loadInbox();
  pending[1](reply({ messages: [mail(202, { subject: "B mail", body: "B body" })], count: 1 }));
  await b;
  pending[0](reply({ messages: [mail(101, { subject: "A mail" })], count: 1 }));
  await a;
  assert.equal(app.messages.value[0].subject, "B mail");
  assert.equal(calls.filter((call) => call.url === "/api/messages").length, 0);
});

test("later inbox request wins within the same account and owns busy state", async (t) => {
  const pending = [];
  const { app } = harness(t, ({ url }) => url.startsWith("/api/inbox?")
    ? new Promise((resolve) => pending.push(resolve)) : undefined);
  app.selectedAccountId.value = "A";
  const a = app.loadInbox("first@icloud.com");
  const b = app.loadInbox("second@icloud.com");
  pending[0](reply({ messages: [mail(1, { body: "first" })], count: 1 }));
  await a;
  assert.equal(app.busy.value.inbox, true);
  pending[1](reply({ messages: [mail(2, { body: "second" })], count: 1 }));
  await b;
  assert.equal(app.messages.value[0].body, "second");
  assert.equal(app.busy.value.inbox, false);
});

test("old alias/job reads and old job actions cannot act on a newly selected account", async (t) => {
  const pending = [];
  const { app, calls } = harness(t, ({ url }) => url.startsWith("/api/aliases?") || url.startsWith("/api/create/jobs?")
    ? new Promise((resolve) => pending.push(resolve)) : undefined);
  app.selectedAccountId.value = "A";
  const aliasRead = app.loadAliases();
  const jobsRead = app.loadCreateJobs();
  app.selectedAccountId.value = "B";
  pending[0](reply({ aliases: [{ email: "A@icloud.com" }] }));
  pending[1](reply({ jobs: [{ id: "A-job", account_id: "A" }], remaining_this_hour: 1 }));
  await Promise.all([aliasRead, jobsRead]);
  assert.equal(app.aliases.value.length, 0);
  assert.equal(app.createJobs.value.length, 0);
  await app.updateCreateJobStatus({ id: "A-job", account_id: "A" }, "pause");
  assert.equal(calls.filter((call) => call.init.method === "POST").length, 0);
});

test("default prefetch is bounded; codes tab loads the rest in batches, including codes after message 50", async (t) => {
  const { app, calls } = harness(t, ({ url, body }) => url === "/api/messages" ? reply({
    messages: body.messages.map(({ uid, folder }) => mail(uid, { folder, body: uid === "100" ? "Verification code 123456" : "" })),
  }) : undefined);
  app.selectedAccountId.value = "A";
  app.mailLimit.value = 100;
  app.messages.value = Array.from({ length: 100 }, (_, index) => mail(index + 1));
  await app.prefetchMessageBodies();
  assert.equal(calls.length, 1);
  assert.deepEqual(calls[0].body.messages.map((m) => m.uid), ["1", "2", "3", "4", "5"]);
  await app.ensureCodeBodies();
  await new Promise((resolve) => setImmediate(resolve));
  assert.deepEqual(calls.map((call) => call.body.messages.length), [5, 50, 45]);
  assert.ok(calls.slice(1).every((call) => call.body.messages.every((m) => Number(m.uid) > 5)));
  assert.equal(app.messages.value[99].body, "Verification code 123456");
  assert.equal(app.extractedCodes.value[0].code, "123456");
});

test("default prefetch puts the selected message first and stays within the bound", async (t) => {
  const { app, calls } = harness(t, ({ url, body }) => url === "/api/messages" ? reply({
    messages: body.messages.map(({ uid, folder }) => mail(uid, { folder, body: `body ${uid}` })),
  }) : undefined);
  app.selectedAccountId.value = "A";
  app.messages.value = Array.from({ length: 30 }, (_, index) => mail(index + 1));
  await app.prefetchMessageBodies({ priorityId: "INBOX:20" });
  assert.equal(calls.length, 1);
  assert.deepEqual(calls[0].body.messages.map((m) => m.uid), ["20", "1", "2", "3", "4"]);
  assert.equal(app.messages.value[19].body, "body 20");
  assert.equal(app.messages.value[10].body, undefined);
});

test("opening the inbox fetches only a few bodies; the codes path fetches all", async (t) => {
  const inbox = Array.from({ length: 40 }, (_, index) => mail(index + 1));
  const { app, calls } = harness(t, ({ url, body }) => {
    if (url.startsWith("/api/inbox?")) return reply({ messages: inbox, count: inbox.length, method: "imap" });
    if (url === "/api/messages") return reply({ messages: body.messages.map(({ uid, folder }) => mail(uid, { folder, body: "b" })) });
    return undefined;
  });
  app.selectedAccountId.value = "A";
  await app.loadInbox();
  await new Promise((resolve) => setImmediate(resolve));
  const bodyCalls = () => calls.filter((call) => call.url === "/api/messages");
  assert.deepEqual(bodyCalls().map((call) => call.body.messages.length), [5]);
  await app.loadInbox(undefined, { prefetchBody: true });
  await new Promise((resolve) => setImmediate(resolve));
  assert.deepEqual(bodyCalls().map((call) => call.body.messages.length), [5, 40]);
});

test("body responses cannot merge by colliding UID across accounts", async (t) => {
  let respond;
  const { app, calls } = harness(t, ({ url }) => url === "/api/messages" ? new Promise((resolve) => { respond = resolve; }) : undefined);
  app.selectedAccountId.value = "A";
  app.messages.value = Array.from({ length: 100 }, (_, index) => mail(index + 1));
  const loading = app.prefetchMessageBodies();
  app.selectedAccountId.value = "B";
  app.messages.value = [mail(1, { subject: "B mail" })];
  respond(reply({ messages: [mail(1, { body: "A secret" })] }));
  await loading;
  assert.equal(app.messages.value[0].body, undefined);
  assert.equal(calls.length, 1);
});

test("unread requires known unread state and HME filter matches actual alias recipients", (t) => {
  const { app } = harness(t);
  app.aliases.value = [{ email: "alias@icloud.com" }];
  app.messages.value = [mail(1, { unread: true }), mail(2, { unread: false }), mail(3), mail(4, { unread: true, to: "personal@icloud.com" }), mail(5, { unread: true, to: "prefixalias@icloud.com" })];
  app.onlyUnread.value = true;
  app.onlyHideMyEmail.value = true;
  assert.equal(app.visibleMessages.value.length, 1);
  assert.equal(app.visibleMessages.value[0].uid, "1");
});

test("API token can recover initial 401 and is sent with JSON write requests", async (t) => {
  const { app, calls, storage } = harness(t, ({ url, init }) => url === "/api/accounts"
    ? init.headers.get("Authorization") === "Bearer secret" ? reply([{ id: "A", name: "A" }]) : reply(null, 401, "token required") : undefined);
  await app.loadAccounts();
  assert.equal(app.authRequired.value, true);
  assert.equal(app.activeTab.value, "settings");
  app.tokenDraft.value = " secret ";
  await app.saveApiToken();
  assert.equal(app.authRequired.value, false);
  assert.equal(app.selectedAccountId.value, "A");
  assert.equal(storage.get("icloud-prime.api-token"), "secret");
  app.passwordForm.value = { icloud_email: "personal@icloud.com", app_password: "app-pass" };
  await app.saveCredentials("password");
  const write = calls.find((call) => call.url.endsWith("/password"));
  assert.equal(write.init.headers.get("Authorization"), "Bearer secret");
  assert.equal(write.body.app_password, "app-pass");
  assert.equal(app.passwordForm.value.app_password, "");
});

test("credential writes use existing API methods, preserve cookie equals, and redact inputs", async (t) => {
  const { app, calls } = harness(t);
  await app.loadAccounts();
  app.cookieValues.value = { "X-APPLE-WEBAUTH-TOKEN": " abc== ", "X-APPLE-WEBAUTH-USER": "v=1:s=1:d=42", "X-APPLE-WEBAUTH-HSA-TRUST": " " };
  await app.saveCredentials("cookies");
  const cookieCall = calls.find((call) => call.url.endsWith("/cookies"));
  assert.equal(cookieCall.init.method, "PUT");
  assert.deepEqual(cookieCall.body.cookies, { "X-APPLE-WEBAUTH-TOKEN": "abc==", "X-APPLE-WEBAUTH-USER": "v=1:s=1:d=42" });
  assert.equal(Object.keys(app.cookieValues.value).length, 0);
  app.loginForm.value = { password: "apple-password", otp_code: "" };
  await app.saveCredentials("login");
  const login = calls.find((call) => call.url.endsWith("/login"));
  assert.equal(login.body.password, "apple-password");
  assert.equal(login.body.otp_code, undefined);
  assert.equal(app.loginForm.value.password, "");
});

test("login is two-step: password first, then only the code on the kept handshake", async (t) => {
  let step = 0;
  const { app, calls } = harness(t, ({ url }) => {
    if (!url.endsWith("/login")) return undefined;
    step++;
    return step === 1 ? reply({ id: "A", needs_otp: true }) : reply({ id: "A" });
  });
  await app.loadAccounts();
  app.loginForm.value = { password: "pw-1", otp_code: "" };
  await app.saveCredentials("login");
  assert.equal(app.loginAwaitingOtp.value, true);
  assert.equal(app.loginForm.value.password, "", "password must not stay in memory");
  assert.match(app.notice.value, /获取验证码/);
  app.loginForm.value.otp_code = " 123456 ";
  await app.saveCredentials("login");
  const logins = calls.filter((call) => call.url.endsWith("/login"));
  assert.equal(logins.length, 2);
  assert.deepEqual(logins[0].body, { password: "pw-1" });
  assert.deepEqual(logins[1].body, { otp_code: "123456" }, "code step must not resend the password");
  assert.equal(app.loginAwaitingOtp.value, false);
  assert.equal(app.notice.value, "登录成功");
});

test("expired pending login returns to the password step", async (t) => {
  const { app } = harness(t, ({ url }) => url.endsWith("/login") ? reply(null, 410, "登录会话已过期,请重新用密码登录") : undefined);
  await app.loadAccounts();
  app.loginAwaitingOtp.value = true;
  app.loginForm.value = { password: "", otp_code: "123456" };
  await app.saveCredentials("login");
  assert.equal(app.loginAwaitingOtp.value, false);
  assert.match(app.error.value, /已过期/);
});

test("wrong code keeps the code step", async (t) => {
  const { app } = harness(t, ({ url }) => url.endsWith("/login") ? reply(null, 422, "验证码错误") : undefined);
  await app.loadAccounts();
  app.loginAwaitingOtp.value = true;
  app.loginForm.value = { password: "", otp_code: "000000" };
  await app.saveCredentials("login");
  assert.equal(app.loginAwaitingOtp.value, true);
  assert.match(app.error.value, /验证码错误/);
});

test("account deletion chooses next account and clears old mailbox state", async (t) => {
  const { app, calls } = harness(t);
  await app.loadAccounts();
  app.messages.value = [mail(1, { body: "A body" })];
  await app.removeAccount();
  assert.equal(app.selectedAccountId.value, "B");
  assert.equal(app.messages.value.length, 0);
  assert.ok(calls.some((call) => call.url === "/api/accounts/A" && call.init.method === "DELETE"));
});

test("late creation response does not select its alias after account switching", async (t) => {
  let respond;
  const { app, calls } = harness(t, ({ url }) => url === "/api/create" ? new Promise((resolve) => { respond = resolve; }) : undefined);
  app.selectedAccountId.value = "A";
  const creating = app.createAlias();
  app.selectedAccountId.value = "B";
  respond(reply({ account_id: "A", email: "created@icloud.com" }));
  await creating;
  assert.equal(app.selectedAlias.value, "");
  assert.equal(calls.length, 1);
});

test("partial batch success retains successful alias and upstream error notice", async (t) => {
  const { app } = harness(t, ({ url }) => url === "/api/create/batch" ? reply({
    created: [{ email: "created@icloud.com" }], created_count: 1, skipped_count: 4,
    remaining_this_hour: 4, last_error: "upstream failed",
  }) : url.startsWith("/api/aliases?") ? reply({ aliases: [{ email: "created@icloud.com" }] }) : undefined);
  app.selectedAccountId.value = "A";
  await app.createAliasBatch();
  assert.equal(app.selectedAlias.value, "created@icloud.com");
  assert.match(app.notice.value, /upstream failed/);
});

test("cached bodies preserve the current inbox read state", async (t) => {
  const { app } = harness(t, ({ url }) => url === "/api/messages"
    ? reply({ messages: [mail(1, { unread: true, body: "cached body" }), mail(2, { unread: false, body: "cached body" })] }) : undefined);
  app.selectedAccountId.value = "A";
  app.messages.value = [mail(1, { unread: false }), mail(2, { unread: true })];
  await app.prefetchMessageBodies();
  app.onlyUnread.value = true;
  assert.equal(app.visibleMessages.value.length, 1);
  assert.equal(app.visibleMessages.value[0].uid, "2");
});

test("known alias search from Web API remains visible when recipient header is unavailable", async (t) => {
  const { app } = harness(t, ({ url }) => url.startsWith("/api/inbox?")
    ? reply({ messages: [mail(1, { uid: undefined, to: "", body: "Web preview" })], count: 1, method: "web_api" }) : undefined);
  app.selectedAccountId.value = "A";
  app.aliases.value = [{ email: "alias@icloud.com" }];
  app.onlyHideMyEmail.value = true;
  await app.loadInbox("alias@icloud.com");
  assert.equal(app.visibleMessages.value.length, 1);
  await app.loadInbox();
  assert.equal(app.visibleMessages.value.length, 0);
});

test("old unauthorized response cannot undo a successful reconnection", async (t) => {
  let respond;
  let accountReads = 0;
  const { app } = harness(t, ({ url }) => url === "/api/accounts" && accountReads++ === 0
    ? new Promise((resolve) => { respond = resolve; }) : undefined);
  const oldRead = app.loadAccounts();
  await app.saveApiToken();
  respond(reply(null, 401, "old failure"));
  await oldRead;
  assert.equal(app.authRequired.value, false);
  assert.equal(app.selectedAccountId.value, "A");
});

test("credential validation warnings and partial folder warnings stay visible", async (t) => {
  const { app } = harness(t, ({ url }) => url.endsWith("/cookies") ? reply({ warning: "Cookie could not be validated" })
    : url.startsWith("/api/inbox?") ? reply({ messages: [], count: 0, warning: "Junk folder unavailable" }) : undefined);
  await app.loadAccounts();
  app.cookieValues.value = { "X-APPLE-WEBAUTH-TOKEN": "abc" };
  await app.saveCredentials("cookies");
  assert.match(app.notice.value, /Cookie could not be validated/);
  await app.loadInbox();
  assert.equal(app.notice.value, "Junk folder unavailable");
});

test("alias controls invoke account-scoped existing endpoints", async (t) => {
  const { app, calls } = harness(t);
  app.selectedAccountId.value = "A";
  for (const action of ["deactivate", "reactivate", "delete"]) {
    app.aliases.value = [{ anonymousId: "alias/1", email: "alias@icloud.com" }];
    await app.changeAlias(app.aliases.value[0], action);
  }
  const writes = calls.filter((call) => call.init.method);
  assert.deepEqual(writes.map((call) => call.url), ["/api/aliases/alias%2F1/deactivate", "/api/aliases/alias%2F1/reactivate", "/api/aliases/alias%2F1"]);
  assert.ok(writes.every((call) => call.body.account_id === "A"));
  assert.equal(writes[2].init.method, "DELETE");
});

test("SMS request keeps the code step and shows the masked phone", async (t) => {
  const { app, calls } = harness(t, ({ url }) => url.endsWith("/login/sms") ? reply({ id: "A", phone: "••• ••12" }) : undefined);
  await app.loadAccounts();
  app.loginAwaitingOtp.value = true;
  await app.requestLoginSms();
  const sms = calls.find((call) => call.url.endsWith("/login/sms"));
  assert.equal(sms.init.method, "POST");
  assert.equal(app.loginAwaitingOtp.value, true);
  assert.match(app.notice.value, /••• ••12/);
});

test("SMS request on an expired login returns to the password step", async (t) => {
  const { app } = harness(t, ({ url }) => url.endsWith("/login/sms") ? reply(null, 410, "登录会话已过期,请重新用密码登录") : undefined);
  await app.loadAccounts();
  app.loginAwaitingOtp.value = true;
  await app.requestLoginSms();
  assert.equal(app.loginAwaitingOtp.value, false);
  assert.match(app.error.value, /已过期/);
});

test("mail times are shown in UTC+8 regardless of the source offset", (t) => {
  const { app } = harness(t);
  assert.equal(app.formatMessageTime("2026-11-25T07:52:32Z"), "2026-11-25 15:52");
  assert.equal(app.formatMessageTime("2026-11-25T15:52:32+08:00"), "2026-11-25 15:52");
  assert.equal(app.formatMessageTime("2026-11-24T23:30:00-05:00"), "2026-11-25 12:30");
  assert.equal(app.formatMessageDateTime("2026-11-25T07:52:32Z"), "2026-11-25 15:52:32 (UTC+8)");
  assert.equal(app.formatMessageTime("2026-12-31T16:00:00Z"), "2027-01-01 00:00", "crosses the year boundary");
  assert.equal(app.formatMessageTime(""), "未知");
  assert.equal(app.formatMessageTime("not a date"), "not a date");
});
