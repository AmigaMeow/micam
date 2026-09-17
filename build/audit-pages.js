const { chromium } = require('playwright');
const CHROME = '/root/shot/chrome/chrome-linux64/chrome';

// 把内层滚动容器展开，让 fullPage 能截全
async function expand(p){
  await p.evaluate(() => {
    const c = document.getElementById('content');
    if (c) { c.style.overflow = 'visible'; c.style.height = 'auto'; c.style.maxHeight = 'none'; }
    document.body.style.overflow = 'visible';
    document.body.style.height = 'auto';
    document.documentElement.style.overflow = 'visible';
    document.documentElement.style.height = 'auto';
  });
}

async function audit(p, name){
  return await p.evaluate((nm) => {
    const issues = [];
    const rc = el => el.getBoundingClientRect();
    document.querySelectorAll('#content *').forEach(el => {
      const r = rc(el);
      if (r.width === 0 || r.height === 0) return;
      if (r.right > window.innerWidth + 2) issues.push('OVERFLOW_X ' + (el.className||el.tagName).toString().slice(0,44) + ' right=' + Math.round(r.right));
      if (r.left < -2) issues.push('OVERFLOW_L ' + (el.className||el.tagName).toString().slice(0,44));
    });
    document.querySelectorAll('img, video, canvas, .ai-ring').forEach(el => {
      const r = rc(el);
      if (r.width > 40 && r.height < 8) issues.push('ZERO_H <' + el.tagName + ' class="' + (el.className||'').toString().slice(0,30) + '">');
    });
    document.querySelectorAll('img').forEach(i => {
      if (i.complete && i.naturalWidth === 0 && i.src && !i.src.startsWith('data:')) issues.push('IMG_FAIL ' + i.src.split('?')[0].slice(-30));
    });
    document.querySelectorAll('#content .card').forEach((c,i) => { if ((c.innerText||'').trim().length < 4) issues.push('EMPTY_CARD #' + i); });
    return {
      page: nm,
      contentH: (document.getElementById('content')||{}).scrollHeight || 0,
      issues: [...new Set(issues)].slice(0, 10),
      n: [...new Set(issues)].length,
      textLen: (document.getElementById('content')||{}).innerText?.length || 0,
    };
  }, name);
}

(async () => {
  const b = await chromium.launch({ executablePath: CHROME,
    args: ['--no-sandbox','--disable-dev-shm-usage','--disable-gpu','--font-render-hinting=none','--hide-scrollbars'] });
  const ctx = await b.newContext({ viewport:{width:1440,height:1000}, deviceScaleFactor:1, locale:'zh-CN' });
  const p = await ctx.newPage();
  const errs = [];
  p.on('pageerror', e => errs.push(String(e).slice(0,110)));
  await p.goto('http://127.0.0.1:8900/?a=' + Date.now(), { waitUntil:'domcontentloaded', timeout:30000 });
  await p.waitForTimeout(6000);

  const out = [];
  const goto = async (k, wait) => {
    await p.evaluate((kk) => { const el = Array.from(document.querySelectorAll('.tabnav')).find(x => x.dataset.k === kk); if (el) el.click(); }, k);
    await p.waitForTimeout(wait || 5500);
  };

  for (const [k, nm] of [['home','首页'],['devices','摄像头'],['events','事件'],['recordings','回放'],['storage','存储'],['settings','设置'],['accounts','账号']]) {
    await goto(k);
    await expand(p);
    await p.waitForTimeout(600);
    out.push(await audit(p, nm));
    await p.screenshot({ path: '/tmp/shots/au2-' + k + '.png', fullPage: true });
    await p.reload({ waitUntil:'domcontentloaded' }); await p.waitForTimeout(5000);
  }

  // AI：先进 AI 页，再点 4 个 Tab
  await goto('ai', 9000);
  for (const [t, nm] of [['overview','AI/概览'],['search','AI/搜索'],['investigate','AI/调查'],['settings','AI/设置']]) {
    await p.evaluate((tt) => { const el = document.querySelector('[data-ait="' + tt + '"]'); if (el) el.click(); }, t);
    await p.waitForTimeout(t === 'overview' ? 6000 : 4500);
    await expand(p);
    await p.waitForTimeout(600);
    out.push(await audit(p, nm));
    await p.screenshot({ path: '/tmp/shots/au2-ai-' + t + '.png', fullPage: true });
    await p.evaluate((tt) => { const el = document.querySelector('[data-ait="' + tt + '"]'); if (el) el.click(); }, t);
  }

  console.log(JSON.stringify(out, null, 1));
  console.log('ERRS ' + (errs.length ? JSON.stringify([...new Set(errs)].slice(0,5)) : '(无)'));
  await b.close();
})().catch(e => { console.error('FAIL ' + String(e).slice(0,250)); process.exit(1); });
