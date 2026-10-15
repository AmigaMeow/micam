#!/usr/bin/env node
/**
 * build/check-css-hygiene.js
 *
 * 三条规则，对应 PHASE0-TOKEN-AUDIT.md §5/§3 的落地要求：
 *
 *   Rule 1  禁止在 spacing 属性（padding/margin/gap 及其方向变体）里
 *           直接写字面 px，必须走 var(--xxx)。
 *           —— 防止"6px 第二套栅格"这种漂移再发生。
 *
 *   Rule 2  同一个选择器，在同一个文件、同一个嵌套上下文（同一层
 *           @media / @keyframes）里不允许出现两次。
 *           —— 对应 §5.2 「131 处同文件重复定义」。
 *
 *   Rule 3  pages.css 不允许重新定义 components.css 里已经定义过的
 *           class 选择器（BEM 子元素、组件根类等）。
 *           —— 对应 §5.1 「.card__head 跨文件覆盖」那条。
 *
 * 用法：
 *   node build/check-css-hygiene.js
 *
 * 退出码：
 *   0 = 无违规
 *   1 = 有违规（可接入 CI / pre-commit）
 *
 * 豁免单条声明：
 *   在声明同一行加注释 `/* css-lint-allow *\/`，Rule 1 会跳过该行。
 *   例：  margin-top: 2px; /* css-lint-allow: optical-nudge *\/
 */

"use strict";

const fs = require("fs");
const path = require("path");

// ============================================================
// 配置区：按你们 Phase 0 的最终决策调整这里
// ============================================================

const CSS_DIR = path.join(__dirname, "..", "platform/fnos/app/www/assets/css");

const FILES = {
  tokens: path.join(CSS_DIR, "tokens.css"),
  components: path.join(CSS_DIR, "components.css"),
  pages: path.join(CSS_DIR, "pages.css"),
};

// Rule 1：会被检查的 spacing 属性
const SPACING_PROPS = [
  "padding", "padding-top", "padding-right", "padding-bottom", "padding-left",
  "margin", "margin-top", "margin-right", "margin-bottom", "margin-left",
  "gap", "row-gap", "column-gap",
];

// Rule 1：允许裸写的字面值（不需要走 var 的例外）
// 默认只放行 0 本身；其余等你们定好 --gap-icon-text / --nudge-optical
// 这些 token 之后，把对应的值加进来即可解除报警。
// 格式：{ value: '6px', properties: ['margin-top'] } 表示
//       "margin-top: 6px" 这种组合允许裸写；不填 properties 则所有属性都放行该值。
const ALLOWED_LITERAL_PX = [
  { value: "0" },
  { value: "0px" },
  // 示例（等 Phase 0 §10 决策落地后取消注释）：
  // { value: '2px', properties: ['margin-top'] },   // --nudge-optical
  // { value: '6px' },                                // --gap-icon-text / --gap-stack-tight
  // { value: '10px' },                               // --pad-pill-x（注意：仅限 padding 的左右两值场景，
  //                                                   //   脚本按 token 粒度放行，场景粒度仍需人工 review）
];

const INLINE_SUPPRESS_MARK = "css-lint-allow";

// ============================================================
// 极简 CSS 解析：把文件切成 { selector, declarations[], context[], line } 的规则列表
// 不处理字符串里带花括号这种极端场景（这份代码库目前没有这种写法）
// ============================================================

function stripComments(css) {
  return css.replace(/\/\*[\s\S]*?\*\//g, (m) => {
    // 保留行号：用等量的换行符占位，不破坏后续行号计算
    const lines = m.match(/\n/g);
    return lines ? "\n".repeat(lines.length) : "";
  });
}

function splitTopLevelSelectors(selectorText) {
  // 按顶层逗号切分选择器列表，忽略 :not(a, b) 这种括号内的逗号
  const result = [];
  let depth = 0;
  let current = "";
  for (const ch of selectorText) {
    if (ch === "(") depth++;
    if (ch === ")") depth--;
    if (ch === "," && depth === 0) {
      result.push(current.trim());
      current = "";
    } else {
      current += ch;
    }
  }
  if (current.trim()) result.push(current.trim());
  return result;
}

