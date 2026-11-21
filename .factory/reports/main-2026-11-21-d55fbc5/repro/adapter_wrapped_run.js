// 用法: node adapter_wrapped_run.js <RunScript 输出文件>
// 以 stub fetch 执行 internal/site.RunScript 生成的包装脚本，观察 adapter 是否真的被调用。
const fs = require('fs');
let fetchCalls = 0;
globalThis.fetch = async () => { fetchCalls++; return { ok: true }; };
const wrapped = fs.readFileSync(process.argv[2], 'utf8');
(async () => {
  const result = await eval(wrapped);
  console.log('adapter result =', JSON.stringify(result), '| fetch calls =', fetchCalls);
  if (!result || result.ok !== true) {
    console.log('FAIL: expected {"ok":true} from the adapter; wrapper never invoked it');
    process.exit(1);
  }
  console.log('PASS');
})();
