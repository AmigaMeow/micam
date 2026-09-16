#!/usr/bin/env node
/* 组装 index.html
 *
 * 为什么用脚本：index.html 由三部分组成——
 *   1. 外壳（HTML）
 *   2. 图标 sprite（构建产物）
 *   3. 应用逻辑（JS）
 * 分开维护，组装时合并成一个文件（飞牛插件单文件部署最简单）。
 */
const fs = require('fs');
const path = require('path');

const W = '/root/Deepseek/micam/platform/fnos/app/www';
const SKEL = '/root/Deepseek/micam/platform/fnos/app/shell.html';
const JS = '/tmp/final-js.txt';

const shell = fs.readFileSync(SKEL, 'utf8');
const sprite = fs.readFileSync(path.join(W, 'assets/img/icons.svg'), 'utf8');
const js = fs.readFileSync(JS, 'utf8');

const out = shell
  .replace('<!--SPRITE-->', sprite)
  .replace('<!--APP_JS-->', js);

fs.writeFileSync(path.join(W, 'index.html'), out);

// 基本校验
const errors = [];
if (out.indexOf('<!DOCTYPE') !== 0) errors.push('DOCTYPE 不在开头');
if ((out.match(/<script>/g) || []).length !== 1) errors.push('script 标签数量异常');
if (!out.includes('id="i-camera"')) errors.push('图标 sprite 缺失');
if (/<script>\s*<\//.test(out)) errors.push('script 内容为空');

console.log('  index.html: ' + out.length + ' 字符, ' + out.split('\n').length + ' 行');
if (errors.length) {
  console.error('  ❌ ' + errors.join('; '));
  process.exit(1);
}
console.log('  ✅ 组装校验通过');
