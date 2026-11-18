// The approval page is served from this file so the deployment's only human
// screen is reviewable in one piece.
package api

// approvalPage is the whole screen. It is deliberately plain: one code, what
// asked for it, and two buttons. No JavaScript, because a page that can grant
// an account should still work when a script does not, and no external asset,
// because a self-hosted service that fetches a font from someone else's CDN
// leaks more than it needs to.
const approvalPage = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex">
<title>Reasonix 自托管 · 设备登录</title>
<style>
  :root { color-scheme: light dark; }
  body {
    margin: 0; min-height: 100vh; display: flex; align-items: center;
    justify-content: center; padding: 24px; box-sizing: border-box;
    font: 15px/1.55 ui-sans-serif, system-ui, "Segoe UI", "Noto Sans SC", sans-serif;
    background: #0f1115; color: #e8eaed;
  }
  main {
    width: 100%; max-width: 420px; background: #171a20;
    border: 1px solid #262b33; border-radius: 16px; padding: 26px 24px 24px;
  }
  h1 { margin: 0 0 4px; font-size: 17px; font-weight: 600; }
  p.lede { margin: 0 0 20px; color: #9aa3ad; font-size: 13px; }
  .code {
    margin: 0 0 6px; font: 600 30px/1.2 ui-monospace, SFMono-Regular, Menlo, monospace;
    letter-spacing: 4px; text-align: center; padding: 14px 8px;
    background: #10131a; border: 1px solid #262b33; border-radius: 10px;
  }
  dl { margin: 18px 0 0; font-size: 13px; }
  dl div { display: flex; gap: 12px; padding: 5px 0; }
  dt { color: #8892a0; min-width: 72px; margin: 0; }
  dd { margin: 0; color: #c9d1d9; word-break: break-word; }
  label { display: block; margin: 20px 0 6px; font-size: 13px; color: #9aa3ad; }
  input[type=password], input[type=text] {
    width: 100%; box-sizing: border-box; padding: 11px 12px; font: inherit;
    color: #e8eaed; background: #10131a; border: 1px solid #262b33; border-radius: 9px;
  }
  .row { display: flex; gap: 10px; margin-top: 18px; }
  button {
    flex: 1; padding: 11px 16px; font: 600 14px/1 inherit; cursor: pointer;
    border-radius: 9px; border: 1px solid #2f3742; background: #1d2129; color: #e8eaed;
  }
  button.primary { background: #2f6feb; border-color: #2f6feb; color: #fff; }
  button:disabled { opacity: .5; cursor: not-allowed; }
  .note { margin: 16px 0 0; font-size: 12px; color: #8892a0; }
  .warn { margin: 16px 0 0; padding: 10px 12px; border-radius: 9px; font-size: 13px;
          background: #2a1d1d; border: 1px solid #5a2b2b; color: #f0b4b4; }
  .ok { margin: 16px 0 0; padding: 10px 12px; border-radius: 9px; font-size: 13px;
        background: #16261c; border: 1px solid #2b5a37; color: #a6e3b8; }
</style>
</head>
<body>
<main>
  <h1>设备登录</h1>
  <p class="lede">确认这台设备是否可以用你的账号连上自托管服务。</p>

  {{if .Error}}<div class="warn">{{.Error}}</div>{{end}}
  {{if .Done}}<div class="ok">已批准。回到那台设备，连接会在几秒内完成。</div>{{end}}
  {{if .Denied}}<div class="ok">已拒绝。那台设备的登录请求已经结束。</div>{{end}}

  {{if .Code}}
  <div class="code">{{.Code}}</div>
  <dl>
    <div><dt>账号</dt><dd>{{.Owner}}</dd></div>
    {{if .Client}}<div><dt>请求方</dt><dd>{{.Client}}</dd></div>{{end}}
    {{if .Expires}}<div><dt>过期</dt><dd>{{.Expires}}</dd></div>{{end}}
  </dl>

  <form method="post" action="/device/approve">
    <input type="hidden" name="userCode" value="{{.RawCode}}">
    <label for="token">管理令牌</label>
    <input id="token" name="token" type="password" autocomplete="current-password"
           required placeholder="REASONIX_SELFHOST_ADMIN_TOKEN">
    <div class="row">
      <button type="submit" class="primary">批准这台设备</button>
      <button type="submit" formaction="/device/deny">拒绝</button>
    </div>
  </form>
  <p class="note">令牌只用于这次批准，不会写进浏览器存储，也不会出现在地址栏里。</p>
  {{else}}
  <form method="get" action="/device">
    <label for="code">输入设备上显示的代码</label>
    <input id="code" name="code" type="text" inputmode="latin" autocomplete="off"
           autocapitalize="characters" spellcheck="false" placeholder="ABCD-EFGH" required>
    <div class="row"><button type="submit" class="primary">继续</button></div>
  </form>
  {{end}}
</main>
</body>
</html>
`