/**
 * 解析 CSS 文本为规则列表。
 * 返回: [{ selectors: string[], selectorRaw: string, body: string,
 *          context: string[], line: number }]
 * context 是从外到内的 at-rule 栈，比如 ['@media (min-width: 768px)']
 * 顶层规则 context 为 []。
 * @keyframes 内部的 0% / 50% / to 等会被当作 selector，
 * context 会包含 '@keyframes xxx'，天然按动画名隔离判重。
 */
function parseRules(css) {
  const clean = stripComments(css);
  const rules = [];
  const contextStack = [];
  let i = 0;
  let line = 1;
  const len = clean.length;

  function countLines(from, to) {
    let n = 0;
    for (let k = from; k < to; k++) if (clean[k] === "\n") n++;
    return n;
  }

  while (i < len) {
    // 跳过空白，顺带计行号
    const wsMatch = clean.slice(i).match(/^\s+/);
    if (wsMatch) {
      line += countLines(i, i + wsMatch[0].length);
      i += wsMatch[0].length;
      continue;
    }

    if (clean[i] === "}") {
      contextStack.pop();
      i++;
      continue;
    }

    // 读取到下一个 { 或 ; 或文件尾，作为 "prelude"（选择器 或 at-rule 头）
    let j = i;
    let depth = 0;
    while (j < len && !((clean[j] === "{" || clean[j] === ";") && depth === 0)) {
      if (clean[j] === "(") depth++;
      if (clean[j] === ")") depth--;
      j++;
    }

    const prelude = clean.slice(i, j).trim();
    const ruleStartLine = line + countLines(i, i); // 当前行号

    if (clean[j] === ";") {
      // 没有花括号的语句（比如 @import），跳过
      line += countLines(i, j + 1);
      i = j + 1;
      continue;
    }

    if (clean[j] !== "{") {
      // 文件结束但没闭合，容错退出
      break;
    }

    // 找到匹配的 }，记录 body
    let k = j + 1;
    let braceDepth = 1;
    while (k < len && braceDepth > 0) {
      if (clean[k] === "{") braceDepth++;
      if (clean[k] === "}") braceDepth--;
      k++;
    }
    const body = clean.slice(j + 1, k - 1);
    const bodyStartLine = line + countLines(i, j + 1);

    if (prelude.startsWith("@")) {
      // at-rule：@media / @keyframes / @supports / @font-face ...
      // 把它压入上下文栈，递归解析 body 里的内容
      contextStack.push(prelude);
      const nested = parseRulesWithOffset(body, bodyStartLine, contextStack);
      rules.push(...nested);
      contextStack.pop();
    } else if (prelude) {
      // 普通规则
      const selectors = splitTopLevelSelectors(prelude);
      rules.push({
        selectors,
        selectorRaw: prelude,
        body,
        context: [...contextStack],
        line: ruleStartLine,
      });
    }

    line += countLines(i, k);
    i = k;
  }

  return rules;

  // 内部辅助：解析嵌套 body，行号基于外层传入的 startLine
  function parseRulesWithOffset(innerCss, startLine, ctxStackRef) {
    // 简化处理：直接复用同一个解析逻辑，但重新计算行号基线
    const innerRules = parseRules(innerCss);
    return innerRules.map((r) => ({
      ...r,
      line: r.line + startLine - 1,
      context: [...ctxStackRef, ...r.context],
    }));
  }
}

// ============================================================
// Rule 1：字面 px 的 spacing 声明
// ============================================================

/**
 * 扫描整个文件，返回所有位于声明块（{} 内）里的声明及其行号。
 *
 * ⚠️ 为什么不能像最初那样用 `^\s*prop\s*:` 逐行匹配：
 * 本项目大量使用**单行规则**写法，例如
 *   .ai-other { display:flex; align-items:center; padding: 6px 0; }
 * 此时属性前面是 `; ` 而不是行首，`^\s*` 会整条漏掉。
 * 实测（2026-10-12）：逐行正则只报 96 处，真实 370 处 —— 漏报 74%。
 */
