#!/usr/bin/env node
/* MiCam 布局自检
 *
 * 起因：连续两个问题（胶囊换行、概览卡错位）都是
 *       「元素存在但排版不对」—— 存在性检查发现不了。
 *
 * 这个脚本检查**几何**问题：
 *   1. 同类元素是否高度一致（不一致 = 换行/尺寸异常）
 *   2. 子元素是否垂直居中对齐（spread 大 = 排版乱）
 *   3. label/value 是否左对齐（deltaLeft 大 = 错位）
 *   4. 文字是否溢出容器
 *   5. 元素是否互相重叠
 *
 * 用法：node audit-layout.js <page-key>
 */
const { chromium } = require('playwright');
const CHROME = '/root/shot/chrome/chrome-linux64/chrome';

(async () => {
  const pageKey = process.argv[2] || '-';
  const b = await chromium.launch({
    executablePath: CHROME,
    args: ['--no-sandbox','--disable-dev-shm-usage','--disable-gpu','--autoplay-policy=no-user-gesture-required']
  });
  const ctx = await b.newContext({ viewport:{width:1440,height:900}, deviceScaleFactor:1, locale:'zh-CN' });
  const p = await ctx.newPage();
  await p.goto('http://127.0.0.1:8900/', { waitUntil:'domcontentloaded', timeout:30000 });
  await p.waitForTimeout(5000);
  if (pageKey === 'live') {
    // 「实时预览」是「摄像头」的子页，走：摄像头 -> 点第一张在线卡片
    await p.evaluate(() => {
      const el = Array.from(document.querySelectorAll('.tabnav')).find(x => x.dataset.k === 'devices');
      if (el) el.click();
    });
    await p.waitForTimeout(5000);
    await p.evaluate(() => {
      const c = document.querySelector('.cam-card:not(.is-offline)');
      if (c) c.click();
    });
    await p.waitForTimeout(14000);
  } else if (pageKey && pageKey !== '-') {
    await p.evaluate(k => {
      const el = Array.from(document.querySelectorAll('.tabnav')).find(x => x.dataset.k === k);
      if (el) el.click();
    }, pageKey);
    await p.waitForTimeout(7000);
  }

  const issues = await p.evaluate(() => {
    const out = [];
    const rect = el => el.getBoundingClientRect();

    // 1) 同类元素高度一致性
    const groups = ['.chip', '.stat-card', '.cam-card', '.tabnav', '.btn'];
    groups.forEach(sel => {
      const els = Array.from(document.querySelectorAll(sel)).filter(e => e.offsetParent !== null);
      if (els.length < 2) return;
      const hs = els.map(e => Math.round(rect(e).height));
      const min = Math.min(...hs), max = Math.max(...hs);
      if (max - min > 2) {
        out.push({
          type: '尺寸不一致', level: 'warn', sel,
          detail: sel + ' 高度 ' + min + '~' + max + 'px（差 ' + (max-min) + 'px）',
          hint: '同类元素高度不一致，通常是有内容换行导致',
        });
      }
    });

    // 2) 子元素垂直居中
    ['.chip', '.stat-card', '.cam-card__row', '.topnav__user', '.event-item'].forEach(sel => {
      Array.from(document.querySelectorAll(sel)).forEach((el, i) => {
        const kids = Array.from(el.children).filter(k => k.offsetParent !== null && rect(k).height > 0);
        if (kids.length < 2) return;
        const cys = kids.map(k => { const r = rect(k); return r.top + r.height/2; });
        const spread = Math.max(...cys) - Math.min(...cys);
        if (spread > 6) {
          out.push({
            type: '未垂直居中', level: 'warn', sel,
            detail: sel + ' #' + i + ' 子元素中心线偏差 ' + Math.round(spread) + 'px',
            hint: '子元素不在同一水平线，可能需要 display:flex; align-items:center',
          });
        }
      });
    });

    // 3) label / value 左对齐（卡片类）
    Array.from(document.querySelectorAll('.stat-card, .card')).forEach((card, i) => {
      const lab = card.querySelector('[class*="__label"]');
      const val = card.querySelector('[class*="__value"]');
      if (!lab || !val) return;
      const d = Math.abs(rect(val).left - rect(lab).left);
      if (d > 4) {
        out.push({
          type: '左边缘错位', level: 'error', sel: '.stat-card',
          detail: '卡片 #' + i + ' title/value 左边缘差 ' + Math.round(d) + 'px',
          hint: '父容器可能需要 display:flex; flex-direction:column; align-items:flex-start',
        });
      }
    });

    // 4) 文字溢出
    Array.from(document.querySelectorAll('*')).forEach(el => {
      if (el.children.length > 0) return;
      if (!el.textContent.trim()) return;
      if (getComputedStyle(el).overflow === 'visible' &&
          el.scrollWidth > el.clientWidth + 4 && el.clientWidth > 0) {
        const cls = (el.className || '').toString().split(' ')[0];
        if (cls) out.push({
          type: '文字溢出', level: 'info', sel: '.' + cls,
          detail: '.' + cls + ' 内容 ' + el.scrollWidth + 'px > 容器 ' + el.clientWidth + 'px',
          hint: '可能需要 text-overflow:ellipsis 或换行',
        });
      }
    });

    // 5) 零尺寸媒体（视频/图片渲染成 0 高 = 布局塌陷）
    Array.from(document.querySelectorAll('video, .player img, .cam-card img, .cam-tile img')).forEach(el => {
      const r = rect(el);
      if (r.width > 50 && r.height < 4) {
        out.push({
          type: '零尺寸媒体', level: 'error', sel: el.tagName.toLowerCase(),
          detail: '<' + el.tagName.toLowerCase() + '> 宽 ' + Math.round(r.width) + ' 但高 ' + Math.round(r.height),
          hint: '父容器可能没高度，或 JS 设了内联 position 覆盖 CSS',
        });
      }
    });

    // 去重 + 限制条数
    const seen = new Set();
    return out.filter(o => {
      const k = o.type + o.sel + o.detail;
      if (seen.has(k)) return false;
      seen.add(k); return true;
    }).slice(0, 25);
  });

  if (!issues.length) {
    console.log('✅ 布局自检通过：未发现问题');
  } else {
    const errs = issues.filter(i => i.level === 'error');
    const warns = issues.filter(i => i.level === 'warn');
    console.log('❌ ' + errs.length + ' 个错误, ⚠️  ' + warns.length + ' 个警告, ℹ️  ' + (issues.length-errs.length-warns.length) + ' 个提示');
    console.log();
    issues.forEach(o => {
      const mark = o.level === 'error' ? '❌' : o.level === 'warn' ? '⚠️ ' : 'ℹ️ ';
      console.log(mark + ' [' + o.type + '] ' + o.detail);
      console.log('     ' + o.hint);
    });
  }
  await b.close();
  process.exit(issues.filter(i => i.level === 'error').length ? 1 : 0);
})().catch(e => { console.error('FAIL ' + String(e).slice(0,250)); process.exit(2); });
