#!/usr/bin/env node
/**
 * 前端健壮性检查 —— 覆盖历史上反复出现的 bug 类型
 *
 * 这些都是我实际犯过的错，加个脚本防止再犯：
 *
 *  ① Promise.all 后访问超出长度的索引
 *     实例：res[3].devices 但 Promise.all 只有 3 项
 *           -> TypeError: Cannot read properties of undefined
 *
 *  ② 引用了未定义的函数
 *     实例：modelLabel / dot2 / mask 在重写大段代码时丢掉
 *           -> 被 .catch() 吞掉 -> 页面空白但不报错
 *
 *  ③ 定义了但从未调用的关键函数
 *     实例：bindFind() 写好但没调 -> 输入框没反应
 *
 * 零依赖，退出码 0=通过 1=有问题。
 */
const fs = require('fs');
const path = require('path');

// 参数：第一个非 -- 开头的是文件路径
const ARGS = process.argv.slice(2);
const QUIET = ARGS.includes('--quiet');
const FILE = ARGS.filter(a => !a.startsWith('--'))[0]
  || path.join(__dirname, '..', 'platform/fnos/app/www/app.js');
const js = fs.readFileSync(FILE, 'utf8');

const problems = [];
const warns = [];

// ===== 收集所有顶层函数定义 =====
const defs = new Set();
{
  const re = /^function\s+([A-Za-z_$][\w$]*)\s*\(/gm;
  let m;
  while ((m = re.exec(js))) defs.add(m[1]);
}

// ===== ① Promise.all / 数组索引越界 =====
{
  const re = /Promise\.all\(\s*\[([\s\S]*?)\]\s*\)/g;
  let m;
  while ((m = re.exec(js))) {
    const body = m[1];
    // 数一下有几项（粗略：按顶层逗号分）
    let depth = 0, items = 1, hasAny = false;
    for (const ch of body) {
      if ('([{'.includes(ch)) depth++;
      else if (')]}'.includes(ch)) depth--;
      else if (ch === ',' && depth === 0) items++;
    }
    if (!body.trim()) { items = 0; } else { hasAny = true; }
    if (!hasAny) continue;

    // 找后续 2000 字符内用到的 res[N]
    const after = js.slice(m.index, m.index + 2000);
    const used = [...after.matchAll(/res\s*\[\s*(\d+)\s*\]/g)].map(x => parseInt(x[1], 10));
    const max = used.length ? Math.max(...used) : -1;
    if (max >= items) {
      const line = js.slice(0, m.index).split('\n').length;
      problems.push(`第 ${line} 行：Promise.all 只有 ${items} 项，但访问了 res[${max}]（越界）`);
    }
  }
}

// ===== ② 未定义的函数引用 =====
// 只检查形如 foo(...) 的显式调用，且名字看起来是项目自定义的
{
  const builtin = new Set([
    'if','for','while','switch','catch','function','return','typeof','new','delete','void',
    'parseInt','parseFloat','isNaN','isFinite','encodeURIComponent','decodeURIComponent',
    'encodeURI','decodeURI','String','Number','Boolean','Array','Object','Date','Math','JSON',
    'Promise','Map','Set','Error','RegExp','Symbol','setTimeout','clearTimeout','setInterval',
    'clearInterval','requestAnimationFrame','fetch','alert','confirm','prompt','structuredClone',
    'Blob','URL','FileReader','FormData','Image','Audio','Event','CustomEvent','IntersectionObserver',
  ]);
  const called = new Map();
  const re = /(?<![.\w$])([a-z][A-Za-z0-9_$]*)\s*\(/g;
  let m;
  while ((m = re.exec(js))) {
    const n = m[1];
    if (builtin.has(n) || defs.has(n)) continue;
    if (!called.has(n)) called.set(n, js.slice(0, m.index).split('\n').length);
  }
  // 过滤掉明显是对象方法/属性的（有点号前缀的已被排除）
  // 只报「看起来像项目函数」的：长度>3 且出现>=2 次
  const counts = new Map();
  for (const [n] of called) {
    const c = (js.match(new RegExp('(?<![.\\w$])' + n + '\\s*\\(', 'g')) || []).length;
    counts.set(n, c);
  }
  for (const [n, line] of called) {
    if (counts.get(n) >= 2 && n.length >= 4) {
      warns.push(`第 ${line} 行：调用了 ${n}() 但未找到定义（可能是外部/内置，请人工确认）`);
    }
  }
}

// ===== ③ 定义了但从未调用的关键函数 =====
{
  const critical = ['bindFind','bindDevTable','bindAccounts','checkCredBanner','refreshLiveMode',
                    'syncUserCardCount','loadCloudDevices','acctDevTable','modelLabel','dot2','maskUser'];
  for (const n of critical) {
    if (!defs.has(n)) continue;
    const uses = (js.match(new RegExp('(?<![.\\w$])' + n + '\\s*\\(', 'g')) || []).length;
    if (uses <= 1) {  // 只有定义那一处
      warns.push(`函数 ${n}() 定义了但从未被调用`);
    }
  }
}

// ===== 输出 =====
if (!QUIET) {
  console.log('前端健壮性检查');
  console.log('  文件: ' + path.relative(process.cwd(), FILE));
  console.log('  顶层函数: ' + defs.size + ' 个');
  console.log('');
}

if (problems.length) {
  console.log('❌ 问题（' + problems.length + '）:');
  problems.forEach(p => console.log('   ' + p));
} else {
  console.log('✅ 无越界访问');
}

if (warns.length && !QUIET) {
  console.log('');
  console.log('⚠️  提示（' + warns.length + '）（局部函数属正常，人工确认即可）:');
  warns.slice(0, 8).forEach(w => console.log('   ' + w));
  if (warns.length > 8) console.log('   ...还有 ' + (warns.length - 8) + ' 条');
}

process.exit(problems.length ? 1 : 0);