function scanDeclarations(css) {
  const clean = stripComments(css); // 保留换行，行号不失真
  const lineOf = new Int32Array(clean.length);
  let ln = 1;
  for (let i = 0; i < clean.length; i++) {
    lineOf[i] = ln;
    if (clean[i] === "\n") ln++;
  }

  const out = [];
  let depth = 0;
  let start = -1; // 当前属性名的起始下标

  for (let i = 0; i < clean.length; i++) {
    const c = clean[i];
    if (c === "{") { depth++; start = -1; continue; }
    if (c === "}") { depth--; start = -1; continue; }
    if (depth <= 0) continue; // 选择器 / @media 前导部分，跳过
    if (c === ";") { start = -1; continue; }

    if (c === ":" && start >= 0) {
      const prop = clean.slice(start, i).trim();
      // 排除嵌套选择器的伪类（如 @media 里的 `.a:hover`）：
      // 属性名只允许 [A-Za-z0-9_-]
      if (!/^[-\w]+$/.test(prop)) { start = -1; continue; }
      let j = i + 1;
      while (j < clean.length && clean[j] !== ";" && clean[j] !== "}") j++;
      out.push({
        prop,
        value: clean.slice(i + 1, j).replace(/\s+/g, " ").trim(),
        line: lineOf[start],
      });
      start = -1;
      i = j - 1;
      continue;
    }

    if (start < 0 && !/\s/.test(c)) start = i;
  }
  return out;
}

function checkLiteralPx(filePath, fileLabel) {
  const raw = fs.readFileSync(filePath, "utf8");
  const lines = raw.split("\n");
  const violations = [];

  for (const decl of scanDeclarations(raw)) {
    if (!SPACING_PROPS.includes(decl.prop)) continue;
    const lineText = lines[decl.line - 1] || "";
    if (lineText.includes(INLINE_SUPPRESS_MARK)) continue;

    for (const tok of decl.value.split(/\s+/)) {
      if (!/^-?\d+(\.\d+)?px$/.test(tok)) continue;
      const allowed = ALLOWED_LITERAL_PX.some(
        (rule) =>
          rule.value === tok &&
          (!rule.properties || rule.properties.includes(decl.prop)),
      );
      if (allowed) continue;
      violations.push({
        file: fileLabel,
        line: decl.line,
        property: decl.prop,
        value: decl.value,
        token: tok,
        text: lineText.trim(),
      });
    }
  }

  return violations;
}

// ============================================================
// Rule 2：同文件同上下文内选择器重复定义
// ============================================================

function checkDuplicateSelectors(filePath, fileLabel) {
  const raw = fs.readFileSync(filePath, "utf8");
  const rules = parseRules(raw);
  const seen = new Map(); // key: context+selector -> [line,...]

  for (const rule of rules) {
    const ctxKey = rule.context.join(" > ");
    for (const sel of rule.selectors) {
      const key = `${ctxKey}::${sel}`;
      if (!seen.has(key)) seen.set(key, []);
      seen.get(key).push(rule.line);
    }
  }

  const violations = [];
  for (const [key, lineList] of seen.entries()) {
    if (lineList.length > 1) {
      const [ctx, sel] = key.split("::");
      violations.push({
        file: fileLabel,
        selector: sel,
        context: ctx || "(top-level)",
        lines: lineList,
      });
    }
  }
  return violations;
}

// ============================================================
// Rule 3：pages.css 重新定义 components.css 的 class 选择器
// ============================================================

function collectClassSelectors(filePath) {
  const raw = fs.readFileSync(filePath, "utf8");
  const rules = parseRules(raw);
  const map = new Map(); // selector -> [{line, context}]

  for (const rule of rules) {
    for (const sel of rule.selectors) {
      if (!sel.startsWith(".")) continue; // 只看 class 选择器
      if (!map.has(sel)) map.set(sel, []);
      map.get(sel).push({ line: rule.line, context: rule.context.join(" > ") });
    }
  }
  return map;
}

function checkCrossFileOverride() {
  const componentsSelectors = collectClassSelectors(FILES.components);
  const pagesSelectors = collectClassSelectors(FILES.pages);

  const violations = [];
  for (const [sel, compDefs] of componentsSelectors.entries()) {
    if (pagesSelectors.has(sel)) {
      violations.push({
        selector: sel,
        componentsLines: compDefs.map((d) => d.line),
        pagesLines: pagesSelectors.get(sel).map((d) => d.line),
      });
    }
  }
  return violations;
}

