// Real-browser configuration import acceptance (spec 12.4).
// Run only in a disposable initialized test environment; it creates drafts/users and switches test licenses.
// IMPORT_E2E_LIVE=1 requires two online demo nodes and checks real business responses after publishing.
const { test } = require('node:test')
const assert = require('node:assert/strict')
const { chromium, request } = require('playwright')
const { execFileSync } = require('node:child_process')
const fs = require('node:fs/promises')
const path = require('node:path')

const BASE = process.env.BASE || 'http://127.0.0.1:8443'
const ADMIN_PW = process.env.ADMIN_PW || 'Demo@2026!'
const LIVE = process.env.IMPORT_E2E_LIVE === '1'
const GROUP_ID = Number(process.env.IMPORT_GROUP_ID || 1)
const OUTPUT = path.resolve(process.env.IMPORT_REPORT_DIR || 'dist/test-reports/import-ui')
const RUN = `im${Date.now().toString(36)}`
const DOMAIN = `${RUN}.example.com`
const UPSTREAM = process.env.IMPORT_UPSTREAM || '172.30.0.31:9001'
const SNIPPET = /client_max_body_size\s+8m;/
const DUMP = `# configuration file /etc/web/web.conf:
http {
 upstream ${RUN} { server ${UPSTREAM}; }
 server {
  listen 80;
  server_name ${DOMAIN};
  client_max_body_size 8m;
  location / { proxy_pass http://${RUN}; }
 }
 server {
  listen 80;
  server_name blocked-${DOMAIN};
  location / { deny all; }
  location ~ \\.php$ { fastcgi_pass 127.0.0.1:9000; }
 }
}
`
const bname = (s) => new RegExp(`^\\s*${[...s].map((c) => c.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')).join('\\s*')}\\s*$`)
const button = (p, name) => p.getByRole('button', { name: bname(name) })
const account = (r) => `${RUN}-${r}`
const password = (r) => `Harness@2026!${r}-${RUN}`
const apiPath = (p) => `/api/v1${p}`
async function json(response, status = 200) {
  // Do not include response bodies: they can contain a session or one-time token.
  assert.equal(response.status(), status, new URL(response.url()).pathname)
  return response.json()
}
async function apiLogin(username, pw) {
  const c = await request.newContext({ baseURL: BASE, extraHTTPHeaders: { 'X-Requested-With': 'etos' } })
  await json(await c.post(apiPath('/auth/login'), { data: { username, password: pw } }))
  return c
}
function issue(installation, expired) {
  const { LIC_TOOL, LIC_KEY, LIC_KID } = process.env
  assert.ok(LIC_TOOL && LIC_KEY && LIC_KID, 'test signer LIC_TOOL/LIC_KEY/LIC_KID is required; license checks must not silently skip')
  const day = (n) => new Date(Date.now() + n * 86400000).toISOString().slice(0, 10)
  const extra = expired ? ['-expires', day(-20), '-grace', '15', '-maintenance', day(365)] : ['-days', '365']
  return execFileSync(LIC_TOOL, ['issue', '-key', LIC_KEY, '-kid', LIC_KID, '-installation-id', installation,
    '-binding', 'installation_only', '-edition', 'professional', '-customer', 'import-acceptance', '-nodes', '10', '-sites', '200', ...extra], { encoding: 'utf8' })
}
async function eventually(fn, message, timeout = 60000) {
  const end = Date.now() + timeout
  while (Date.now() < end) {
    if (await fn()) return
    await new Promise((resolve) => setTimeout(resolve, 250))
  }
  assert.fail(message)
}

