# 本机直连连续对话与实际工具调用测试

测试日期：2026-11-17（Asia/Shanghai）。

## 结论

当前源码构建的服务在 `deepseek-v4.1-flash-free` 的 Chat Completions 接口上可以完成连续对话、流式与非流式工具调用、工具结果回传及后续回答。共28次推理请求全部HTTP 200，实际执行10次工具（6次read、4次edit）。日志逐项确认 `attempts=1`、`@direct` 以及客户端显式session保持一致，没有429或出口重试。

| 测试组 | 推理请求 | 实际工具执行 | 结果 |
| --- | --- | --- | --- |
| 连续对话，非流式 | 9 | 3 | 全部200，内容与执行结果通过 |
| 连续对话，流式 | 9 | 3 | 全部200，内容与执行结果通过 |
| 自动选择工具，非流式 | 5 | 2 | 全部200，内容与执行结果通过 |
| 自动选择工具，流式 | 5 | 2 | 全部200，内容与执行结果通过 |

## 测试内容

- 两组连续对话分别固定project/session，发送完整历史，包含实际assistant消息、reasoning_content以及role=tool结果。流程为记住随机代号和初始数量、回忆、读取临时文件、编辑文件、再次读取、最终汇总代号/初始数量/文件最终值/编辑回执。
- read工具实际读取随机文件内容；隐藏amount没有出现在首次读取请求的用户文本中。执行器另行生成随机回执，只通过工具结果回传，用于验证模型实际消费了工具结果。
- edit工具实际完成 `amount=385 → amount=392` 和 `amount=153 → amount=160`。随后read检查新内容，最终模型正确关联对话历史与编辑回执。
- 两组自动工具测试全程使用 `tool_choice=auto`，根据自然语言返回read和edit调用；执行器修改 `status=pending → status=done`，回传结果后模型正确回答并继续回忆。
- 客户端声明read/edit工具，使用file_path/old_string/new_string/replace_all参数。read使用官方description和客户端schema，edit使用Harness原description。网关负责现有工具整形及名称/参数还原。执行器断言返回工具名属于客户端声明、参数合法且目标限定为自己的临时文件；全部10次工具执行成功。
- 流式响应按SSE实际合并tool_calls片段，检查终止标记、工具索引、调用ID、工具名称及可解析JSON参数。工具响应finish_reason均为tool_calls；没有出现工具名拼接重复或官方参数名泄漏导致的调用失败。

自动模式流式读取后的一条答案为 `code=AUTO-1072CE48 READ-22b39be14d`，严格格式期望是 `AUTO-1072CE48 READ-22b39be14d`。原始断言因多出的code=前缀停止；文件值和随机回执均正确。保留原响应及偏差记录，按去除该字段前缀后的内容校验通过，在原project/session和原始历史上继续完成编辑及回忆。此项属于回答格式偏差，未发生工具链失败。最终回忆按指定格式精确返回。

## 实例和路由

- 地址：`http://127.0.0.1:8080`，服务测试结束后保持运行，管理UI关闭。
- 使用新编译的临时二进制，包含工作区已有未提交修改。源码HEAD：`67ed3c0690f40503f3094c23e99c3dc8b8ea36de`。
- 从项目现有config.json生成临时副本，关闭WARP、proxies=[direct]、proxyfile为空、关闭主动推理探测；原config.json哈希保持不变。
- 服务启动清除HTTP_PROXY/HTTPS_PROXY/ALL_PROXY及小写同名环境变量；客户端显式ProxyHandler({})，不使用环境代理。
- 无Zen/Go上游Key，使用匿名通道。本地鉴权继续使用原server key，其值未输出到结果或请求证据。
- direct标签确认网关无显式代理；不作为公网IP不变的证明。
- 验证接口：`POST /v1/chat/completions`；本轮没有访问Responses或Anthropic Messages推理接口，也未测试其它模型或其它工具集。
- 连续对话依赖客户端提交完整历史。本轮不证明仅凭session能够自动补回历史。

## 证据与复现

临时证据目录：`/private/tmp/opencode-direct-tools-vou7pd1p/`。

- `live_test.py`：两组连续对话测试；`auto_tools.py`：自动选择工具测试及同会话接续。
- `results.json`：28次请求的HTTP状态、request_id、project/session、实际答案和校验结果。
- `tool-executions.json`：10次真实工具调用、参数、结果和文件前后哈希。
- `*-request.json`：请求体，不含鉴权；`*-response.json` / `*-response.sse`：原始响应；`*-history.json`：实际完整历史。
- `service.log`：服务路由证据；`audit.json`：逐项审计；`build-metadata.json`：源码及原配置哈希。
- 临时 `config.json` 含原本地server key，权限为0600；整个证据目录权限为0700。

Go测试 `go test ./...`、重新构建及结果审计通过。原配置和已有源码diff保持不变；本轮仅新增这份测试报告，没有提交或推送。

首次响应时间：`2026-11-17T00:07:52.756043+08:00`；最后响应时间：`2026-11-17T00:10:46.425256+08:00`。28次推理耗时范围1.064—5.698秒，平均2.438秒。