// ============================================================
// Rule 4（warn）：pages.css 用「祖先限定 / 伪类」重新定义组件内部样式
// ------------------------------------------------------------
// Rule 3 只比较「完全相同的选择器文本」，抓不到这类写法：
//
//   components.css   .card__title { gap: var(--space-2) }      /* 8px */
//   pages.css        .card__head .card__title { gap: 6px }     /* 6px，实际生效 */
//
// 选择器文本不同 → Rule 3 放行，但视觉上确实覆盖了组件内部样式。
// 这正是 PHASE0-TOKEN-AUDIT.md §5.1 那条 .card__head 问题的完整链条。
//
// 判定逻辑（三道闸，全部满足才报）：
//   ① pages.css 选择器的**主体**（最后一个 compound，如 `.a .b` 里的 `.b`）
//      含有 components.css 已定义的 class；
//   ② 该 class 属于「组件内部结构」——
//      是 BEM 元素（含 `__`），或它自己拥有 BEM 子元素（说明是个组件块）；
//   ③ pages.css 里声明的属性与 components.css 里同名 class 的属性**真重叠**。
//
// 闸 ② 是为了排除 `.icon` / `.btn` / `.badge` 这类**通用原语**：
//   `.home-head__sub .icon { margin-top: 2px }` 是合法的上下文微调，
//   不是「覆盖组件内部样式」。实测闸 ② 把候选从 72 处降到 21 处。
//
// ⚠️ **严重级别为 warn，不计入退出码**。原因（实测）：
//   即使过了三道闸，仍有**刻意为之**的合法覆盖，例如
//     pages.css:1950  `.ai-row2 > .card + .card { margin-top: 0 }`
//   上面紧邻的注释写明「全局 .card + .card{margin-top:20px} 在多列网格里
//   会把右侧卡片推下去 20px —— 网格内清零」。这是有意的，不是漂移。
//   这类判断机器做不了，所以只提示、不阻断。
// ============================================================

// 状态/修饰类前缀：这些是状态 token，pages 里限定祖先使用属合法
const STATE_CLASS_PREFIX = /^(is-|has-|js-)/;

function stripParens(s) {
  let out = "";
  let depth = 0;
  for (const c of s) {
    if (c === "(") depth++;
    if (c === ")") depth--;
    if (depth === 0) out += c;
  }
  return out;
}

function classesIn(text) {
  return [...stripParens(text).matchAll(/\.([A-Za-z0-9_-]+)/g)].map((m) => m[1]);
}

/** 取选择器的「主体」：最后一个 compound（`.a > .b:hover` → `.b:hover`） */
function subjectOf(selector) {
  const parts = selector.split(/\s*[>+~]\s*|\s+/).filter(Boolean);
  return parts[parts.length - 1] || selector;
}

function propsIn(body) {
  return new Set(
    body
      .split(";")
      .map((d) => d.split(":")[0].trim())
      .filter((p) => /^[-\w]+$/.test(p)),
  );
}

/** 同 splitTopLevelSelectors，但额外返回每个选择器在原文中的起始下标
 *  —— 用于给分组选择器（`.a,\n.b,\n.c { }`）各自算出**自己那一行**的行号。 */
function splitTopLevelSelectorsWithIndex(selectorText) {
  const result = [];
  let depth = 0;
  let current = "";
  let start = 0;
  for (let i = 0; i < selectorText.length; i++) {
    const ch = selectorText[i];
    if (ch === "(") depth++;
    else if (ch === ")") depth--;
    else if (ch === "," && depth === 0) {
      const lead = current.length - current.replace(/^\s+/, "").length;
      if (current.trim()) result.push({ text: current.trim(), index: start + lead });
      current = "";
      start = i + 1;
      continue;
    }
    current += ch;
  }
  if (current.trim()) {
    const lead = current.length - current.replace(/^\s+/, "").length;
    result.push({ text: current.trim(), index: start + lead });
  }
  return result;
}

