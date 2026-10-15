// 全页面健康检查：不只看 JS 错误，还要看**是否渲染出了错误卡片**
//
// 教训（2026-10-12）：存储页出现「加载失败 s is not defined」，
// 但那个异常被 .catch() 吞掉并渲染成 errorCard，
// 所以只监听 pageerror 的检查完全看不到 —— 误判为「全部 OK」。
// 这个脚本改为检查渲染结果：错误卡片 / 空内容 / 关键区块是否存在。
//
// 用法：node build/check-pages.js      （需要本机 8900 在跑）
// 退出码：0=全部正常，1=有页面渲染异常
const CHROME = process.env.MICAM_CHROME || '/root/shot/chrome/chrome-linux64/chrome';
const BASE   = process.env.MICAM_URL || 'http://127.0.0.1:8900/';

// playwright 装在 /root/shot/node_modules（截图工具目录），不在本仓库里。
// 依次尝试解析，都失败就给出可直接执行的提示，而不是抛一堆栈。
function loadPlaywright(){
  var cands = ['playwright', '/root/shot/node_modules/playwright'];
  for(var i = 0; i < cands.length; i++){
    try { return require(cands[i]); } catch(e){}
  }
  console.error('找不到 playwright。安装：cd /root/shot && npm i playwright');
  process.exit(2);
}
const { chromium } = loadPlaywright();
const PAGES = [
  { k:'-',          name:'首页',   must:['家庭概览'] },
  { k:'devices',    name:'摄像头', must:['摄像头列表'] },
  { k:'events',     name:'事件',   must:['事件中心'] },
  { k:'recordings', name:'回放',   must:['录像日历'] },
  { k:'storage',    name:'存储',   must:['总容量'] },
  { k:'ai',         name:'AI',     must:['AI 智能'] },
  { k:'accounts',   name:'账号',   must:[] },
  { k:'settings',   name:'设置',   must:[] },
];
(async () => {
  const b = await chromium.launch({ executablePath:CHROME,
    args:['--no-sandbox','--disable-dev-shm-usage','--disable-gpu','--hide-scrollbars'] });
  const p = await (await b.newContext({viewport:{width:1440,height:1000},locale:'zh-CN'})).newPage();
  const errs=[]; p.on('pageerror',e=>errs.push(String(e).slice(0,110)));
  await p.goto(BASE,{waitUntil:'domcontentloaded'});
  await p.waitForTimeout(2500);

  let bad = 0;
  for(const pg of PAGES){
    if(pg.k !== '-'){ await p.click(`.topnav__menu .tabnav[data-k="${pg.k}"]`); }

    // ⚠️ 不要用固定 sleep 等页面就绪 —— 那会产生**假失败**。
    //    2026-10-13 实测：存储页要扫描录像目录、AI 页要并发拉 4 个接口，
    //    都需要 >3s；固定等 3s 时会误报「缺关键区块: 总容量」，
    //    而实际上页面完全正常（截图确认）。
    //    正确做法：轮询等待「错误卡片出现」或「关键区块出现」或超时。
    const deadline = Date.now() + 20000;
    let snap = null, prevLen = -1, stable = 0;
    while(Date.now() < deadline){
      snap = await p.evaluate((must)=>{
        const ct = document.getElementById('content');
        const txt = ct ? ct.innerText : '';
        const isErr = txt.includes('加载失败');          // errorCard 的标题
        const missing = (must||[]).filter(m=>!txt.includes(m));
        // 仍在加载中（骨架屏 / 读取中）：关键区块可能还没出现，不能据此判失败
        const loading = /加载中|读取中|加载事件|加载录像|加载存储/.test(txt);
        return { isErr, missing, len: txt.length, loading,
                 errText: isErr ? txt.slice(0,90).replace(/\n/g,' ') : '' };
      }, pg.must);
      if(snap.isErr) break;                                  // 明确失败，立即判定
      if(!snap.loading && snap.missing.length===0 && snap.len>40){
        // ⚠️ 还要求**长度稳定**：页面可能先渲染头部、稍后才填主体
        //    （如 AI 页先出「AI 智能」标题，再 paintAITab 填 aiBody）。
        //    只判"关键区块出现"会在这一刻提前通过，量到 296 而非 390 字符。
        if(snap.len === prevLen) stable++;
        else stable = 0;
        prevLen = snap.len;
        if(stable >= 1) break;                               // 连续两次一致 => 已稳定
      }
      await p.waitForTimeout(400);
    }

    const r = snap || { isErr:false, missing:pg.must||[], len:0, errText:'' };
    const ok = !r.isErr && r.missing.length===0 && r.len>40;
    if(!ok) bad++;
    console.log(`${ok?'✅':'❌'} ${pg.name.padEnd(4)} ${r.isErr?('错误卡片: '+r.errText):(r.missing.length?('缺关键区块: '+r.missing.join(',')):('内容 '+r.len+' 字符'))}`);
  }
  console.log(bad? `\n❌ ${bad} 个页面有问题` : '\n✅ 全部页面正常渲染');
  if(errs.length) console.log('JS 错误:', errs);
  await b.close();
  process.exit(bad?1:0);
})();