test('配置导入：浏览器、API、审批与业务响应', { timeout: 240000 }, async (t) => {
  await fs.mkdir(OUTPUT, { recursive: true })
  const clients = []
  const contexts = []
  const admin = await apiLogin('admin', ADMIN_PW)
  clients.push(admin)
  const license = await json(await admin.get(apiPath('/license')))
  assert.equal(license.state, 'VALID', 'requires a valid test license')
  const restoreLicense = issue(license.installation_id, false)
  // Validate the signer/installation pairing before any tests mutate the environment.
  assert.equal((await json(await admin.post(apiPath('/license/import'), { data: { raw: restoreLicense } }))).state, 'VALID')
  const approval = await json(await admin.get(apiPath('/release-approval')))
  assert.equal(approval.enabled, false, 'run after demo e2e with release approval disabled')
  const roles = {}
  let browser
  let page
  let uploaded
  let applied
  let baseRevision
  let selectedGroup
  let sourceSites
  let pendingImport
  const resources = []
  try {
    browser = await chromium.launch()
    for (const role of ['operator', 'viewer', 'releaser']) {
      const initial = `${password(role)}-initial`
      await json(await admin.post(apiPath('/users'), { data: { username: account(role), password: initial, role } }))
      const c = await apiLogin(account(role), initial)
      clients.push(c)
      await json(await c.put(apiPath('/auth/password'), { data: { old_password: initial, new_password: password(role) } }))
      roles[role] = c
    }
    const groups = await json(await admin.get(apiPath('/node-groups')))
    selectedGroup = groups.find((g) => g.id === GROUP_ID)
    assert.ok(selectedGroup, 'target node group exists')
    if (LIVE) assert.ok(selectedGroup.online >= 2, 'live mode requires at least two online nodes; zero-node publish is not business verification')
    baseRevision = selectedGroup.current_revision_id
    sourceSites = await json(await admin.get(apiPath(`/sites?group_id=${GROUP_ID}`)))
    const ctx = await browser.newContext({ locale: 'zh-CN', baseURL: BASE, acceptDownloads: true })
    contexts.push(ctx)
    page = await ctx.newPage()
    page.on('response', (r) => {
      if (new URL(r.url()).origin === new URL(BASE).origin && /\.(?:js|css)(?:\?|$)/.test(r.url()) && r.status() >= 400) resources.push(new URL(r.url()).pathname)
    })
    await t.test('UI-02 / IMP-08 登录与刷新配置导入入口', async () => {
      await page.goto('/imports')
      await page.waitForURL((url) => url.pathname === "/login")
      await page.locator('input[autocomplete="username"]').fill(account('operator'))
      await page.locator('input[autocomplete="current-password"]').fill(password('operator'))
      await button(page, '登录').click()
      await page.waitForURL((url) => url.pathname === "/imports")
      await page.goto('/imports')
      await page.reload()
      await page.getByRole('heading', { name: '配置导入', exact: true }).waitFor()
      assert.ok(await button(page, '上传配置').isEnabled())
      if (groups.length > 1) await page.getByText(selectedGroup.name, { exact: true }).first().click()
    })
    await t.test('IMP-01/04 上传文本并展示逐行转换报告', async () => {
      const chooser = page.waitForEvent('filechooser')
      await button(page, '上传配置').click()
      const response = page.waitForResponse((r) => r.request().method() === 'POST' && new URL(r.url()).pathname === apiPath('/imports'))
      await (await chooser).setFiles({ name: `${RUN}.conf`, mimeType: 'text/plain', buffer: Buffer.from(DUMP) })
      uploaded = await json(await response)
      assert.equal(uploaded.import.group_id, GROUP_ID)
      assert.equal(uploaded.report.sites.length, 2)
      const unsupported = uploaded.report.sites.flatMap((s) => s.issues).find((i) => i.directive === 'fastcgi_pass')
      assert.ok(unsupported && unsupported.file === '/etc/web/web.conf' && unsupported.line > 0 && unsupported.blocking)
      await page.getByRole('heading', { name: new RegExp(`导入报告 #${uploaded.import.id} `) }).waitFor()
      await page.getByText('fastcgi_pass', { exact: true }).waitFor()
      const blockingOnly = page.getByRole('checkbox', { name: '只看需人工处理的站点问题' })
      await blockingOnly.check()
      await page.getByText('fastcgi_pass', { exact: true }).waitFor()
      await blockingOnly.uncheck()
      assert.ok(await button(page, '应用为草稿').isEnabled())
    })
    await t.test('IMP-04 下载报告内容与上传报告一致且无私钥', async () => {
      assert.ok(uploaded)
      const download = page.waitForEvent('download')
      await button(page, '下载报告').click()
      const d = await download
      assert.equal(d.suggestedFilename(), `import-report-${uploaded.import.id}.json`)
      const stream = await d.createReadStream()
      const chunks = []
      for await (const c of stream) chunks.push(c)
      const raw = Buffer.concat(chunks).toString()
      assert.ok(!raw.includes('PRIVATE KEY'))
      assert.deepEqual(JSON.parse(raw), uploaded.report)
    })
    await t.test('IMP-05 上传不创建草稿或改变线上版本', async () => {
      assert.ok(uploaded)
      assert.deepEqual(await json(await admin.get(apiPath(`/sites?group_id=${GROUP_ID}`))), sourceSites)
      const g = (await json(await admin.get(apiPath('/node-groups')))).find((g) => g.id === GROUP_ID)
      assert.equal(g.current_revision_id, baseRevision)
    })
    await t.test('IMP-05/06 应用生成草稿，未转换站点停用，片段待审批', async () => {
      assert.ok(uploaded)
      const response = page.waitForResponse((r) => r.request().method() === 'POST' && new URL(r.url()).pathname === apiPath(`/imports/${uploaded.import.id}/apply`))
      await button(page, '应用为草稿').click()
      applied = await json(await response)
      assert.deepEqual(applied.errors, [])
      assert.equal(applied.sites.length, 2)
      assert.equal(applied.sites.filter((s) => s.enabled).length, 1)
      assert.equal(applied.snippets.length, 1)
      await page.getByRole('heading', { name: '应用结果', exact: true }).waitFor()
      await eventually(async () => (await button(page, '应用为草稿').count()) === 0, 'apply action should disappear after confirmation')
      const g = (await json(await admin.get(apiPath('/node-groups')))).find((g) => g.id === GROUP_ID)
      assert.equal(g.current_revision_id, baseRevision)
      const preview = await json(await admin.get(apiPath(`/node-groups/${GROUP_ID}/preview`)))
      assert.ok(!SNIPPET.test(preview.files.map((f) => f.diff).join('\n')))
    })
    await t.test('IMP-05 重复应用返回冲突且不生成重复站点', async () => {
      assert.ok(uploaded && applied)
      const before = await json(await admin.get(apiPath(`/sites?group_id=${GROUP_ID}`)))
      const e = await json(await roles.operator.post(apiPath(`/imports/${uploaded.import.id}/apply`), { data: {} }), 409)
      assert.equal(e.code, 'ALREADY_APPLIED')
      assert.deepEqual(await json(await admin.get(apiPath(`/sites?group_id=${GROUP_ID}`))), before)
    })
    await t.test('无路由的导入草稿可以在站点编辑页添加并保存路由', async () => {
      const name = `empty-${RUN}`
      const emptyDump = `# configuration file /etc/web/web.conf:\nhttp { server { listen 80; server_name ${name}.example.com; client_max_body_size 8m; root /outside; } }\n`
      const imp = await json(await roles.operator.post(apiPath('/imports'), { multipart: { group_id: String(GROUP_ID), file: { name: 'empty.conf', mimeType: 'text/plain', buffer: Buffer.from(emptyDump) } } }))
      const result = await json(await roles.operator.post(apiPath(`/imports/${imp.import.id}/apply`), { data: {} }))
      assert.equal(result.sites.length, 1)
      assert.equal(result.sites[0].enabled, false)
      const before = (await json(await roles.operator.get(apiPath(`/sites?group_id=${GROUP_ID}`)))).find((s) => s.name === name)
      assert.equal(before.spec.routes, null)
      await page.goto('/sites')
      const row = page.getByRole('row').filter({ hasText: `${name}.example.com` })
      await button(row, '编辑').click()
      await button(page, '添加路由').click()
      const input = page.locator('.route input.code').first()
      await input.waitFor({ timeout: 5000 })
      assert.equal(await input.inputValue(), '/new/')
      await input.fill('/health')
      const updated = page.waitForResponse((r) => r.request().method() === 'PUT' && new URL(r.url()).pathname === apiPath(`/sites/${before.id}`))
      await button(page, '保存').click()
      const saved = await json(await updated)
      assert.equal(saved.spec.routes[0].path, '/health')
      assert.equal(saved.enabled, false)
      await page.goto('/imports')
      await page.getByRole('heading', { name: '配置导入', exact: true }).waitFor()
    })
    await t.test('可一次选择主配置及被引用的文件', async () => {
      const chooser = page.waitForEvent('filechooser')
      await button(page, '上传配置').click()
      const response = page.waitForResponse((r) => r.request().method() === 'POST' && new URL(r.url()).pathname === apiPath('/imports'))
      await (await chooser).setFiles([
        { name: 'web.conf', mimeType: 'text/plain', buffer: Buffer.from(`http { server { listen 80; server_name multi-${DOMAIN}; include /old/conf/routes.conf; } }`) },
        { name: 'routes.conf', mimeType: 'text/plain', buffer: Buffer.from('location /multi { deny all; }') },
      ])
      const result = await json(await response)
      assert.equal(result.import.source, 'files')
      assert.equal(result.report.sites.length, 1)
      assert.equal(result.report.sites[0].spec.routes[0].path, '/multi')
      await page.getByRole('heading', { name: new RegExp(`导入报告 #${result.import.id} `) }).waitFor()
    })
    await t.test('AUTH-01 / IMP-08 viewer/releaser 无上传按钮且接口拒绝读写导入', async () => {
      assert.ok(uploaded)
      for (const role of ['viewer', 'releaser']) {
        const c = await browser.newContext({ baseURL: BASE, storageState: await roles[role].storageState() })
        contexts.push(c)
        const p = await c.newPage()
        await p.goto('/imports')
        await p.getByRole('heading', { name: '配置导入', exact: true }).waitFor()
        assert.equal(await button(p, '上传配置').count(), 0)
        for (const suffix of ['', `/${uploaded.import.id}`, `/${uploaded.import.id}/report`]) {
          assert.equal((await roles[role].get(apiPath(`/imports${suffix}`))).status(), 403)
        }
        assert.equal((await roles[role].post(apiPath('/imports'), { multipart: { group_id: String(GROUP_ID), file: { name: 'denied.conf', mimeType: 'text/plain', buffer: Buffer.from(DUMP) } } })).status(), 403)
        assert.equal((await roles[role].post(apiPath(`/imports/${uploaded.import.id}/apply`), { data: {} })).status(), 403)
        await c.close()
      }
    })
    await t.test('SNP-02 / IMP-06 自批被拒，另一人批准后进入预览', async () => {
      assert.ok(applied)
      const id = applied.snippets[0]
      const e = await json(await roles.operator.post(apiPath(`/snippets/${id}/approve`), { data: {} }), 403)
      assert.equal(e.code, 'SELF_APPROVAL')
      await json(await admin.post(apiPath(`/snippets/${id}/approve`), { data: {} }))
      const preview = await json(await admin.get(apiPath(`/node-groups/${GROUP_ID}/preview`)))
      assert.ok(SNIPPET.test(preview.files.map((f) => f.diff).join('\n')))
    })
    await t.test('IMP-08 切换节点组，导入记录按组过滤', async () => {
      assert.ok(uploaded)
      const other = await json(await admin.post(apiPath('/node-groups'), { data: { name: `${RUN}-other` } }))
      await page.reload()
      await page.getByText(other.name, { exact: true }).first().click()
      await eventually(async () => !(await page.getByRole('row', { name: new RegExp(`${RUN}\\.conf`) }).count()), 'history from previous group must disappear')
      await page.getByText(selectedGroup.name, { exact: true }).first().click()
      await page.getByRole('row', { name: new RegExp(`${RUN}\\.conf`) }).waitFor()
      assert.ok((await json(await admin.get(apiPath(`/imports?group_id=${other.id}`)))).every((i) => i.group_id === other.id))
    })
    await t.test('LIC-07 / IMP-08 过期时禁止上传/应用，报告可读，恢复后可写', async () => {
      assert.ok(uploaded)
      pendingImport = await json(await roles.operator.post(apiPath('/imports'), { multipart: { group_id: String(GROUP_ID), file: { name: 'pending.conf', mimeType: 'text/plain', buffer: Buffer.from(DUMP) } } }))
      try {
        const expired = issue(license.installation_id, true)
        assert.equal((await json(await admin.post(apiPath('/license/import'), { data: { raw: expired } }))).state, 'EXPIRED')
        await page.reload()
        await page.getByRole('heading', { name: '配置导入', exact: true }).waitFor()
        await eventually(async () => button(page, '上传配置').isDisabled(), 'upload must be disabled while expired')
        const e = await json(await roles.operator.post(apiPath(`/imports/${pendingImport.import.id}/apply`), { data: {} }), 423)
        assert.equal(e.code, 'LICENSE_READ_ONLY')
        assert.equal((await roles.operator.post(apiPath('/imports'), { multipart: { group_id: String(GROUP_ID), file: { name: 'expired.conf', mimeType: 'text/plain', buffer: Buffer.from(DUMP) } } })).status(), 423)
        await json(await roles.operator.get(apiPath(`/imports/${pendingImport.import.id}/report`)))
      } finally {
        assert.equal((await json(await admin.post(apiPath('/license/import'), { data: { raw: restoreLicense } }))).state, 'VALID')
      }
      await page.reload()
      await eventually(async () => button(page, '上传配置').isEnabled(), 'upload must recover after valid license')
    })
    await t.test('MVP-10 / IMP-05 导入草稿发布到真实双节点并验证业务', { skip: LIVE ? false : 'control-only run: live dataplane verification not executed' }, async () => {
      assert.ok(applied)
      const dep = await json(await roles.releaser.post(apiPath(`/node-groups/${GROUP_ID}/publish`), { data: { note: `import acceptance ${RUN}` } }))
      let result
      await eventually(async () => {
        result = await json(await admin.get(apiPath(`/deployments/${dep.id}`)))
        return result.status !== 'APPLYING'
      }, 'deployment did not finish', 120000)
      assert.equal(result.status, 'APPLIED')
      assert.ok(result.nodes.length >= 2)
      assert.ok(result.nodes.every((n) => n.status === 'APPLIED'))
      const urls = (process.env.IMPORT_NODE_URLS || 'http://127.0.0.1:8001,http://127.0.0.1:8002').split(',')
      assert.ok(urls.length >= 2)
      for (const url of urls) {
        const response = await admin.get(`${url}/api/orders`, { headers: { Host: DOMAIN } })
        const business = await json(response)
        assert.equal(business.service, 'order-api')
        assert.equal(business.path, '/api/orders')
        await assert.rejects(admin.get(`${url}/api/orders`, { headers: { Host: `blocked-${DOMAIN}` } }), /socket hang up|ECONNRESET/, 'disabled imported host must hit default 444')
      }
    })
    await t.test('UI-01/02 页面资源无 404，导入记录刷新后仍能打开', async () => {
      assert.ok(uploaded)
      await page.reload()
      if (groups.length > 1) await page.getByText(selectedGroup.name, { exact: true }).first().click()
      const row = page.getByRole('row', { name: new RegExp(`${RUN}\\.conf`) })
      await row.getByText('查看', { exact: true }).click()
      await page.getByRole('heading', { name: '应用结果', exact: true }).waitFor()
      assert.deepEqual(resources, [])
    })
  } finally {
    // Always restore the valid test license even if a browser assertion fails.
    try { await json(await admin.post(apiPath('/license/import'), { data: { raw: restoreLicense } })) } finally {
      for (const c of contexts) await c.close().catch(() => {})
      if (browser) await browser.close()
      for (const c of clients) await c.dispose()
    }
  }
})
