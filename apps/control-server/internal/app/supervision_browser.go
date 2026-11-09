package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

// 驱动和启动规格由服务端构造，模型只提供受限步骤。浏览器、服务及后裔均属于
// 同一持久作业对象；报告只在整个容器及输出确认收敛后才能成为验证证据。
const supervisionBrowserDriver = `const fs=require('node:fs');const path=require('node:path');const cp=require('node:child_process');const {createRequire}=require('node:module');
const cfg=JSON.parse(fs.readFileSync(process.argv[2],'utf8'));let server,browser,page;let result={passed:false,steps:[],console:[],url:cfg.check.url,screenshot:'screenshot.png'};
const sleep=ms=>new Promise(r=>setTimeout(r,ms));
(async()=>{try{
 let playwright;for(const base of cfg.moduleBases){try{playwright=createRequire(path.join(base,'package.json'))('playwright');break}catch{}}
 if(!playwright)throw Error('未安装可用的 Playwright，浏览器验收能力缺失');
 server=cp.spawn(cfg.server[0],cfg.server.slice(1),{cwd:cfg.cwd,shell:false,windowsHide:true,stdio:['ignore','pipe','pipe']});let serverError;
 server.on('error',e=>serverError=e);server.stdout.on('data',b=>process.stdout.write(b));server.stderr.on('data',b=>process.stderr.write(b));
 let ready=false;for(let i=0;i<150;i++){if(serverError)throw serverError;if(server.exitCode!==null)throw Error('测试服务在就绪前退出');try{const r=await fetch(cfg.check.url,{signal:AbortSignal.timeout(1000)});if(r.ok){ready=true;break}}catch{}await sleep(200)}if(!ready)throw Error('测试服务未在预算内就绪');
 browser=await playwright.chromium.launch({headless:true});result.browserVersion=browser.version();const context=await browser.newContext({viewport:{width:1440,height:1000},acceptDownloads:false});
 const origin=new URL(cfg.check.url).origin;await context.route('**/*',route=>{const u=new URL(route.request().url());return u.origin===origin||u.protocol==='data:'?route.continue():route.abort()});page=await context.newPage();page.setDefaultTimeout(8000);
 page.on('console',m=>{if(result.console.length<100)result.console.push({type:m.type(),text:m.text().slice(0,4000)})});page.on('pageerror',e=>{if(result.console.length<100)result.console.push({type:'pageerror',text:String(e).slice(0,4000)})});await page.goto(cfg.check.url,{waitUntil:'domcontentloaded',timeout:30000});
 for(const step of cfg.check.steps){const locator=page.locator(step.selector||'body');switch(step.action){case 'click':await locator.click();break;case 'fill':await locator.fill(step.value);break;case 'check':await locator.check();break;case 'select':await locator.selectOption(step.value);break;case 'visible':await locator.waitFor({state:'visible'});break;case 'text':if(!(await locator.innerText()).includes(step.value))throw Error('文本断言失败: '+step.selector);break;case 'value':if(await locator.inputValue()!==step.value)throw Error('表单值断言失败: '+step.selector);break;case 'url':if(!page.url().includes(step.value))throw Error('URL断言失败');break;default:throw Error('不支持的浏览器步骤')}result.steps.push({...step,passed:true})}
 if(result.console.some(x=>x.type==='pageerror'))throw Error('页面存在未捕获运行错误');result.passed=true;
 }catch(e){result.error=String(e);process.exitCode=1}finally{try{if(page){const image=await page.screenshot({fullPage:false});if(image.length>20*1024*1024)throw Error('截图超过产物预算');fs.writeFileSync(path.join(cfg.artifacts,'screenshot.png'),image)}}catch{}try{if(browser)await browser.close()}catch{}try{if(server)server.kill()}catch{}const report=JSON.stringify(result);if(Buffer.byteLength(report)>256*1024)throw Error('报告超过产物预算');fs.writeFileSync(path.join(cfg.artifacts,'report.json'),report);}})();`

func (s *Server) prepareSupervisionBrowser(rootPath string, check supervisionBrowserCheck, executionID string) (string, []string, string, func(), error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var sessionID string
	if err := s.db.QueryRowContext(ctx, `select session_id from supervision_work_items where execution_id=?`, executionID).Scan(&sessionID); err != nil {
		return "", nil, "", nil, err
	}
	const artifactReserve int64 = 24 << 20
	if err := s.withSupervisionTx(ctx, sessionID, time.Now().UTC(), func(tx *sql.Tx, _ map[string]WorkspaceAdmission, ss *supervisionSession) error {
		if ss.Control != "" || ss.RecoveryState != "ready" {
			return errSupervisionState
		}
		_, err := tx.ExecContext(ctx, `insert into supervision_storage_reservations(execution_id,session_id,bytes) values (?,?,?)`, executionID, sessionID, artifactReserve)
		return err
	}); err != nil {
		return "", nil, "", nil, err
	}
	return prepareSupervisionBrowserFiles(rootPath, check, filepath.Join(s.checkpointStorageRoot(), "supervision", "artifacts", executionID))
}

func prepareSupervisionBrowserFiles(rootPath string, check supervisionBrowserCheck, artifacts string) (string, []string, string, func(), error) {
	if check.ServerProgram == "" {
		return "", nil, "", nil, errors.New("浏览器验证必须启动本次快照的独立测试服务")
	}
	u, err := url.Parse(check.URL)
	if err != nil {
		return "", nil, "", nil, err
	}
	address := u.Host
	if u.Port() == "" {
		port := "80"
		if u.Scheme == "https" {
			port = "443"
		}
		address = net.JoinHostPort(u.Hostname(), port)
	}
	if conn, err := net.DialTimeout("tcp", address, 300*time.Millisecond); err == nil {
		conn.Close()
		return "", nil, "", nil, errors.New("测试端口已被占用，不能把已有服务当作当前代码的验收实例")
	}
	server, args, err := supervisionNativeVerificationCommand(check.ServerProgram, check.ServerArgs)
	if err != nil {
		return "", nil, "", nil, err
	}
	node, _, err := supervisionNativeVerificationCommand("node", nil)
	if err != nil {
		return "", nil, "", nil, err
	}
	if err := os.MkdirAll(artifacts, 0700); err != nil {
		return "", nil, "", nil, err
	}
	driver := filepath.Join(artifacts, "driver.cjs")
	config := filepath.Join(artifacts, "config.json")
	working, _ := os.Getwd()
	bases := []string{rootPath, working, filepath.Join(working, "apps", "web"), filepath.Join(working, "..", "web")}
	for parent := working; ; parent = filepath.Dir(parent) {
		bases = append(bases, parent, filepath.Join(parent, "web"), filepath.Join(parent, "apps", "web"))
		if filepath.Dir(parent) == parent {
			break
		}
	}
	raw, _ := json.Marshal(map[string]any{"check": check, "cwd": rootPath, "server": append([]string{server}, args...), "artifacts": artifacts, "moduleBases": bases})
	if len(raw) > 2<<20 {
		return "", nil, "", nil, errors.New("浏览器驱动配置超过2MiB产物预留限制")
	}
	if err := os.WriteFile(driver, []byte(supervisionBrowserDriver), 0600); err != nil {
		return "", nil, "", nil, err
	}
	if err := os.WriteFile(config, raw, 0600); err != nil {
		return "", nil, "", nil, err
	}
	cleanup := func() { _ = os.Remove(driver); _ = os.Remove(config) }
	return node, []string{driver, config}, filepath.Join(artifacts, "report.json"), cleanup, nil
}
