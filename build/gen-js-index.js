#!/usr/bin/env node
/* ===== 生成前端 JS 的分节索引注释 =====
 *
 * 背景：/tmp/final-js.txt 约 6800 行，单文件、无构建步骤（刻意选择）。
 * 文件内用 `// ===== 页面：XXX =====` 分节，但没有总索引，找代码只能靠搜。
 *
 * 这个脚本在文件开头插入（或刷新）一段索引注释。
 * **只加注释，不改任何代码逻辑。**
 *
 * 用法：
 *   node build/gen-js-index.js           # 写入（幂等）
 *   node build/gen-js-index.js --check   # 只检查索引是否过期，过期退出码 1
 */

const fs = require('fs');

const JS = process.env.MICAM_JS || '/tmp/final-js.txt';

const BEGIN = '// ===== MiCam 前端 — 分节索引 =====';
const END = '// ===== 分节索引结束 =====';

// 分节注释：// ===== 页面：事件中心（增强版）=====  /  // ============ Tab 3：调查 ============
const MARKER = /^\/\/ =+ ?(.*?) ?=+\s*$/;

function stripIndex(text) {
  const lines = text.split('\n');
  const b = lines.findIndex(l => l.startsWith(BEGIN));
  if (b < 0) return lines;
  const e = lines.findIndex((l, i) => i > b && l.trim() === END);
  const cut = e < 0 ? b + 1 : e + 1;
  // 顺带吃掉索引块后面的空行，避免重复生成时越积越多
  let rest = lines.slice(cut);
  while (rest.length && rest[0].trim() === '') rest.shift();
  return rest;
}

// 中文字符按 2 列宽算，好让索引对齐
function dispWidth(s) {
  let w = 0;
  for (const ch of s) w += /[\u1100-\u115f\u2e80-\ua4cf\uac00-\ud7a3\uf900-\ufaff\ufe30-\ufe6f\uff00-\uff60\uffe0-\uffe6]/.test(ch) ? 2 : 1;
  return w;
}
function padTo(s, w) {
  const d = w - dispWidth(s);
  return d > 0 ? s + ' '.repeat(d) : s;
}

function build(bodyLines) {
  const marks = [];
  bodyLines.forEach((l, i) => {
    const m = l.match(MARKER);
    // 索引用的是 { line, text }，索引块自身不在 bodyLines 里
    if (m) marks.push({ text: l, name: m[1].trim(), line: i + 1 });
  });
  // 行数固定，与数字位数无关，所以可以先算长度再定行号
  const head = [
    BEGIN,
    '// （由 build/gen-js-index.js 自动生成，勿手改）',
    '//',
    '// 说明：下面的行号会随改动漂移，找代码时**直接搜注释本身**更可靠，例如：',
    '//   搜 `// ===== 页面：事件中心` 或 `// ============ Tab 3：调查`',
    '//',
  ];
  const tail = [
    '//',
    '// 重新生成索引：node build/gen-js-index.js',
    END,
  ];
  const blockLen = head.length + marks.length + tail.length;
  const maxW = marks.reduce((a, m) => Math.max(a, dispWidth(m.name)), 0);
  const body = [];
  for (let i = 0; i < marks.length; i++) {
    const start = marks[i].line + blockLen;
    const end = (i + 1 < marks.length ? marks[i + 1].line + blockLen - 1 : bodyLines.length + blockLen);
    body.push('//   ' + padTo(marks[i].name, maxW) + '   L ' + String(start).padStart(5) + ' - ' + String(end).padStart(5));
  }
  return { head, body, tail, count: marks.length };
}

const raw = fs.readFileSync(JS, 'utf8');
const bodyLines = stripIndex(raw);
const { head, body, tail, count } = build(bodyLines);
const out = head.concat(body, tail).join('\n') + '\n' + bodyLines.join('\n');

if (process.argv.includes('--check')) {
  if (out !== raw) {
    console.error('❌ 分节索引已过期，请运行: node build/gen-js-index.js');
    process.exit(1);
  }
  console.log('✅ 分节索引是最新的（' + count + ' 节）');
  process.exit(0);
}

if (out === raw) {
  console.log('  索引已是最新，无需改动（' + count + " 节）");
} else {
  fs.writeFileSync(JS, out);
  console.log('  ✅ 已写入分节索引：' + count + ' 节，索引块 ' + (head.length + body.length + tail.length) + ' 行');
  console.log('  文件总行数：' + out.split('\n').length);
}