/** 把文件解析成 { sel, line, props } 列表（跳过 at-rule 前导） */
function rulesWithProps(filePath) {
  const src = stripComments(fs.readFileSync(filePath, "utf8"));
  const out = [];
  const re = /([^{}]+)\{([^{}]*)\}/g;
  let m;
  while ((m = re.exec(src))) {
    if (/^\s*@/.test(m[1])) continue;
    const props = propsIn(m[2]);
    /* ⚠️ 行号要算到**每个选择器自己的起始位置**，不能用 m.index 也不用 { 的位置：
       · 用 m.index 会指向上一条规则的结尾（2026-10-12 实测 96% 错位）
       · 用 `{` 的位置会让分组选择器 `.a,\n.b,\n.c { }` 的三个选择器
         全部指向最后一行（实测 7 处），按行号查证会找错选择器
       见 --self-test。 */
    for (const s of splitTopLevelSelectorsWithIndex(m[1])) {
      const line = (src.slice(0, m.index + s.index).match(/\n/g) || []).length + 1;
      out.push({ sel: s.text, line, props });
    }
  }
  return out;
}

function checkAncestorOverride() {
  const compRules = rulesWithProps(FILES.components);
  const pagesRules = rulesWithProps(FILES.pages);

  // components.css 里每个 class 声明的属性集合
  const compProps = new Map();
  for (const r of compRules) {
    for (const c of classesIn(subjectOf(r.sel))) {
      if (!compProps.has(c)) compProps.set(c, new Set());
      for (const p of r.props) compProps.get(c).add(p);
    }
  }
  // 拥有 BEM 子元素的 class = 组件块（闸 ② 的依据）
  const blocksWithChildren = new Set();
  for (const c of compProps.keys()) {
    if (c.includes("__")) blocksWithChildren.add(c.split("__")[0]);
  }

  const hits = [];
  for (const r of pagesRules) {
    const subjClasses = classesIn(subjectOf(r.sel)).filter(
      (c) => compProps.has(c) && !STATE_CLASS_PREFIX.test(c),
    );
    if (!subjClasses.length) continue;

    // 闸 ②：必须是组件内部结构
    const internal = subjClasses.filter(
      (c) => c.includes("__") || blocksWithChildren.has(c),
    );
    if (!internal.length) continue;

    // 闸 ③：属性真重叠
    const overlap = new Set();
    for (const c of internal) {
      for (const p of r.props) if (compProps.get(c).has(p)) overlap.add(p);
    }
    if (!overlap.size) continue;

    hits.push({
      selector: r.sel,
      line: r.line,
      base: internal.join(", "),
      overlap: [...overlap],
    });
  }
  return hits;
}

// ============================================================
// Rule 5（warn）：冗余断点
// ------------------------------------------------------------
// 来源：对方会话（session-281d55c6）在 commit 2250a28 里建议
// 「增加同选择器 + 属性集重叠 + 断点不同的判据」。
//
// ⚠️ 但对方的判据定义**过宽**：单纯「断点不同」是**合法**的响应式写法，
//    报它会产生大量误报。经 fixture 验证后收窄为：
//
//    判定条件（三条全满足）：
//      ① 同一文件、同一选择器、**声明体逐字相同**；
//      ② 出现在 ≥2 个**同向**断点里（全是 max-width 或全是 min-width）；
//      ③ 这些断点的取值**互不相同**。
//
//    为什么满足①②③即为冗余：
//      两条声明体相同 ⇒ 在两者的重叠区间内，谁生效结果都一样
//      ⇒ 范围较窄的那条恒为冗余，**与源序无关**
//      （⚠️ 「与源序无关」是 fixture 逼出来的修正：
//        我第一版写成「较大值包含较小值」，结果源序颠倒的用例判错）
//
//    不报的情况：
//      · 声明体不同 → 可能是刻意覆盖（合法）
//      · min-width 与 max-width 混合 → 无包含关系
//
// 严重级别 warn：真实仓库当前为 0 组（对方已清），
// 保留它用于防止回归。
// ============================================================

