#!/usr/bin/env node
/* ============================================================================
 * MiCam 前端 CSS 类名 / CSS 变量一致性检查
 * ----------------------------------------------------------------------------
 * 为什么需要：
 *   · JS 里用了 class="switch__track" 但 CSS 漏写  -> 开关完全不可见
 *   · 48 处用了从未定义的 CSS 变量 --text-2/--text-3 -> 颜色静默失效
 *   · 引入第三方 CSS 框架导致 17 个同名类冲突
 * 这三类 bug 都不会报错，只会在界面上"静默变形"，所以要有脚本防回归。
 *
 * 零依赖：只用 Node 内置模块（fs / path），不安装任何第三方包、不需要构建步骤。
 *
 * 用法：
 *   node build/check-classes.js
 *   node build/check-classes.js --js /tmp/final-js.txt --css platform/fnos/app/www/assets/css
 *   node build/check-classes.js --verbose
 *   退出码：发现「未定义」问题 -> 1，否则 -> 0
 * ========================================================================== */
'use strict';

const fs = require('fs');
const path = require('path');

const SCRIPT_DIR = __dirname;
// 默认读【仓库里的 app.js】而不是 /tmp/final-js.txt：
// /tmp 下的软链接会被清理（2026-09-24 就丢过一次，导致检查器报 ENOENT）。
const DEFAULT_JS = require('path').join(__dirname, '..', 'platform/fnos/app/www/app.js');
const DEFAULT_CSS_DIR = path.resolve(SCRIPT_DIR, '..', 'platform/fnos/app/www/assets/css');

/* ---------------------------------------------------------------------------
 * 0. 命令行参数
 * ------------------------------------------------------------------------- */
function parseArgs(argv) {
  const opt = { js: DEFAULT_JS, css: DEFAULT_CSS_DIR, verbose: false, json: false, help: false };
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    if (a === '--js' || a === '-j') opt.js = argv[++i];
    else if (a === '--css' || a === '-c') opt.css = argv[++i];
    else if (a === '--verbose' || a === '-v') opt.verbose = true;
    else if (a === '--json') opt.json = true;
    else if (a === '--help' || a === '-h') opt.help = true;
    else if (a.startsWith('--js=')) opt.js = a.slice(5);
    else if (a.startsWith('--css=')) opt.css = a.slice(6);
    else { console.error('未知参数: ' + a); opt.help = true; }
  }
  return opt;
}

const HELP = [
  'MiCam 前端类名检查 —— node build/check-classes.js [选项]',
  '',
  '  -j, --js  <文件>   前端 JS（默认 ' + DEFAULT_JS + '）',
  '                     也接受 index.html：会自动取出内联 <script>，行号按 HTML 计',
  '  -c, --css <目录>   含 tokens.css / base.css / components.css / pages.css 的目录',
  '                     默认 ' + DEFAULT_CSS_DIR,
  '  -v, --verbose      额外列出「未使用类名前若干个」「动态类名明细」',
  '      --json         以 JSON 输出（供 CI / 其它脚本消费），不改变退出码',
  '  -h, --help         显示本帮助',
  '',
  '退出码：0 = 通过，1 = 发现未定义类名 / 未定义 CSS 变量，2 = 用法或读取错误',
].join('\n');

/* ---------------------------------------------------------------------------
 * 1. 行号工具
 * ------------------------------------------------------------------------- */
function buildLineIndex(src) {
  const starts = [0];
  for (let i = 0; i < src.length; i++) if (src[i] === '\n') starts.push(i + 1);
  return function lineOf(offset) {
    let lo = 0, hi = starts.length - 1;
    while (lo < hi) {
      const mid = (lo + hi + 1) >> 1;
      if (starts[mid] <= offset) lo = mid; else hi = mid - 1;
    }
    return lo + 1;
  };
}

/* ---------------------------------------------------------------------------
 * 2. JS 扫描：抹掉注释（保留长度 -> 行号不变），同时记录字符串字面量区间
 * ------------------------------------------------------------------------- */
function scanJs(src) {
  const buf = src.split('');
  const literals = [];
  let i = 0;
  const n = src.length;
  let state = 'code';
  let litStart = -1;
  let litQuote = '';

  while (i < n) {
    const c = src[i];
    const c2 = src[i + 1];

    if (state === 'code') {
      if (c === '/' && c2 === '/') { buf[i] = ' '; buf[i + 1] = ' '; state = 'line'; i += 2; continue; }
      if (c === '/' && c2 === '*') { buf[i] = ' '; buf[i + 1] = ' '; state = 'block'; i += 2; continue; }
      if (c === '"' || c === "'") { state = 'str'; litStart = i; litQuote = c; i++; continue; }
      i++; continue;
    }
    if (state === 'line') {
      if (c === '\n') { state = 'code'; i++; continue; }
      buf[i] = ' '; i++; continue;
    }
    if (state === 'block') {
      if (c === '*' && c2 === '/') { buf[i] = ' '; buf[i + 1] = ' '; state = 'code'; i += 2; continue; }
      if (c !== '\n') buf[i] = ' ';
      i++; continue;
    }
    /* state === 'str' */
    if (c === '\\') { i += 2; continue; }
    if (c === litQuote) {
      literals.push({
        start: litStart, end: i,
        contentStart: litStart + 1, contentEnd: i,
        raw: src.slice(litStart + 1, i),
      });
      state = 'code'; i++; continue;
    }
    i++; continue;
  }
  if (state === 'str') { /* 未闭合的字符串：JS 本身有问题，交给 node --check */ }
  return { code: buf.join(''), literals };
}

