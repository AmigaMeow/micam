#!/usr/bin/env node
/* 组装 index.html
 *
 * ===== 为什么 CSS 要内联 =====
 *
 * 飞牛网关把 /app/micam/ 转发给插件时会**剥掉前缀**，
 * 导致页面里的相对路径（assets/css/x.css）解析错乱——
 * 浏览器会去请求 /assets/css/x.css 而不是 /app/micam/assets/css/x.css。
 *
 * 实测日志证据：
 *   GET /app/micam/     <- 页面
 *   GET /hls.min.js     <- 相对路径被解析到根，而不是 /app/micam/hls.min.js
 *
 * 结果：CSS 全部 404 -> 插件变成无样式的裸 HTML。
 *
 * 所以 CSS 一律内联（只有 42KB）。图标 sprite 和 JS 本来就已经内联。
 * 这样 index.html 在任何环境（网关 / 直连 / iframe）表现一致。
 */
const fs = require('fs');
const path = require('path');

const ROOT = '/root/Deepseek/micam/platform/fnos/app';
const W = ROOT + '/www';
const SKEL = ROOT + '/shell.html';
const JS = '/tmp/final-js.txt';

const CSS_FILES = [
  'assets/ui/basecoat.css',
  'assets/css/tokens.css',
  'assets/css/base.css',
  'assets/css/components.css',
  'assets/css/pages.css',
];

const shell = fs.readFileSync(SKEL, 'utf8');
const sprite = fs.readFileSync(path.join(W, 'assets/img/icons.svg'), 'utf8');
const js = fs.readFileSync(JS, 'utf8');

// 内联 CSS：按顺序拼接，标注来源便于调试
let css = '';
for (const f of CSS_FILES) {
  const raw = fs.readFileSync(path.join(W, f), 'utf8');
  css += '\n/* ===== ' + f + ' ===== */\n' + raw + '\n';
}

let out = shell
  .replace('<!--SPRITE-->', sprite)
  .replace('<!--APP_JS-->', js)
  .replace('<!--INLINE_CSS-->', css);

// 注入构建时间戳，便于确认浏览器加载的是哪一版
const stamp = new Date().toISOString().replace('T',' ').slice(0,19);
const withStamp = out.replace('<title>小米摄像头</title>',
  '<title>小米摄像头</title>\n<meta name="micam-build" content="' + stamp + '">');
fs.writeFileSync(path.join(W, 'index.html'), withStamp);

// 校验
const errors = [];
if (out.indexOf('<!DOCTYPE') !== 0) errors.push('DOCTYPE 不在开头');
if ((out.match(/<script>/g) || []).length !== 1) errors.push('script 标签数量异常');
if (!out.includes('id="i-camera"')) errors.push('图标 sprite 缺失');
if (!out.includes('--bg:')) errors.push('CSS 未内联');
if (out.includes('<link rel="stylesheet"')) errors.push('仍有外部 CSS 引用');

console.log('  index.html: ' + (out.length / 1024).toFixed(0) + ' KB, ' + out.split('\n').length + ' 行');
console.log('  内联 CSS:   ' + (css.length / 1024).toFixed(0) + ' KB');
if (errors.length) {
  console.error('  ❌ ' + errors.join('; '));
  process.exit(1);
}
console.log('  ✅ 组装校验通过（无外部 CSS 依赖）');