function checkRedundantBreakpoints(filePath, fileLabel) {
  const src = stripComments(fs.readFileSync(filePath, "utf8"));
  const rows = [];

  // 逐条匹配 @media（函数体只是一条规则的常规写法）
  const outer = /@media\s*\(([^)]*)\)\s*\{([^{}]*(?:\{[^{}]*\}[^{}]*)*)\}/g;
  let m;
  while ((m = outer.exec(src))) {
    const cond = m[1].replace(/\s+/g, " ").trim();
    const line = (src.slice(0, m.index).match(/\n/g) || []).length + 1;
    const inner = /[^{}]+\{[^{}]*\}/g;
    let mm;
    while ((mm = inner.exec(m[2]))) {
      const s = mm[0];
      const i = s.indexOf("{");
      rows.push({
        cond,
        selector: s.slice(0, i).trim(),
        body: s.slice(i + 1, s.lastIndexOf("}")).replace(/\s+/g, " ").trim(),
        line,
      });
    }
  }

  const groups = new Map();
  for (const r of rows) {
    const key = `${r.selector}::${r.body}`;
    if (!groups.has(key)) groups.set(key, []);
    groups.get(key).push(r);
  }

  const hits = [];
  for (const [key, list] of groups) {
    if (list.length < 2) continue;
    const conds = [...new Set(list.map((r) => r.cond))];
    if (conds.length < 2) continue; // 断点完全相同另有 Rule 2 管
    const allMax = conds.every((c) => /max-width/.test(c));
    const allMin = conds.every((c) => /min-width/.test(c));
    if (!allMax && !allMin) continue; // 混合方向无包含关系

    const [selector, body] = key.split("::");
    hits.push({
      file: fileLabel,
      selector,
      body,
      places: list.map((r) => ({ cond: r.cond, line: r.line })),
    });
  }
  return hits;
}

// ============================================================
// 自检：验证各解析器报出的行号真的指向该选择器
// ------------------------------------------------------------
// 为什么需要（2026-10-12 实测）：
//   rulesWithProps 用 `src.slice(0, m.index)` 算行号，而 m[1] 里含
//   「上一条规则结尾到本条 { 之间的全部内容」，m.index 指向的是**上一条的结尾**。
//   结果 1643 条里 1577 条（96%）行号错位，按行号查证会读到前一条规则。
//   这个 bug 不会让计数变错，只会让人**按错误的位置去改代码** —— 更危险。
//
// 用法：node build/check-css-hygiene.js --self-test
// ============================================================

function selfTest() {
  const fsx = require("fs");
  let pass = 0;
  let fail = 0;
  const check = (name, ok, detail) => {
    if (ok) { pass++; console.log(`  ✅ ${name}`); }
    else { fail++; console.log(`  ❌ ${name}${detail ? "  " + detail : ""}`); }
  };

  console.log("=== 解析器自检 ===\n");

  for (const [file, label] of [
    [FILES.components, "components.css"],
    [FILES.pages, "pages.css"],
  ]) {
    const raw = fsx.readFileSync(file, "utf8");
    const lines = raw.split("\n");

    // 1) rulesWithProps 的行号：该行应包含选择器的首个类名/标签
    //    注意：分组选择器（`.a,\n.b,\n.c { }`）经 splitTopLevelSelectors 展开后
    //    多个选择器**共享同一行号**，这是正确的，所以判定要看
    //    「该行所属的整条规则」是否包含这个选择器，而不是「该行文本」。
    const rules = rulesWithProps(file);
    let bad = 0;
    let firstBad = null;
    for (const r of rules) {
      const text = (lines[r.line - 1] || "").trim();
      if (text.endsWith("{")) {
        // 选择器单独占一行（或以 { 结尾）→ 该行应含首段
        const head = r.sel.split(/[\s,:>+~.]+/).filter(Boolean)[0];
        if (head && !text.includes(head)) {
          bad++;
          if (!firstBad) firstBad = `${r.sel} 报到 L${r.line}，该行是「${text.slice(0, 40)}」`;
        }
      } else {
        // 分组或以 `{ 声明 }` 收尾的单行规则 → 该行应含任意选择器片段
        const frag = r.sel.split(/[\s,:>+~]+/).filter(Boolean).pop();
        if (frag && !text.includes(frag.split("__").pop())) {
          bad++;
          if (!firstBad) firstBad = `${r.sel} 报到 L${r.line}，该行是「${text.slice(0, 40)}」`;
        }
      }
    }
    check(
      `rulesWithProps 行号 (${label})  ${rules.length} 条`,
      bad === 0,
      bad ? `${bad} 条错位，例：${firstBad}` : "",
    );

    // 2) parseRules 的行号：该行应包含选择器首段
    const rules2 = parseRules(raw);
    let bad2 = 0;
    let firstBad2 = null;
    for (const r of rules2) {
      const text = (lines[r.line - 1] || "").trim();
      const head = r.selectorRaw.split(/[\s,:>+~.]+/).filter(Boolean)[0];
      if (head && !text.includes(head)) {
        bad2++;
        if (!firstBad2) firstBad2 = `${r.selectorRaw} 报到 L${r.line}`;
      }
    }
    check(
      `parseRules 行号 (${label})  ${rules2.length} 条`,
      bad2 === 0,
      bad2 ? `${bad2} 条错位，例：${firstBad2}` : "",
    );
  }

  console.log(`\n${fail === 0 ? "✅ 全部通过" : "❌ 有失败项"}  通过 ${pass} / 失败 ${fail}`);
  process.exit(fail === 0 ? 0 : 1);
}