function literalAt(literals, offset) {
  for (let i = 0; i < literals.length; i++) {
    if (literals[i].start === offset) return literals[i];
  }
  return null;
}

function literalsIn(literals, from, to) {
  const out = [];
  for (let i = 0; i < literals.length; i++) {
    const L = literals[i];
    if (L.start >= from && L.end <= to) out.push(L);
    else if (L.start > to) break;
  }
  return out;
}

function prevNonSpace(src, pos) {
  let i = pos - 1;
  while (i >= 0 && /\s/.test(src[i])) i--;
  return i;
}

/* 左侧被 '+' 接续且自身不以空白开头 -> 首 token 是拼接片段 */
function headConcatBoundary(src, L) {
  const p = prevNonSpace(src, L.start);
  return p >= 0 && src[p] === '+' && L.raw !== '' && !/^\s/.test(L.raw);
}

/* 右侧被 '+' 接续且自身不以空白结尾 -> 末 token 是拼接片段 */
function tailConcatBoundary(src, L) {
  return continuesWithConcat(src, L.end + 1) && L.raw !== '' && !/\s$/.test(L.raw);
}

/* 从 pos 开始看：这个字符串是不是被 '+' 拼接下来的一段？
 * 若后面的拼接片段以空白开头（或为空），则本段末尾的 token 是完整的，不算歧义。 */
function continuesWithConcat(src, pos) {
  let i = pos;
  while (i < src.length && /\s/.test(src[i])) i++;
  if (src[i] !== '+') return false;
  i++;
  while (i < src.length && /\s/.test(src[i])) i++;
  const q = src[i];
  if (q === '"' || q === "'") {
    let j = i + 1;
    while (j < src.length && src[j] !== q) { if (src[j] === '\\') j++; j++; }
    const raw = src.slice(i + 1, j);
    if (raw === '' || /^\s/.test(raw)) return false;
  }
  return true;
}

/* ---------------------------------------------------------------------------
 * 3. JS 侧收集：类名 + 动态片段
 * ------------------------------------------------------------------------- */
const CLASS_TOKEN_RE = /^[A-Za-z_][A-Za-z0-9_-]*$/;
const CLASS_ATTR_VALUE_OK = /^[A-Za-z0-9_\-\s]*$/;

function createCollector() {
  return {
    /* name -> { lines:Set<number>, certain:boolean } */
    classes: new Map(),
    /* key -> { line, kind, code } —— 动态/歧义，单独统计 */
    dynamic: new Map(),
  };
}

function addClass(col, name, line, certain) {
  if (!CLASS_TOKEN_RE.test(name)) return;
  let e = col.classes.get(name);
  if (!e) { e = { lines: new Set(), certain: false }; col.classes.set(name, e); }
  e.lines.add(line);
  if (certain) e.certain = true;
}

function addDynamic(col, key, line, kind, code) {
  if (!key) return;
  if (!col.dynamic.has(key)) col.dynamic.set(key, { line: line, kind: kind, code: code });
}

/* 把一个字符串字面量里的类名 token 收进来（用于拼接链里嵌套的字面量） */
function addLiteralTokens(col, code, L, lineOf) {
  if (L.raw === '') return;
  if (!CLASS_ATTR_VALUE_OK.test(L.raw)) {
    addDynamic(col, '类名字面量（含表达式）', lineOf(L.start), 'lit-expr', L.raw.trim().slice(0, 40));
    return;
  }
  const headAmb = headConcatBoundary(code, L);
  const tailAmb = tailConcatBoundary(code, L);
  const re = /[A-Za-z0-9_-]+/g;
  const toks = [];
  let t;
  while ((t = re.exec(L.raw))) toks.push({ name: t[0], off: t.index });
  toks.forEach(function (tk, i) {
    const amb = (i === 0 && headAmb) || (i === toks.length - 1 && tailAmb);
    addClass(col, tk.name, lineOf(L.contentStart + tk.off), !amb);
    if (amb) addDynamic(col, tk.name + '（拼接片段）', lineOf(L.contentStart + tk.off), 'lit-concat', tk.name + ' + ...');
  });
}

/* 3.1 class="..." / class=\"...\" */
const CLASS_ATTR_RE = /\bclass\s*=\s*(\\?["'])([\s\S]*?)\1/gd;

function collectClassAttrs(code, literals, col, lineOf) {
  let m;
  CLASS_ATTR_RE.lastIndex = 0;
  while ((m = CLASS_ATTR_RE.exec(code))) {
    const raw = m[2];
    const span = m.indices[2];
    const line = lineOf(span[0]);
    const tailConcat = continuesWithConcat(code, m.index + m[0].length);

    /* 属性值里可能混进了 JS 代码，例如：
     *   '<svg class="' + cls + '" ...'                  -> 整体动态
     *   '<button class="chip' + (on ? ' is-active' : '') + '" ...'
     * 做法：只取开头「完整的类名字符前缀」，被截断的最后一个 token 一律按歧义处理，
     * 绝不硬猜拼接后的结果（宁可少报）。 */
    const valueOk = CLASS_ATTR_VALUE_OK.test(raw);
    const prefix = valueOk ? raw : (raw.match(/^[A-Za-z0-9_\-\s]*/) || [''])[0];
    const cut = !valueOk && prefix.length < raw.length;
    if (cut) addDynamic(col, 'class= 拼接表达式', line, 'attr-expr', raw.trim().slice(0, 40));

    const toks = [];
    const re = /[A-Za-z0-9_-]+/g;
    let t;
    while ((t = re.exec(prefix))) toks.push({ name: t[0], off: t.index });

    if (toks.length === 0) {
      if (tailConcat || cut) addDynamic(col, 'class="（动态拼接）"', line, 'attr-dynamic', raw.trim().slice(0, 40));
    } else {
      toks.forEach(function (tk, idx) {
        const isLast = idx === toks.length - 1;
        const certain = !(isLast && (tailConcat || (cut && !/\s$/.test(prefix))));
        addClass(col, tk.name, lineOf(span[0] + tk.off), certain);
        if (!certain) addDynamic(col, tk.name + '（拼接片段）', lineOf(span[0] + tk.off), 'attr-concat', tk.name + ' + ...');
      });
    }
    /* 拼接链里嵌套的字面量同样贡献类名，例如：
     *   "class=\"tag " + (on ? "tag-on" : "tag-off") + "\""
     * 注意排除 type==="ok" 这类比较操作数（它们不是类名）。 */
    if (cut || tailConcat) {
      literalsIn(literals, span[0], span[1]).forEach(function (L) {
        if (isComparisonOperand(code, L)) return;
        addLiteralTokens(col, code, L, lineOf);
      });
    }
  }
}

/* 3.2 className = ... */
/* className = ...（同时覆盖常见的类名变量：cls / klass / classNames）
 * 排除 ==/===/=>，避免把比较、箭头函数当成赋值 */
const CLASSNAME_RE = /\b(?:className|classNames|cls|klass)\s*\+?=(?![=>])\s*/g;

function findExprEnd(src, start) {
  let depth = 0, i = start;
  let q = '';
  while (i < src.length) {
    const c = src[i];
    if (q) {
      if (c === '\\') { i += 2; continue; }
      if (c === q) { q = ''; i++; continue; }
      i++; continue;
    }
    if (c === '"' || c === "'") { q = c; i++; continue; }
    if (c === '(' || c === '[' || c === '{') { depth++; i++; continue; }
    if (c === ')' || c === ']' || c === '}') { if (depth === 0) break; depth--; i++; continue; }
    if (c === ';' && depth === 0) break;
    if (c === '\n' && depth === 0) break;
    i++;
  }
  return i;
}

/* L 是否是比较运算符的操作数（=== / !== / == / !=） */
function isComparisonOperand(code, L) {
  const before = code.slice(Math.max(0, L.start - 4), L.start);
  if (/(===|!==|==|!=)\s*$/.test(before)) return true;
  const after = code.slice(L.end + 1, Math.min(code.length, L.end + 5));
  if (/^\s*(===|!==|==|!=)/.test(after)) return true;
  return false;
}

function collectClassNames(code, literals, col, lineOf) {
  let m;
  CLASSNAME_RE.lastIndex = 0;
  while ((m = CLASSNAME_RE.exec(code))) {
    const start = m.index + m[0].length;
    const end = findExprEnd(code, start);
    /* 只保留「值字面量」：排除 type==="ok" 这类比较操作数，
     * 它们不是类名，混进来会把真正的类名误判成拼接片段。 */
    const lits = literalsIn(literals, start, end).filter(function (L) { return !isComparisonOperand(code, L); });
    if (lits.length === 0) {
      addDynamic(col, 'className = ' + code.slice(start, Math.min(end, start + 24)).trim(), lineOf(start), 'classname-expr', code.slice(start, end).trim().slice(0, 40));
      CLASSNAME_RE.lastIndex = end;
      continue;
    }
    lits.forEach(function (L, idx) {
      const isFirst = idx === 0;
      const isLast = idx === lits.length - 1;
      const headAmb = !isFirst && !/^\s/.test(L.raw);
      /* 下一段以空白开头（或为空串）-> 本段末尾的 token 是完整类名 */
      const tailAmb = !isLast
        ? (!/\s$/.test(L.raw) && lits[idx + 1].raw !== '' && !/^\s/.test(lits[idx + 1].raw))
        : continuesWithConcat(code, L.end + 1);
      if (!CLASS_ATTR_VALUE_OK.test(L.raw)) {
        addDynamic(col, 'className 片段（表达式）', lineOf(L.start), 'classname-expr', L.raw.trim().slice(0, 40));
        return;
      }
      const re = /[A-Za-z0-9_-]+/g;
      const toks = [];
      let t;
      while ((t = re.exec(L.raw))) toks.push({ name: t[0], off: t.index });
      toks.forEach(function (tk, ti) {
        const amb = (ti === 0 && headAmb) || (ti === toks.length - 1 && tailAmb);
        addClass(col, tk.name, lineOf(L.contentStart + tk.off), !amb);
        if (amb) addDynamic(col, tk.name + '（拼接片段）', lineOf(L.contentStart + tk.off), 'classname-concat', tk.name + ' + ...');
      });
    });
    CLASSNAME_RE.lastIndex = end;
  }
}

/* 3.3 classList.add/remove/toggle/contains/replace(...) */
const CLASSLIST_RE = /classList\s*\.\s*(add|remove|toggle|contains|replace)\s*\(/g;

function splitTopLevel(text) {
  const out = [];
  let depth = 0, start = 0, q = '';
  for (let i = 0; i < text.length; i++) {
    const c = text[i];
    if (q) { if (c === '\\') { i++; continue; } if (c === q) q = ''; continue; }
    if (c === '"' || c === "'") { q = c; continue; }
    if (c === '(' || c === '[' || c === '{') { depth++; continue; }
    if (c === ')' || c === ']' || c === '}') { depth--; continue; }
    if (c === ',' && depth === 0) { out.push(text.slice(start, i)); start = i + 1; }
  }
  out.push(text.slice(start));
  return out;
}

function findMatchParen(src, openIdx) {
  let depth = 0, i = openIdx, q = '';
  while (i < src.length) {
    const c = src[i];
    if (q) { if (c === '\\') { i += 2; continue; } if (c === q) q = ''; i++; continue; }
    if (c === '"' || c === "'") { q = c; i++; continue; }
    if (c === '(') { depth++; i++; continue; }
    if (c === ')') { depth--; if (depth === 0) return i; i++; continue; }
    i++;
  }
  return src.length;
}

function blankLiterals(text, litRanges) {
  const a = text.split('');
  litRanges.forEach(function (r) { for (let i = r[0]; i < r[1]; i++) if (a[i] !== '\n') a[i] = ' '; });
  return a.join('');
}

function collectClassList(code, literals, col, lineOf) {
  let m;
  CLASSLIST_RE.lastIndex = 0;
  while ((m = CLASSLIST_RE.exec(code))) {
    const method = m[1];
    const openIdx = m.index + m[0].length - 1;
    const closeIdx = findMatchParen(code, openIdx);
    const argsText = code.slice(openIdx + 1, closeIdx);
    const argsStart = openIdx + 1;
    const args = splitTopLevel(argsText);

    /* toggle 的第 2 参 / contains 只有 1 参 */
    let useCount = args.length;
    if (method === 'toggle' || method === 'contains') useCount = Math.min(1, args.length);

    let cursor = 0;
    for (let ai = 0; ai < args.length; ai++) {
      const argText = args[ai];
      const argAbsStart = argsStart + cursor;
      cursor += argText.length + 1;
      if (ai >= useCount) continue;

      const lits = literalsIn(literals, argAbsStart, argAbsStart + argText.length);
      /* 参数本身就是单个字符串字面量（没有拼接） */
      const pure = lits.length === 1 && /^\s*["'][^"']*["']\s*$/.test(argText);
      const concat = lits.length > 0 && /\+/.test(blankLiterals(argText, lits.map(function (L) { return [L.start - argAbsStart, L.end - argAbsStart]; })));

      if (lits.length === 0) {
        const expr = argText.trim();
        addDynamic(col, 'classList.' + method + '(' + (expr.length > 26 ? expr.slice(0, 26) + '…' : expr) + ')',
          lineOf(argAbsStart), 'classlist-var', expr);
        continue;
      }
      lits.forEach(function (L) {
        if (!CLASS_ATTR_VALUE_OK.test(L.raw)) {
          addDynamic(col, 'classList.' + method + ' 片段（表达式）', lineOf(L.start), 'classlist-expr', L.raw.trim().slice(0, 40));
          return;
        }
        const re = /[A-Za-z0-9_-]+/g;
        let t;
        while ((t = re.exec(L.raw))) {
          const amb = concat && !pure;
          addClass(col, t[0], lineOf(L.contentStart + t.index), !amb);
          if (amb) addDynamic(col, t[0] + '（拼接片段）', lineOf(L.contentStart + t.index), 'classlist-concat', t[0] + ' + ...');
        }
      });
    }
    CLASSLIST_RE.lastIndex = closeIdx;
  }
}

/* 3.4 querySelector / querySelectorAll / closest / matches / getElementsByClassName */
const SELECTOR_RE = /\b(querySelectorAll|querySelector|closest|matches|getElementsByClassName)\s*\(/g;

function classesFromSelector(sel) {
  const out = [];
  let i = 0, depth = 0, q = '';
  while (i < sel.length) {
    const c = sel[i];
    if (q) { if (c === '\\') { i += 2; continue; } if (c === q) q = ''; i++; continue; }
    if (c === '"' || c === "'") { q = c; i++; continue; }
    if (c === '[') { depth++; i++; continue; }
    if (c === ']') { depth--; i++; continue; }
    if (depth === 0 && c === '.') {
      let j = i + 1, name = '';
      while (j < sel.length && /[A-Za-z0-9_-]/.test(sel[j])) { name += sel[j]; j++; }
      if (name && /[A-Za-z_]/.test(name[0])) out.push({ name: name, pos: i });
      i = j; continue;
    }
    i++;
  }
  return out;
}

function collectSelectors(code, literals, col, lineOf) {
  let m;
  SELECTOR_RE.lastIndex = 0;
  while ((m = SELECTOR_RE.exec(code))) {
    const fn = m[1];
    const openIdx = m.index + m[0].length - 1;
    const closeIdx = findMatchParen(code, openIdx);
    const argsText = code.slice(openIdx + 1, closeIdx);
    const argsStart = openIdx + 1;
    SELECTOR_RE.lastIndex = closeIdx;

    const lits = literalsIn(literals, argsStart, closeIdx);
    const blanked = blankLiterals(argsText, lits.map(function (L) { return [L.start - argsStart, L.end - argsStart]; }));
    const hasConcat = /\+/.test(blanked);

    if (lits.length === 0) {
      const expr = argsText.trim();
      addDynamic(col, fn + '(' + (expr.length > 24 ? expr.slice(0, 24) + '…' : expr) + ')',
        lineOf(argsStart), 'selector-expr', expr);
      continue;
    }
    let found = 0;
    lits.forEach(function (L) {
      if (fn === 'getElementsByClassName') {
        const re = /[A-Za-z0-9_-]+/g;
        let t;
        while ((t = re.exec(L.raw))) {
          found++;
          addClass(col, t[0], lineOf(L.contentStart + t.index), !hasConcat);
          if (hasConcat) addDynamic(col, t[0] + '（拼接片段）', lineOf(L.contentStart + t.index), 'selector-concat', L.raw.trim().slice(0, 40));
        }
        return;
      }
      classesFromSelector(L.raw).forEach(function (cl) {
        found++;
        addClass(col, cl.name, lineOf(L.contentStart + cl.pos), !hasConcat);
        if (hasConcat) addDynamic(col, cl.name + '（拼接片段）', lineOf(L.contentStart + cl.pos), 'selector-concat', L.raw.trim().slice(0, 40));
      });
    });
    if (found === 0 && hasConcat) {
      addDynamic(col, fn + '(动态选择器)', lineOf(argsStart), 'selector-expr', argsText.trim().slice(0, 40));
    }
  }
}

/* 3.5 var(--xxx) */
const VAR_RE = /var\(\s*(--[A-Za-z0-9_-]+)\s*[,)]/g;

function collectCssVars(code, col, lineOf) {
  const used = new Map();
  let m;
  VAR_RE.lastIndex = 0;
  while ((m = VAR_RE.exec(code))) {
    const name = m[1];
    const line = lineOf(m.index);
    if (!used.has(name)) used.set(name, new Set());
    used.get(name).add(line);
  }
  /* 动态变量名：var(--' + x + ') */
  const DYN = /var\(\s*(?!--[A-Za-z0-9_-]+\s*[,)])/g;
  while ((m = DYN.exec(code))) {
    addDynamic(col, 'var(动态变量名)', lineOf(m.index), 'var-expr', code.slice(m.index, m.index + 30).split('\n')[0]);
  }
  return used;
}

/* ---------------------------------------------------------------------------
 * 4. CSS 侧收集
 * ------------------------------------------------------------------------- */
function stripCssComments(css) {
  return css.replace(/\/\*[\s\S]*?\*\//g, function (s) { return s.replace(/[^\n]/g, ' '); });
}

function classesFromCssSelector(prelude) {
  const out = [];
  let i = 0, depth = 0, q = '';
  while (i < prelude.length) {
    const c = prelude[i];
    if (q) { if (c === '\\') { i += 2; continue; } if (c === q) q = ''; i++; continue; }
    if (c === '"' || c === "'") { q = c; i++; continue; }
    if (c === '[') { depth++; i++; continue; }
    if (c === ']') { depth--; i++; continue; }
    if (depth === 0 && c === '.') {
      let j = i + 1, name = '';
      while (j < prelude.length && /[A-Za-z0-9_-]/.test(prelude[j])) { name += prelude[j]; j++; }
      if (name && /[A-Za-z_]/.test(name[0])) out.push(name);
      i = j; continue;
    }
    i++;
  }
  return out;
}

/* 逐 { } 扫描，跳过 @media / @supports / @keyframes 的 prelude，收集真正的类选择器 */
function scanCss(css) {
  const classes = new Map();    /* name -> Set<file:line> 简化为首现行号 */
  const vars = new Map();
  const stack = [];
  let i = 0, preludeStart = 0, line = 1;
  let q = '';
  const n = css.length;
  while (i < n) {
    const c = css[i];
    if (c === '\n') line++;
    if (q) { if (c === '\\') { i += 2; continue; } if (c === q) q = ''; i++; continue; }
    if (c === '"' || c === "'") { q = c; i++; continue; }
    if (c === '{') {
      const prelude = css.slice(preludeStart, i);
      if (/^\s*@/.test(prelude)) {
        stack.push('at');
      } else {
        stack.push('rule');
        const names = classesFromCssSelector(prelude);
        for (let k = 0; k < names.length; k++) {
          if (!classes.has(names[k])) classes.set(names[k], line);
        }
      }
      i++; preludeStart = i; continue;
    }
    if (c === '}') { stack.pop(); i++; preludeStart = i; continue; }
    if (c === ';') { i++; preludeStart = i; continue; }
    i++;
  }
  /* 自定义属性：任何作用域下的 --name: */
  const re = /(--[A-Za-z0-9_-]+)\s*:/g;
  let m;
  while ((m = re.exec(css))) {
    const ln = 1 + (css.slice(0, m.index).match(/\n/g) || []).length;
    if (!vars.has(m[1])) vars.set(m[1], ln);
  }
  return { classes: classes, vars: vars };
}

/* ---------------------------------------------------------------------------
 * 4b. 主题作用域分析（:root = 亮色 / .dark = 暗色）
 * -------------------------------------------------------------------------
 * 为什么需要（2026-10-12 实测发现的真 bug）：
 *
 *   --text-1/2/3 三个兼容别名只在 .dark 块里定义，:root 没有。
 *   而 app.js 有 16 处内联样式用 var(--text-3)。
 *   -> 亮色模式下这 16 处 color 声明**无效**、颜色继承父级，
 *      「次要文字」在亮色下没有层次。
 *
 * 但 scanCss 把**所有作用域的变量拍平成一个 Map**，
 * 所以「只在 .dark 定义」的变量会被当成「已定义」，
 * 上面那条 undefined 检查报「未定义的 CSS 变量: 0」——查不出来。
 *
 * 这个函数按作用域分别收集，让「亮色缺失」变成可检测的问题。
 * ------------------------------------------------------------------------- */
function scanThemeScopes(css) {
  const light = new Map();  /* :root 下定义 —— 亮色生效 */
  const dark = new Map();   /* .dark 下定义 —— 暗色覆盖 */

  /* 逐个提取 (selector) { ... } 块，只关心 :root 与 .dark */
  let i = 0;
  const n = css.length;
  while (i < n) {
    const open = css.indexOf('{', i);
    if (open < 0) break;
    const sel = css.slice(i, open).trim();
    /* 找匹配的 }（这两个块内部无嵌套规则） */
    let depth = 1, j = open + 1;
    while (j < n && depth > 0) {
      if (css[j] === '{') depth++;
      else if (css[j] === '}') depth--;
      j++;
    }
    const body = css.slice(open + 1, j - 1);

    /* 取该选择器最后一个逗号分隔项，判断是不是 :root / .dark */
    const last = sel.split(',').pop().trim();
    let target = null;
    if (last === ':root' || /^html:root$/.test(last)) target = light;
    else if (last === '.dark' || /^html\.dark$/.test(last)) target = dark;

    if (target) {
      const vr = /(--[A-Za-z0-9_-]+)\s*:/g;
      let vm;
      while ((vm = vr.exec(body))) {
        const ln = 1 + (css.slice(0, open + 1 + vm.index).match(/\n/g) || []).length;
        if (!target.has(vm[1])) target.set(vm[1], ln);
      }
    }
    i = j;
  }
  return { light: light, dark: dark };
}

/* ---------------------------------------------------------------------------
 * 5. 读取 JS / HTML 源（index.html 会自动抽取内联 script，行号仍按 HTML 计）
 * ------------------------------------------------------------------------- */
function readJsSource(file) {
  const raw = fs.readFileSync(file, 'utf8');
  const looksHtml = /<!DOCTYPE|<html[\s>]/i.test(raw.slice(0, 4000));
  if (!looksHtml) return { text: raw, kind: 'js' };

  const buf = raw.split('');
  for (let i = 0; i < buf.length; i++) if (buf[i] !== '\n') buf[i] = ' ';
  let best = null;
  const re = /<script\b[^>]*>([\s\S]*?)<\/script>/gi;
  let m;
  while ((m = re.exec(raw))) {
    if (!best || m[1].length > best[1].length) best = m;
  }
  if (!best) return { text: raw, kind: 'js' };
  const bodyStart = best.index + best[0].length - best[1].length - '</script>'.length;
  const body = raw.slice(bodyStart, bodyStart + best[1].length);
  for (let i = 0; i < body.length; i++) buf[bodyStart + i] = body[i];
  return { text: buf.join(''), kind: 'html' };
}

/* ---------------------------------------------------------------------------
 * 6. 主流程
 * ------------------------------------------------------------------------- */
function main() {
  const opt = parseArgs(process.argv.slice(2));
  if (opt.help) { console.log(HELP); return 0; }

  let jsSrc;
  try { jsSrc = readJsSource(opt.js); }
  catch (e) { console.error('❌ 读不到 JS: ' + opt.js + ' —— ' + e.message); return 2; }

  let cssFiles;
  try {
    cssFiles = fs.readdirSync(opt.css).filter(function (f) { return /\.css$/i.test(f); }).sort();
  } catch (e) { console.error('❌ 读不到 CSS 目录: ' + opt.css + ' —— ' + e.message); return 2; }
  if (cssFiles.length === 0) { console.error('❌ CSS 目录里没有 .css: ' + opt.css); return 2; }

  const cssClasses = new Map();
  const cssVars = new Map();
  const themeLight = new Map();  /* :root 定义的变量（亮色生效） */
  const themeDark = new Map();   /* .dark 定义的变量 */
  cssFiles.forEach(function (f) {
    const text = stripCssComments(fs.readFileSync(path.join(opt.css, f), 'utf8'));
    const r = scanCss(text);
    r.classes.forEach(function (ln, name) { if (!cssClasses.has(name)) cssClasses.set(name, { file: f, line: ln }); });
    r.vars.forEach(function (ln, name) { if (!cssVars.has(name)) cssVars.set(name, { file: f, line: ln }); });

    const sc = scanThemeScopes(text);
    sc.light.forEach(function (ln, name) { if (!themeLight.has(name)) themeLight.set(name, { file: f, line: ln }); });
    sc.dark.forEach(function (ln, name) { if (!themeDark.has(name)) themeDark.set(name, { file: f, line: ln }); });
  });

  const scanned = scanJs(jsSrc.text);
  const lineOf = buildLineIndex(jsSrc.text);
  const col = createCollector();
  collectClassAttrs(scanned.code, scanned.literals, col, lineOf);
  collectClassNames(scanned.code, scanned.literals, col, lineOf);
  collectClassList(scanned.code, scanned.literals, col, lineOf);
  collectSelectors(scanned.code, scanned.literals, col, lineOf);
  const usedVars = collectCssVars(scanned.code, col, lineOf);

  /* ---- 比对 ---- */
  const undefinedClasses = [];
  let certainCount = 0, ambiguousCount = 0;
  col.classes.forEach(function (e, name) {
    if (e.certain) {
      certainCount++;
      if (!cssClasses.has(name)) undefinedClasses.push({ name: name, lines: Array.from(e.lines).sort(function (a, b) { return a - b; }) });
    } else {
      ambiguousCount++;
    }
  });
  undefinedClasses.sort(function (a, b) { return a.lines[0] - b.lines[0] || a.name.localeCompare(b.name); });

  const undefinedVars = [];
  usedVars.forEach(function (lines, name) {
    if (!cssVars.has(name)) undefinedVars.push({ name: name, lines: Array.from(lines).sort(function (a, b) { return a - b; }) });
  });
  undefinedVars.sort(function (a, b) { return a.lines[0] - b.lines[0] || a.name.localeCompare(b.name); });

  /* 亮色模式下失效的变量：只在 .dark 定义、被 JS 引用，但 :root 没有。
     —— 声明无效 -> 颜色继承父级，不报错、界面上静默降级。
     实例：--text-3（2026-10-12 修复前）。详见 scanThemeScopes 注释。 */
  const darkOnlyVars = [];
  usedVars.forEach(function (lines, name) {
    if (themeDark.has(name) && !themeLight.has(name) && !darkOnlyVars.some(function (v) { return v.name === name; })) {
      darkOnlyVars.push({
        name: name,
        lines: Array.from(lines).sort(function (a, b) { return a - b; }),
        darkLine: themeDark.get(name),
      });
    }
  });
  darkOnlyVars.sort(function (a, b) { return a.lines[0] - b.lines[0] || a.name.localeCompare(b.name); });

  const unusedCssClasses = [];
  cssClasses.forEach(function (loc, name) { if (!col.classes.has(name)) unusedCssClasses.push({ name: name, loc: loc }); });
  unusedCssClasses.sort(function (a, b) { return a.name.localeCompare(b.name); });

  const dynCount = col.dynamic.size;
  const problemCount = undefinedClasses.length + undefinedVars.length + darkOnlyVars.length;

  /* ---- 输出 ---- */
  if (opt.json) {
    console.log(JSON.stringify({
      js: opt.js,
      css: opt.css,
      usedClasses: certainCount,
      ambiguousOnlyClasses: ambiguousCount,
      cssClasses: cssClasses.size,
      cssClassNames: Array.from(cssClasses.keys()).sort(),
      usedClassNames: Array.from(col.classes.keys()).sort(),
      undefinedClasses: undefinedClasses.map(function (u) { return { name: u.name, lines: u.lines }; }),
      undefinedVars: undefinedVars.map(function (u) { return { name: u.name, lines: u.lines }; }),
      darkOnlyVars: darkOnlyVars.map(function (u) { return { name: u.name, lines: u.lines, darkLine: u.darkLine }; }),
      unusedCssClasses: unusedCssClasses.map(function (u) { return u.name; }),
      dynamic: Array.from(col.dynamic.keys()),
      problems: problemCount,
    }, null, 2));
    return problemCount === 0 ? 0 : 1;
  }

  const L = [];
  const pad = function (s, w) { return (s + ' '.repeat(Math.max(0, w - s.length))); };

  L.push('✅ MiCam 前端类名检查');
  L.push('');
  L.push('JS 使用的类名:        ' + certainCount);
  L.push('CSS 定义的类名:       ' + cssClasses.size);
  L.push('');

  if (undefinedClasses.length) {
    L.push('❌ JS 用了但 CSS 未定义（' + undefinedClasses.length + '）:');
    undefinedClasses.forEach(function (u) {
      const extra = u.lines.length > 1 ? '（共 ' + u.lines.length + ' 处）' : '';
      L.push('   ' + pad(u.name, 24) + '第 ' + u.lines[0] + ' 行' + extra);
    });
  } else {
    L.push('✅ JS 用了但 CSS 未定义: 0');
  }
  L.push('');

  if (undefinedVars.length) {
    L.push('❌ 未定义的 CSS 变量（' + undefinedVars.length + '）:');
    undefinedVars.forEach(function (u) {
      const extra = u.lines.length > 1 ? '（共 ' + u.lines.length + ' 处）' : '';
      L.push('   ' + pad(u.name, 24) + '第 ' + u.lines[0] + ' 行' + extra);
    });
  } else {
    L.push('✅ 未定义的 CSS 变量: 0');
  }
  L.push('');

  if (darkOnlyVars.length) {
    L.push('❌ 只在暗色定义、:root 缺失的变量（' + darkOnlyVars.length + '）—— 亮色模式下声明无效:');
    darkOnlyVars.forEach(function (u) {
      const extra = u.lines.length > 1 ? '（JS 引用 ' + u.lines.length + ' 处）' : '';
      L.push('   ' + pad(u.name, 24) + '暗色定义于 ' + u.darkLine.file + ':' + u.darkLine.line +
             '，JS 引用第 ' + u.lines[0] + ' 行' + extra);
    });
    L.push('   ↑ 修法：把该变量按亮色取值补进 :root（见 tokens.css 顶部 :root 块）');
  } else {
    L.push('✅ 亮暗模式变量对称（无「只在暗色定义」的被引用变量）');
  }
  L.push('');

  L.push('ℹ CSS 定义但未使用: ' + unusedCssClasses.length + ' 个（仅提示）');
  L.push('ℹ 动态类名已跳过: ' + dynCount + ' 个');
  L.push('');

  if (opt.verbose) {
    if (unusedCssClasses.length) {
      L.push('-- 未使用类名（前 40）:');
      unusedCssClasses.slice(0, 40).forEach(function (u) { L.push('   ' + u.name + '  (' + u.loc.file + ':' + u.loc.line + ')'); });
      L.push('');
    }
    if (dynCount) {
      L.push('-- 动态类名明细:');
      col.dynamic.forEach(function (v, k) { L.push('   ' + k + '   第 ' + v.line + ' 行  [' + v.kind + ']'); });
      L.push('');
    }
  }

  L.push(problemCount === 0 ? '✅ 检查通过' : '❌ 发现 ' + problemCount + ' 个问题');
  console.log(L.join('\n'));

  return problemCount === 0 ? 0 : 1;
}

process.exitCode = main();