// ============================================================
// 输出
// ============================================================

function printSection(title, items, formatter) {
  console.log(`\n[${title}]`);
  if (items.length === 0) {
    console.log("  ✓ 无违规");
    return;
  }
  items.forEach((item) => console.log("  " + formatter(item)));
  console.log(`  共 ${items.length} 处`);
}

function main() {
  if (process.argv.includes("--self-test")) return selfTest();

  console.log("=== CSS Hygiene Check ===");

  let totalViolations = 0;

  // Rule 1
  const r1 = [
    ...checkLiteralPx(FILES.components, "components.css"),
    ...checkLiteralPx(FILES.pages, "pages.css"),
  ];
  printSection(
    "Rule 1  字面 px 的 spacing 声明（未走 var）",
    r1,
    (v) => `${v.file}:${v.line}  ${v.property}: ${v.value};  (裸值 ${v.token})`,
  );
  totalViolations += r1.length;

  // Rule 2
  const r2 = [
    ...checkDuplicateSelectors(FILES.components, "components.css"),
    ...checkDuplicateSelectors(FILES.pages, "pages.css"),
  ];
  printSection(
    "Rule 2  同文件同上下文内选择器重复定义",
    r2,
    (v) =>
      `${v.file}  ${v.selector}  (${v.context})  —— 定义于第 ${v.lines.join(", ")} 行`,
  );
  totalViolations += r2.length;

  // Rule 3
  const r3 = checkCrossFileOverride();
  printSection(
    "Rule 3  pages.css 重新定义 components.css 的选择器",
    r3,
    (v) =>
      `${v.selector}  components.css:${v.componentsLines.join(",")}  →  pages.css:${v.pagesLines.join(",")}（生效）`,
  );
  totalViolations += r3.length;

  // Rule 4（warn，不计入退出码）
  const r4 = checkAncestorOverride();
  printSection(
    "Rule 4 (warn)  pages.css 用祖先限定覆盖组件内部样式",
    r4,
    (v) =>
      `pages.css:${v.line}  ${v.selector}  —— 覆盖 .${v.base} 的 [${v.overlap.join(", ")}]`,
  );
  console.log(
    "  ↑ 提示级：含刻意为之的合法覆盖（如 .ai-row2 > .card + .card），请人工确认。",
  );

  // Rule 5（warn，不计入退出码）
  const r5 = [
    ...checkRedundantBreakpoints(FILES.components, "components.css"),
    ...checkRedundantBreakpoints(FILES.pages, "pages.css"),
  ];
  printSection(
    "Rule 5 (warn)  冗余断点（同选择器 + 同声明体 + 同向不同断点）",
    r5,
    (v) =>
      `${v.file}  ${v.selector}  —— ${v.places.map((p) => `@media (${p.cond}) L${p.line}`).join("  /  ")}`,
  );
  console.log(
    "  ↑ 提示级：声明体相同则窄断点恒为冗余，可删；来源见 PEER-RESPONSE-2250a28.md §2.2。",
  );

  console.log("\n" + "=".repeat(40));
  const warnCount = r4.length + r5.length;
  if (totalViolations === 0) {
    console.log(
      `PASS：未发现违规。${warnCount ? `（另有 ${warnCount} 处提示级，不计入退出码）` : ""}`,
    );
    process.exit(0);
  } else {
    console.log(
      `FAILED：共发现 ${totalViolations} 处违规（Rule 1-3 合计）。` +
        (warnCount
          ? ` 另有 ${warnCount} 处提示级（Rule 4 ${r4.length} + Rule 5 ${r5.length}）。`
          : ""),
    );
    process.exit(1);
  }
}

main();
